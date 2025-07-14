package main

import (
	"context"
	"encoding/csv"
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

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

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
func loadRedirectsFromS3(s3Path string) (map[string][]Redirect, int, error) {
	pathParts := strings.SplitN(strings.TrimPrefix(s3Path, "s3://"), "/", 2)
	bucket := pathParts[0]
	prefix := ""
	if len(pathParts) > 1 {
		prefix = pathParts[1]
	}

	cfg, err := config.LoadDefaultConfig(context.TODO())
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
func loadRules() error {
	csvSrc := os.Getenv("SWERVE_CSV_SRC")
	if csvSrc == "" {
		csvSrc = "/app/redirects" // Default to local directory
	}

	var tempRedirects map[string][]Redirect
	var totalRules int
	var err error

	if strings.HasPrefix(csvSrc, "s3://") {
		log.Printf("Loading redirects from S3 source: %s", csvSrc)
		tempRedirects, totalRules, err = loadRedirectsFromS3(csvSrc)
	} else {
		log.Printf("Loading redirects from local directory: %s", csvSrc)
		tempRedirects, totalRules, err = loadRedirectsFromDir(csvSrc)
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

	for _, rule := range rules {
		targetURL := ""
		if rule.MatchType == "exact" && rule.SourcePathOrRegex == path {
			targetURL = rule.TargetURLFormat
		} else if rule.MatchType == "regex" && rule.Regex != nil && rule.Regex.MatchString(path) {
			rewrittenURL := strings.ReplaceAll(rule.TargetURLFormat, "$path", path)
			targetURL = rule.Regex.ReplaceAllString(path, rewrittenURL)
		}

		if targetURL != "" {
			log.Printf("Redirecting: %s%s -> %s (Rule: %s, Weight: %d)", host, path, targetURL, rule.SourcePathOrRegex, rule.Weight)
			http.Redirect(w, r, targetURL, rule.StatusCode)
			return
		}
	}

	log.Printf("No match found for: %s%s", host, path)
	http.NotFound(w, r)
}

func main() {
	if err := loadRules(); err != nil {
		log.Fatalf("FATAL: Failed to load redirect rules: %v", err)
	}

	http.HandleFunc("/", redirectHandler)

	log.Println("Starting Swerve redirection service on :8080...")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Fatalf("Could not start server: %s\n", err)
	}
}
