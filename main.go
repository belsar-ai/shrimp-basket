package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// --- CONFIGURATION & GLOBALS ---
const (
	defaultPort    = "12345"
	quarantineDays = 7
)

var (
	cacheDir    string
	normPattern = regexp.MustCompile(`[-_.]+`)
	pypiClient  = &http.Client{Timeout: 15 * time.Second}
	npmClient   = &http.Client{Timeout: 15 * time.Second}

	// Input Validation Regexes
	pypiNameRegex = regexp.MustCompile(`^[A-Za-z0-9._-]{1,214}$`)
	npmNameRegex  = regexp.MustCompile(`^(@[A-Za-z0-9-_.]+/)?[A-Za-z0-9-_.]+$`)
)

// --- STRUCTS FOR PYPI & NPM PARSING ---

// PyPIJSONResponse represents the release info returned by PyPI JSON API
type PyPIJSONResponse struct {
	Releases map[string][]struct {
		Filename      string    `json:"filename"`
		UploadTimeIso time.Time `json:"upload_time_iso_8601"`
	} `json:"releases"`
}

// PEP691File represents file objects in the simple JSON response
type PEP691File struct {
	Filename       string            `json:"filename"`
	URL            string            `json:"url"`
	Hashes         map[string]string `json:"hashes"`
	RequiresPython string            `json:"requires-python,omitempty"`
	Yanked         interface{}       `json:"yanked,omitempty"`
	CoreMetadata   interface{}       `json:"core-metadata,omitempty"`
}

// --- UTILITY FUNCTIONS ---

func getCacheDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.Getenv("HOME")
	}
	return filepath.Join(home, ".cache", "shrimp-basket")
}

// normalizePEP503 normalizes a package name according to PEP 503
func normalizePEP503(name string) string {
	return strings.ToLower(normPattern.ReplaceAllString(name, "-"))
}

// fetchUpstream executes an HTTP request against an upstream registry.
// No retry layer: every package manager (pip, npm, uv, yarn, pnpm) already
// retries on its own, and stacking retries multiplies upstream load on outage.
func fetchUpstream(client *http.Client, req *http.Request) (*http.Response, error) {
	req.Header.Set("User-Agent", "shrimp-basket/1.0.0")
	return client.Do(req)
}

// --- CORE FILTERING LOGIC ---

// filterPyPIIndex filters a PEP 691 JSON simple index, preserving all unknown fields (PEP 740, etc.)
func filterPyPIIndex(pkg string, data []byte) ([]byte, error) {
	var rawIndex map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawIndex); err != nil {
		return nil, err
	}

	normPkg := normalizePEP503(pkg)

	// TODO: PEP 700 introduces upload-time directly in the simple JSON.
	// Once PyPI rolls this out completely, we can read it from rawIndex["files"]
	// and skip this second upstream fetch to /json.
	// Fetch publish dates from PyPI JSON API
	req, _ := http.NewRequest("GET", fmt.Sprintf("https://pypi.org/pypi/%s/json", normPkg), nil)
	resp, err := fetchUpstream(pypiClient, req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch release dates (failing closed): %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("package metadata JSON not found on PyPI (failing closed)")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status from PyPI JSON API: %d (failing closed)", resp.StatusCode)
	}

	var jsonMeta PyPIJSONResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 30*1024*1024)).Decode(&jsonMeta); err != nil {
		return nil, fmt.Errorf("failed to decode PyPI JSON metadata: %w (failing closed)", err)
	}

	// Build map of filename -> version, and version -> upload safety
	cutoff := time.Now().AddDate(0, 0, -quarantineDays)
	fileToVersion := make(map[string]string)
	versionSafety := make(map[string]bool)
	
	// A version is safe only if ALL its uploaded files are safe (version-level filtering).
	for version, uploads := range jsonMeta.Releases {
		versionSafe := true
		if len(uploads) == 0 {
			versionSafe = false
		}
		for _, upload := range uploads {
			fileToVersion[upload.Filename] = version
			if upload.UploadTimeIso.After(cutoff) {
				versionSafe = false
			}
		}
		versionSafety[version] = versionSafe
	}

	// Filter files list
	var files []json.RawMessage
	if filesRaw, ok := rawIndex["files"]; ok {
		if err := json.Unmarshal(filesRaw, &files); err != nil {
			return nil, err
		}
	}

	var filteredFiles []json.RawMessage
	var remainingVersions []string
	seenVersions := make(map[string]bool)

	for _, fileRaw := range files {
		var file PEP691File
		if err := json.Unmarshal(fileRaw, &file); err != nil {
			continue
		}

		// Map file to its version directly from JSON metadata (no regex parsing needed)
		version, exists := fileToVersion[file.Filename]
		if !exists {
			// Fail-closed for unknown files (not listed in metadata)
			log.Printf("[QUARANTINE] Blocked file %s (not listed in releases metadata - failing closed)", file.Filename)
			continue
		}

		safe, vExists := versionSafety[version]
		if !vExists || !safe {
			log.Printf("[QUARANTINE] Blocked file %s (version %s is quarantined)", file.Filename, version)
			continue
		}

		filteredFiles = append(filteredFiles, fileRaw)
		if !seenVersions[version] {
			seenVersions[version] = true
			remainingVersions = append(remainingVersions, version)
		}
	}

	// Update files array
	newFilesRaw, err := json.Marshal(filteredFiles)
	if err != nil {
		return nil, err
	}
	rawIndex["files"] = newFilesRaw

	// Filter PEP 700 top-level versions array in lockstep (if present)
	if _, ok := rawIndex["versions"]; ok {
		newVersionsRaw, err := json.Marshal(remainingVersions)
		if err != nil {
			return nil, err
		}
		rawIndex["versions"] = newVersionsRaw
	}

	return json.Marshal(rawIndex)
}

