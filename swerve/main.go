package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/jessevdk/go-flags"
)

// Options holds the application's configuration.
type Options struct {
	CsvSrc             string        `long:"csv-src" env:"SWERVE_CSV_SRC" description:"Source for the CSV redirect files. Can be a local path or an S3 URI." default:"/app/redirects"`
	PollInterval       time.Duration `long:"poll-interval" env:"SWERVE_POLL_INTERVAL" description:"Interval to poll for rule changes (e.g., 5m, 1h). Set to 0 to disable." default:"0"`
	HealthCheckDomain  string        `long:"health-check-domain" env:"SWERVE_HEALTH_CHECK_DOMAIN" description:"The domain on which to expose the health check endpoint. If empty, it responds on all domains."`
	HealthCheckPath    string        `long:"health-check-path" env:"SWERVE_HEALTH_CHECK_PATH" description:"The path for the health check endpoint (e.g., /healthz). If not set, the endpoint is disabled."`
	AWSRegion          string        `long:"aws-region" env:"AWS_REGION" description:"The AWS region for the S3 bucket."`
	AWSAccessKeyID     string        `long:"aws-access-key-id" env:"AWS_ACCESS_KEY_ID" description:"AWS access key. If not set, IAM role is assumed."`
	AWSSecretAccessKey string        `long:"aws-secret-access-key" env:"AWS_SECRET_ACCESS_KEY" description:"AWS secret key."`
	AWSSessionToken    string        `long:"aws-session-token" env:"AWS_SESSION_TOKEN" description:"AWS session token."`
}

// Redirect represents a single, compiled redirect rule.
type Redirect struct {
	SourceHost        string
	MatchType         string // "exact" or "regex"
	SourcePathOrRegex string
	TargetURLFormat   string
	StatusCode        int
	Weight            int
	Regex             *regexp.Regexp // Holds the compiled regular expression
}

// HealthStatus represents the JSON response for the health check.
type HealthStatus struct {
	Status    string `json:"status"`
	Domains   int    `json:"domains"`
	Redirects int    `json:"redirects"`
}

// RedirectLogEntry represents the structured log for a successful redirect.
type RedirectLogEntry struct {
	Timestamp string `json:"timestamp"`
	Host      string `json:"host"`
	Path      string `json:"path"`
	TargetURL string `json:"target_url"`
	Rule      string `json:"rule"`
	Weight    int    `json:"weight"`
	Type      string `json:"type"` // To distinguish this log type
}

// redirectMap stores a slice of redirect rules for each host, sorted by weight.
var (
	redirectMap = make(map[string][]Redirect)
	mapMutex    = &sync.RWMutex{}
)

// parseRules reads CSV data from an io.Reader and converts it into a map of Redirect rules.
func parseRules(csvData io.Reader, sourceName string) (map[string][]Redirect, int, error) {
	reader := csv.NewReader(csvData)
	reader.FieldsPerRecord = -1 // Allow variable number of fields
	records, err := reader.ReadAll()
	if err != nil {
		return nil, 0, fmt.Errorf("could not parse CSV from %s: %w", sourceName, err)
	}

	rulesByHost := make(map[string][]Redirect)
	rulesCount := 0
	for i, record := range records[1:] { // Skip header row
		if len(record) == 0 || (len(record) == 1 && record[0] == "") || strings.HasPrefix(strings.TrimSpace(record[0]), "#") {
			continue
		}
		if len(record) != 6 {
			log.Printf("File %s, Line %d: Skipping invalid record (must have 6 columns): %v", sourceName, i+2, record)
			continue
		}

		host, matchType, pathOrRegex := strings.TrimSpace(record[0]), strings.TrimSpace(record[1]), strings.TrimSpace(record[2])
		statusCode, _ := strconv.Atoi(record[4])
		weight, _ := strconv.Atoi(record[5])

		rule := Redirect{
			SourceHost:        host,
			MatchType:         matchType,
			SourcePathOrRegex: pathOrRegex,
			TargetURLFormat:   strings.TrimSpace(record[3]),
			StatusCode:        statusCode,
			Weight:            weight,
		}

		if matchType == "regex" {
			rule.Regex, err = regexp.Compile(pathOrRegex)
			if err != nil {
				log.Printf("File %s, Line %d: Invalid regex '%s', skipping rule. Error: %v", sourceName, i+2, pathOrRegex, err)
				continue
			}
		}
		rulesByHost[host] = append(rulesByHost[host], rule)
		rulesCount++
	}
	return rulesByHost, rulesCount, nil
}

