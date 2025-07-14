package main

import (
	"encoding/csv"
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

// loadRedirectsFromDir reads all .csv files from a directory, compiles the rules,
// sorts them by weight, and populates the in-memory map.
func loadRedirectsFromDir(dirPath string) error {
	log.Printf("Loading redirect rules from directory: %s", dirPath)
	// A temporary map to hold all rules from all files before sorting
	tempRedirects := make(map[string][]Redirect)
	totalRules := 0

	// Walk the directory to find all .csv files
	err := filepath.WalkDir(dirPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(strings.ToLower(d.Name()), ".csv") {
			log.Printf("Processing file: %s", path)
			// Open and parse the individual CSV file
			file, err := os.Open(path)
			if err != nil {
				log.Printf("WARNING: Could not open file %s: %v", path, err)
				return nil // Continue with other files
			}
			defer file.Close()

			reader := csv.NewReader(file)
			reader.FieldsPerRecord = -1 // Allow variable number of fields
			records, err := reader.ReadAll()
			if err != nil {
				log.Printf("WARNING: Could not parse CSV file %s: %v", path, err)
				return nil // Continue with other files
			}

			// Process each record in the file
			for i, record := range records[1:] { // Skip header row
				// Ignore empty or commented lines
				if len(record) == 0 || (len(record) == 1 && record[0] == "") || strings.HasPrefix(strings.TrimSpace(record[0]), "#") {
					continue
				}

				if len(record) != 6 {
					log.Printf("File %s, Line %d: Skipping invalid record (must have 6 columns): %v", d.Name(), i+2, record)
					continue
				}

				host := strings.TrimSpace(record[0])
				matchType := strings.TrimSpace(record[1])
				pathOrRegex := strings.TrimSpace(record[2])

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
						log.Printf("File %s, Line %d: Invalid regex '%s', skipping rule. Error: %v", d.Name(), i+2, pathOrRegex, err)
						continue
					}
				}
				tempRedirects[host] = append(tempRedirects[host], rule)
				totalRules++
			}
		}
		return nil
	})

	if err != nil {
		return err
	}

	// Sort the rules for each host by weight (descending)
	for host := range tempRedirects {
		sort.Slice(tempRedirects[host], func(i, j int) bool {
			return tempRedirects[host][i].Weight > tempRedirects[host][j].Weight
		})
	}

	// Safely swap the old map with the new one
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
		log.Printf("No host found for: %s", host)
		http.NotFound(w, r)
		return
	}

	// Iterate through the rules (already sorted by weight)
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
	// The directory where all .csv redirect files are located.
	// This path corresponds to the volume mount in docker-compose.yml.
	redirectsDir := "/app/redirects"

	if err := loadRedirectsFromDir(redirectsDir); err != nil {
		log.Fatalf("Failed to perform initial load of redirects: %v", err)
	}

	http.HandleFunc("/", redirectHandler)

	log.Println("Starting advanced redirect server on :8080...")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Fatalf("Could not start server: %s\n", err)
	}
}