// filterNPMIndex filters an NPM registry index, preserving all unknown fields
func filterNPMIndex(pkg string, data []byte) ([]byte, error) {
	var rawIndex map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawIndex); err != nil {
		return nil, err
	}

	// Parse versions and time
	var versions map[string]json.RawMessage
	if versionsRaw, ok := rawIndex["versions"]; ok {
		if err := json.Unmarshal(versionsRaw, &versions); err != nil {
			return nil, err
		}
	} else {
		return nil, fmt.Errorf("missing versions in npm registry JSON")
	}

	var timeMap map[string]string
	if timeRaw, ok := rawIndex["time"]; ok {
		if err := json.Unmarshal(timeRaw, &timeMap); err != nil {
			return nil, err
		}
	} else {
		return nil, fmt.Errorf("missing time in npm registry JSON")
	}

	cutoff := time.Now().AddDate(0, 0, -quarantineDays)
	isSafe := make(map[string]bool)

	// Evaluate timestamps
	for version, timeStr := range timeMap {
		if version == "created" || version == "modified" {
			continue
		}
		t, err := time.Parse(time.RFC3339Nano, timeStr)
		if err != nil {
			// Fail closed on unparseable publish time
			isSafe[version] = false
			continue
		}
		if t.After(cutoff) {
			isSafe[version] = false
			log.Printf("[QUARANTINE] NPM blocked %s version %s (published within 7 days)", pkg, version)
		} else {
			isSafe[version] = true
		}
	}

	// Filter versions map
	filteredVersions := make(map[string]json.RawMessage)
	for version, details := range versions {
		if safe, exists := isSafe[version]; exists && safe {
			filteredVersions[version] = details
		}
	}

	// Filter time map
	filteredTime := make(map[string]string)
	if created, ok := timeMap["created"]; ok {
		filteredTime["created"] = created
	}
	if modified, ok := timeMap["modified"]; ok {
		filteredTime["modified"] = modified
	}
	for version, timeStr := range timeMap {
		if version == "created" || version == "modified" {
			continue
		}
		if safe, exists := isSafe[version]; exists && safe {
			filteredTime[version] = timeStr
		}
	}

	// Re-serialize maps and update top-level JSON fields
	newVersionsRaw, err := json.Marshal(filteredVersions)
	if err != nil {
		return nil, err
	}
	rawIndex["versions"] = newVersionsRaw

	newTimeRaw, err := json.Marshal(filteredTime)
	if err != nil {
		return nil, err
	}
	rawIndex["time"] = newTimeRaw

	// Note: We leave the "dist-tags" (e.g. latest) pointing to blocked versions unmodified.
	// This results in the correct, loud client-side failure mode (no matching version found)
	// rather than silently downgrading the user to an older version.

	return json.Marshal(rawIndex)
}

// --- HTTP SERVER HANDLERS ---

func handlePyPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	pkg := strings.TrimPrefix(r.URL.Path, "/simple/")
	pkg = strings.TrimSuffix(pkg, "/")

	if pkg == "" {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	// Input Validation (Fail Closed)
	if !pypiNameRegex.MatchString(pkg) {
		log.Printf("[SECURITY] Blocked invalid PyPI package name: %s", pkg)
		http.Error(w, "Invalid Package Name", http.StatusBadRequest)
		return
	}

	normPkg := normalizePEP503(pkg)
	cacheFile := filepath.Join(cacheDir, "pypi", url.QueryEscape(normPkg)+".json")
	
	// Check Cache
	if data, err := readCache(cacheFile); err == nil {
		servePyPICached(w, r, data)
		return
	}

	log.Printf("[FETCH] PyPI upstream index for: %s", normPkg)
	
	// Fetch from PyPI simple JSON endpoint
	req, _ := http.NewRequest("GET", fmt.Sprintf("https://pypi.org/simple/%s/", normPkg), nil)
	req.Header.Set("Accept", "application/vnd.pypi.simple.v1+json")

	resp, err := fetchUpstream(pypiClient, req)
	if err != nil {
		log.Printf("Upstream PyPI request error: %v", err)
		http.Error(w, "Gateway Error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if resp.StatusCode != http.StatusOK {
		http.Error(w, "Upstream Error", http.StatusBadGateway)
		return
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 30*1024*1024))
	if err != nil {
		http.Error(w, "Read Error", http.StatusInternalServerError)
		return
	}

	// Filter and fail closed if filtering fails
	filteredBody, err := filterPyPIIndex(normPkg, body)
	if err != nil {
		log.Printf("Filtering error (failing closed): %v", err)
		http.Error(w, "Gateway Quarantine Error", http.StatusBadGateway)
		return
	}

	// Write Cache Atomically
	writeCache(cacheFile, filteredBody)

	servePyPICached(w, r, filteredBody)
}

func servePyPICached(w http.ResponseWriter, r *http.Request, data []byte) {
	accept := r.Header.Get("Accept")
	if accept != "" && !strings.Contains(accept, "application/vnd.pypi.simple.v1+json") && !strings.Contains(accept, "*/*") {
		http.Error(w, "Only application/vnd.pypi.simple.v1+json is supported", http.StatusNotAcceptable)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.pypi.simple.v1+json")
	if r.Method == http.MethodHead {
		return
	}
	w.Write(data)
}

func handleNPM(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}

	pkg := strings.ToLower(strings.TrimPrefix(r.URL.Path, "/"))
	if pkg == "" || pkg == "favicon.ico" || pkg == "robots.txt" {
		w.WriteHeader(http.StatusNotFound)
		return
	}

	// Route 1: Intercept NPM special endpoints and tarball downloads
	if strings.Contains(r.URL.Path, "/-/") {
		redirectURL := "https://registry.npmjs.org" + r.URL.RequestURI()
		w.Header().Set("Location", redirectURL)
		w.WriteHeader(http.StatusFound)
		return
	}

	// Input Validation (Fail Closed)
	if !npmNameRegex.MatchString(pkg) {
		// Suppress logging security alerts for common harmless static assets/file probes
		if !strings.HasSuffix(pkg, ".js") && !strings.HasSuffix(pkg, ".png") && !strings.HasSuffix(pkg, ".css") && !strings.HasSuffix(pkg, ".ico") {
			log.Printf("[SECURITY] Blocked invalid NPM package name: %s", pkg)
		}
		http.Error(w, "Invalid Package Name", http.StatusBadRequest)
		return
	}

	cacheFile := filepath.Join(cacheDir, "npm", url.QueryEscape(pkg)+".json")

	// Check Cache
	if data, err := readCache(cacheFile); err == nil {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodHead {
			return
		}
		w.Write(data)
		return
	}

	log.Printf("[FETCH] NPM upstream index for: %s", pkg)

	// Use canonical escaped form for scoped packages (e.g. @scope%2Fpkg)
	upstreamPath := pkg
	if strings.Contains(pkg, "/") {
		parts := strings.SplitN(pkg, "/", 2)
		upstreamPath = parts[0] + "%2F" + parts[1]
	}
	req, _ := http.NewRequest("GET", "https://registry.npmjs.org/"+upstreamPath, nil)

	resp, err := fetchUpstream(npmClient, req)
	if err != nil {
		log.Printf("Upstream NPM request error: %v", err)
		http.Error(w, "Gateway Error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if resp.StatusCode != http.StatusOK {
		http.Error(w, "Upstream NPM Error", http.StatusBadGateway)
		return
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 30*1024*1024))
	if err != nil {
		http.Error(w, "Read Error", http.StatusInternalServerError)
		return
	}

	filteredBody, err := filterNPMIndex(pkg, body)
	if err != nil {
		log.Printf("NPM Filtering error (failing closed): %v", err)
		http.Error(w, "Gateway Quarantine Error", http.StatusBadGateway)
		return
	}

	writeCache(cacheFile, filteredBody)

	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodHead {
		return
	}
	w.Write(filteredBody)
}

// --- CACHING OPERATIONS ---

func readCache(cacheFile string) ([]byte, error) {
	return os.ReadFile(cacheFile)
}