// loadRedirectsFromDir loads all .csv files from a local directory.
func loadRedirectsFromDir(dirPath string) (map[string][]Redirect, int, error) {
	aggregatedRules := make(map[string][]Redirect)
	totalRules := 0

	err := filepath.WalkDir(dirPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(strings.ToLower(d.Name()), ".csv") {
			log.Printf("Processing file: %s", path)
			file, err := os.Open(path)
			if err != nil {
				log.Printf("WARNING: Could not open file %s: %v", path, err)
				return nil
			}
			defer file.Close()

			rules, count, err := parseRules(file, path)
			if err != nil {
				log.Printf("WARNING: %v", err)
				return nil
			}
			for host, hostRules := range rules {
				aggregatedRules[host] = append(aggregatedRules[host], hostRules...)
			}
			totalRules += count
		}
		return nil
	})

	if err != nil {
		return nil, 0, err
	}
	return aggregatedRules, totalRules, nil
}

// loadRedirectsFromS3 loads all .csv files from an S3 bucket/prefix.
func loadRedirectsFromS3(s3Path string, opts Options) (map[string][]Redirect, int, error) {
	pathParts := strings.SplitN(strings.TrimPrefix(s3Path, "s3://"), "/", 2)
	bucket := pathParts[0]
	prefix := ""
	if len(pathParts) > 1 {
		prefix = pathParts[1]
	}

	var cfgOptions []func(*config.LoadOptions) error
	if opts.AWSAccessKeyID != "" && opts.AWSSecretAccessKey != "" {
		log.Println("Using static AWS credentials.")
		creds := credentials.NewStaticCredentialsProvider(opts.AWSAccessKeyID, opts.AWSSecretAccessKey, opts.AWSSessionToken)
		cfgOptions = append(cfgOptions, config.WithCredentialsProvider(creds))
	} else {
		log.Println("Using default AWS credential chain (e.g., IAM role).")
	}

	if opts.AWSRegion != "" {
		cfgOptions = append(cfgOptions, config.WithRegion(opts.AWSRegion))
	}

	cfg, err := config.LoadDefaultConfig(context.TODO(), cfgOptions...)
	if err != nil {
		return nil, 0, fmt.Errorf("failed to load AWS configuration: %w", err)
	}

	client := s3.NewFromConfig(cfg)
	paginator := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket),
		Prefix: aws.String(prefix),
	})

	aggregatedRules := make(map[string][]Redirect)
	totalRules := 0

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(context.TODO())
		if err != nil {
			return nil, 0, fmt.Errorf("failed to list objects in S3 bucket %s: %w", bucket, err)
		}
		for _, obj := range page.Contents {
			if !strings.HasSuffix(strings.ToLower(*obj.Key), ".csv") {
				continue
			}
			log.Printf("Processing S3 object: s3://%s/%s", bucket, *obj.Key)
			resp, err := client.GetObject(context.TODO(), &s3.GetObjectInput{Bucket: aws.String(bucket), Key: obj.Key})
			if err != nil {
				log.Printf("WARNING: Could not get S3 object %s: %v", *obj.Key, err)
				continue
			}

			rules, count, err := parseRules(resp.Body, *obj.Key)
			resp.Body.Close()
			if err != nil {
				log.Printf("WARNING: %v", err)
				continue
			}
			for host, hostRules := range rules {
				aggregatedRules[host] = append(aggregatedRules[host], hostRules...)
			}
			totalRules += count
		}
	}
	return aggregatedRules, totalRules, nil
}

// loadRules orchestrates loading rules from the configured source.
func loadRules(opts Options) error {
	var tempRedirects map[string][]Redirect
	var totalRules int
	var err error

	if strings.HasPrefix(opts.CsvSrc, "s3://") {
		log.Printf("Loading redirects from S3 source: %s", opts.CsvSrc)
		tempRedirects, totalRules, err = loadRedirectsFromS3(opts.CsvSrc, opts)
	} else {
		log.Printf("Loading redirects from local directory: %s", opts.CsvSrc)
		tempRedirects, totalRules, err = loadRedirectsFromDir(opts.CsvSrc)
	}

	if err != nil {
		return err
	}

	for host := range tempRedirects {
		sort.Slice(tempRedirects[host], func(i, j int) bool {
			return tempRedirects[host][i].Weight > tempRedirects[host][j].Weight
		})
	}

	mapMutex.Lock()
	redirectMap = tempRedirects
	mapMutex.Unlock()

	log.Printf("Successfully loaded %d redirect rules across %d domains.", totalRules, len(redirectMap))
	return nil
}