func writeCache(cacheFile string, data []byte) {
	// Validate JSON payload before caching to prevent corruption poisoning
	if !json.Valid(data) {
		log.Printf("Failed to write cache: generated bytes are invalid JSON")
		return
	}

	os.MkdirAll(filepath.Dir(cacheFile), 0755)
	
	// Atomic Cache Write using temp file + Rename. Saves cache files with 0600 permissions.
	tmpFile := cacheFile + ".tmp"
	if err := os.WriteFile(tmpFile, data, 0600); err != nil {
		log.Printf("Failed to write temporary cache: %v", err)
		return
	}
	if err := os.Rename(tmpFile, cacheFile); err != nil {
		log.Printf("Failed to commit cache atomically: %v", err)
		os.Remove(tmpFile)
	}
}

// --- DAILY UPDATE ROUTINE ---

func runDailyUpdate() {
	cacheDir = getCacheDir()
	log.Printf("Starting daily metadata cache update in: %s", cacheDir)

	// Update PyPI Cache
	pypiFiles, _ := filepath.Glob(filepath.Join(cacheDir, "pypi", "*.json"))
	for _, file := range pypiFiles {
		escapedPkg := strings.TrimSuffix(filepath.Base(file), ".json")
		pkg, err := url.QueryUnescape(escapedPkg)
		if err == nil {
			updatePackageCache("pypi", pkg)
			time.Sleep(50 * time.Millisecond) // Fast but rate-limited sleep
		}
	}

	// Update NPM Cache
	npmFiles, _ := filepath.Glob(filepath.Join(cacheDir, "npm", "*.json"))
	for _, file := range npmFiles {
		escapedPkg := strings.TrimSuffix(filepath.Base(file), ".json")
		pkg, err := url.QueryUnescape(escapedPkg)
		if err == nil {
			updatePackageCache("npm", pkg)
			time.Sleep(50 * time.Millisecond) // Fast but rate-limited sleep
		}
	}
	log.Printf("Daily update complete.")
}

func updatePackageCache(registryType, pkg string) {
	log.Printf("[UPDATE] Fetching latest metadata for: %s (%s)", pkg, registryType)
	var targetUrl string
	var filterFunc func(string, []byte) ([]byte, error)
	var cacheFile string

	if registryType == "pypi" {
		normPkg := normalizePEP503(pkg)
		targetUrl = fmt.Sprintf("https://pypi.org/simple/%s/", normPkg)
		filterFunc = filterPyPIIndex
		cacheFile = filepath.Join(cacheDir, "pypi", url.QueryEscape(normPkg)+".json")
	} else {
		upstreamPath := pkg
		if strings.Contains(pkg, "/") {
			parts := strings.SplitN(pkg, "/", 2)
			upstreamPath = parts[0] + "%2F" + parts[1]
		}
		targetUrl = "https://registry.npmjs.org/" + upstreamPath
		filterFunc = filterNPMIndex
		cacheFile = filepath.Join(cacheDir, "npm", url.QueryEscape(pkg)+".json")
	}

	req, _ := http.NewRequest("GET", targetUrl, nil)
	if registryType == "pypi" {
		req.Header.Set("Accept", "application/vnd.pypi.simple.v1+json")
	}

	var resp *http.Response
	var err error
	if registryType == "pypi" {
		resp, err = fetchUpstream(pypiClient, req)
	} else {
		resp, err = fetchUpstream(npmClient, req)
	}

	if err != nil {
		log.Printf("Update request failed for %s: %v", pkg, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("Update response error for %s: %d", pkg, resp.StatusCode)
		return
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 30*1024*1024))
	if err != nil {
		log.Printf("[UPDATE] Failed to read response body for %s: %v", pkg, err)
		return
	}

	filteredBody, err := filterFunc(pkg, body)
	if err != nil {
		log.Printf("Update filter failed for %s: %v (failing closed)", pkg, err)
		return
	}

	writeCache(cacheFile, filteredBody)
}

// --- MAIN RUNTIME ---

func main() {
	updateFlag := flag.Bool("update", false, "Run daily update script and exit")
	flag.Parse()

	// Initialize cache dir location globally
	cacheDir = getCacheDir()

	if *updateFlag {
		runDailyUpdate()
		return
	}

	log.Printf("[START] Starting shrimp-basket quarantine proxy...")
	log.Printf("[START] Cache Directory: %s", cacheDir)

	listener, err := net.Listen("tcp", "127.0.0.1:"+defaultPort)
	if err != nil {
		log.Fatalf("Failed to listen on 127.0.0.1:%s: %v", defaultPort, err)
	}
	log.Printf("[START] Listening on 127.0.0.1:%s", defaultPort)

	mux := http.NewServeMux()
	mux.HandleFunc("/simple/", handlePyPI)
	mux.HandleFunc("/", handleNPM)

	server := &http.Server{
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
		log.Fatalf("Server error: %v", err)
	}
}