// redirectHandler finds the highest-weighted matching rule and performs the redirect.
func redirectHandler(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	if i := strings.LastIndex(host, ":"); i != -1 {
		host = host[:i]
	}
	path := r.URL.Path

	mapMutex.RLock()
	rules, hostExists := redirectMap[host]
	mapMutex.RUnlock()

	if !hostExists {
		http.NotFound(w, r)
		return
	}

	normalizedPath := path
	if len(path) > 1 && strings.HasSuffix(path, "/") {
		normalizedPath = strings.TrimRight(path, "/")
	}

	for _, rule := range rules {
		targetURL := ""

		if rule.MatchType == "exact" {
			normalizedRulePath := rule.SourcePathOrRegex
			if len(normalizedRulePath) > 1 && strings.HasSuffix(normalizedRulePath, "/") {
				normalizedRulePath = strings.TrimRight(normalizedRulePath, "/")
			}
			if normalizedRulePath == normalizedPath {
				targetURL = rule.TargetURLFormat
			}
		} else if rule.MatchType == "regex" && rule.Regex != nil && rule.Regex.MatchString(path) {
			rewrittenURL := strings.ReplaceAll(rule.TargetURLFormat, "$path", path)
			targetURL = rule.Regex.ReplaceAllString(path, rewrittenURL)
		}

		if targetURL != "" {
			// *** UPDATED: Log successful redirects as structured JSON ***
			logEntry := RedirectLogEntry{
				Timestamp: time.Now().UTC().Format(time.RFC3339),
				Host:      host,
				Path:      path,
				TargetURL: targetURL,
				Rule:      rule.SourcePathOrRegex,
				Weight:    rule.Weight,
				Type:      "redirect_hit",
			}
			logJSON, err := json.Marshal(logEntry)
			if err != nil {
				// Fallback to old logging format if JSON marshaling fails
				log.Printf("ERROR marshaling log entry: %v", err)
				log.Printf("Redirecting: %s%s -> %s (Rule: %s, Weight: %d)", host, path, targetURL, rule.SourcePathOrRegex, rule.Weight)
			} else {
				log.Println(string(logJSON))
			}

			http.Redirect(w, r, targetURL, rule.StatusCode)
			return
		}
	}

	log.Printf("No match found for: %s%s", host, path)
	http.NotFound(w, r)
}

// healthCheckHandler serves the health check endpoint.
func healthCheckHandler(w http.ResponseWriter, r *http.Request) {
	mapMutex.RLock()
	domainCount := len(redirectMap)
	redirectCount := 0
	for _, rules := range redirectMap {
		redirectCount += len(rules)
	}
	mapMutex.RUnlock()

	status := HealthStatus{
		Status:    "ok",
		Domains:   domainCount,
		Redirects: redirectCount,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(status)
}

func main() {
	var opts Options
	parser := flags.NewParser(&opts, flags.Default)
	if _, err := parser.Parse(); err != nil {
		if flagsErr, ok := err.(*flags.Error); ok && flagsErr.Type == flags.ErrHelp {
			os.Exit(0)
		} else {
			os.Exit(1)
		}
	}

	if err := loadRules(opts); err != nil {
		log.Fatalf("FATAL: Failed to perform initial load of redirect rules: %v", err)
	}

	if opts.PollInterval > 0 {
		go func() {
			ticker := time.NewTicker(opts.PollInterval)
			for range ticker.C {
				log.Println("Polling for rule updates...")
				if err := loadRules(opts); err != nil {
					log.Printf("ERROR: Failed to reload rules: %v", err)
				}
			}
		}()
	}

	// Create a single handler that routes requests.
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Check if the request is for the health check endpoint.
		if opts.HealthCheckPath != "" && r.URL.Path == opts.HealthCheckPath {
			// If a health check domain is specified, it must match.
			if opts.HealthCheckDomain != "" {
				host := r.Host
				if i := strings.LastIndex(host, ":"); i != -1 {
					host = host[:i]
				}
				if host == opts.HealthCheckDomain {
					healthCheckHandler(w, r)
					return
				}
			} else {
				// If no domain is specified, serve on any domain.
				healthCheckHandler(w, r)
				return
			}
		}

		// Otherwise, use the redirect handler.
		redirectHandler(w, r)
	})

	log.Println("Starting Swerve redirection service on :8080...")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Fatalf("Could not start server: %s\n", err)
	}
}
