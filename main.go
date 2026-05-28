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
	defaultPort     = "12345"
	quarantineDays  = 7
	maxResponseSize = 128 * 1024 * 1024
)

var (
	normPattern   = regexp.MustCompile(`[-_.]+`)
	pypiNameRegex = regexp.MustCompile(`^[A-Za-z0-9._-]{1,214}$`)
	npmNameRegex  = regexp.MustCompile(`^(@[A-Za-z0-9-_.]+/)?[A-Za-z0-9-_.]+$`)
)

// Proxy bundles the shared dependencies (HTTP client, cache directory) used
// by the registry handlers and the daily update routine. Tests construct
// their own Proxy with a mock transport instead of mutating package globals.
type Proxy struct {
	httpClient     *http.Client
	cacheDir       string
	npmBypassList  map[string]bool
	pypiBypassList map[string]bool
}

func (p *Proxy) isNPMBypassed(pkg string) bool {
	if p.npmBypassList[strings.ToLower(pkg)] {
		return true
	}
	fileNpm, _, err := loadExceptions()
	if err != nil {
		return false
	}
	return fileNpm[strings.ToLower(pkg)]
}

func (p *Proxy) isPyPIBypassed(pkg string) bool {
	if p.pypiBypassList[normalizePEP503(pkg)] {
		return true
	}
	_, filePypi, err := loadExceptions()
	if err != nil {
		return false
	}
	return filePypi[normalizePEP503(pkg)]
}

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

func getExceptionsFilePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.Getenv("HOME")
	}
	return filepath.Join(home, ".config", "shrimp-basket", "exceptions.txt")
}

// parseExceptionURL parses a package URL and returns (registry, pkgName, err)
// Supported domains: npmjs.com and pypi.org
func parseExceptionURL(rawURL string) (registry string, pkgName string, err error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "", "", err
	}

	host := strings.ToLower(u.Host)
	path := u.Path

	if host == "npmjs.com" || host == "www.npmjs.com" {
		// Expects /package/pkgName or /package/@scope/pkgName
		const prefix = "/package/"
		if !strings.HasPrefix(path, prefix) {
			return "", "", fmt.Errorf("invalid npmjs URL path: %s (expected /package/...)", path)
		}
		pkg := strings.TrimPrefix(path, prefix)
		pkg = strings.TrimSuffix(pkg, "/")
		if pkg == "" {
			return "", "", fmt.Errorf("missing package name in npmjs URL")
		}
		if strings.HasPrefix(pkg, "@") {
			parts := strings.Split(pkg, "/")
			if len(parts) != 2 || parts[1] == "" {
				return "", "", fmt.Errorf("invalid scoped package format: %s (expected @scope/pkg)", pkg)
			}
		}
		return "npm", pkg, nil
	}

	if host == "pypi.org" || host == "www.pypi.org" {
		// Expects /project/pkgName/
		const prefix = "/project/"
		if !strings.HasPrefix(path, prefix) {
			return "", "", fmt.Errorf("invalid pypi URL path: %s (expected /project/...)", path)
		}
		pkg := strings.TrimPrefix(path, prefix)
		pkg = strings.TrimSuffix(pkg, "/")
		if pkg == "" {
			return "", "", fmt.Errorf("missing package name in pypi URL")
		}
		if strings.Contains(pkg, "/") {
			return "", "", fmt.Errorf("invalid pypi package name: %s (cannot contain slashes)", pkg)
		}
		return "pypi", pkg, nil
	}

	return "", "", fmt.Errorf("unsupported registry domain: %s (only npmjs.com and pypi.org are supported)", host)
}

func loadExceptions() (map[string]bool, map[string]bool, error) {
	path := getExceptionsFilePath()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}

	npmBypass := make(map[string]bool)
	pypiBypass := make(map[string]bool)

	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		reg, pkg, err := parseExceptionURL(line)
		if err != nil {
			log.Printf("Warning: ignoring invalid exception URL '%s': %v", line, err)
			continue
		}
		if reg == "npm" {
			npmBypass[strings.ToLower(pkg)] = true
		} else if reg == "pypi" {
			pypiBypass[normalizePEP503(pkg)] = true
		}
	}
	return npmBypass, pypiBypass, nil
}

func modifyException(rawURL string, add bool) error {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return fmt.Errorf("exception URL cannot be empty")
	}

	// Validate the URL format first
	reg, pkg, err := parseExceptionURL(rawURL)
	if err != nil {
		return fmt.Errorf("invalid exception URL: %w", err)
	}

	// Normalize casing of URL based on registry conventions
	var normalizedURL string
	if reg == "npm" {
		normalizedURL = fmt.Sprintf("https://www.npmjs.com/package/%s", strings.ToLower(pkg))
	} else {
		normalizedURL = fmt.Sprintf("https://pypi.org/project/%s/", normalizePEP503(pkg))
	}

	path := getExceptionsFilePath()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create config directory: %w", err)
	}

	var lines []string
	data, err := os.ReadFile(path)
	if err == nil {
		rawLines := strings.Split(string(data), "\n")
		for _, line := range rawLines {
			line = strings.TrimSpace(line)
			if line != "" {
				lines = append(lines, line)
			}
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("failed to read exceptions file: %w", err)
	}

	found := false
	var newLines []string
	for _, line := range lines {
		if strings.EqualFold(line, normalizedURL) {
			found = true
			if add {
				newLines = append(newLines, line)
			}
		} else {
			newLines = append(newLines, line)
		}
	}

	if add && !found {
		newLines = append(newLines, normalizedURL)
		fmt.Printf("Adding '%s' to exceptions list...\n", normalizedURL)
	} else if !add && found {
		fmt.Printf("Removing '%s' from exceptions list...\n", normalizedURL)
	} else if add && found {
		fmt.Printf("'%s' is already in the exceptions list.\n", normalizedURL)
		return nil
	} else if !add && !found {
		fmt.Printf("'%s' was not found in the exceptions list.\n", normalizedURL)
		return nil
	}

	output := strings.Join(newLines, "\n")
	if len(newLines) > 0 {
		output += "\n"
	}

	// Atomic write using temp file and rename
	tmp, err := os.CreateTemp(dir, "exceptions-*.tmp")
	if err != nil {
		return fmt.Errorf("failed to create temporary config file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if err := os.Chmod(tmpName, 0644); err != nil {
		tmp.Close()
		return fmt.Errorf("failed to chmod temporary config: %w", err)
	}
	if _, err := tmp.Write([]byte(output)); err != nil {
		tmp.Close()
		return fmt.Errorf("failed to write temporary config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("failed to close temporary config: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("failed to commit exceptions file atomically: %w", err)
	}

	fmt.Printf("Updated exceptions list saved to %s\n", path)

	// Clear the cache file for this package so the proxy fetches the fresh index next time
	cacheSubdir := "npm"
	normalizedPkg := strings.ToLower(pkg)
	if reg == "pypi" {
		cacheSubdir = "pypi"
		normalizedPkg = normalizePEP503(pkg)
	}

	cacheFile := filepath.Join(getCacheDir(), cacheSubdir, url.QueryEscape(normalizedPkg)+".json")
	if err := os.Remove(cacheFile); err != nil && !os.IsNotExist(err) {
		log.Printf("Warning: failed to clear cache for %s: %v", pkg, err)
	} else if err == nil {
		fmt.Printf("Cleared cache for %s (%s)\n", pkg, reg)
	}

	return nil
}

func printExceptions() error {
	path := getExceptionsFilePath()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("No exceptions configured.")
			return nil
		}
		return fmt.Errorf("failed to read exceptions file: %w", err)
	}

	lines := strings.Split(string(data), "\n")
	count := 0
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			fmt.Println(line)
			count++
		}
	}

	if count == 0 {
		fmt.Println("No exceptions configured.")
	}
	return nil
}

// normalizePEP503 normalizes a package name according to PEP 503
func normalizePEP503(name string) string {
	return strings.ToLower(normPattern.ReplaceAllString(name, "-"))
}

// fetchUpstream executes an HTTP request against an upstream registry.
// No retry layer: every package manager (pip, npm, uv, yarn, pnpm) already
// retries on its own, and stacking retries multiplies upstream load on outage.
func (p *Proxy) fetchUpstream(req *http.Request) (*http.Response, error) {
	req.Header.Set("User-Agent", "shrimp-basket/1.0.0")
	return p.httpClient.Do(req)
}

// --- CORE FILTERING LOGIC ---

// filterPyPIIndex filters a PEP 691 JSON simple index, preserving all unknown fields (PEP 740, etc.)
func (p *Proxy) filterPyPIIndex(pkg string, data []byte) ([]byte, error) {
	if p.isPyPIBypassed(pkg) {
		log.Printf("[BYPASS] PyPI package %s is on the bypass list", pkg)
		return data, nil
	}
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
	resp, err := p.fetchUpstream(req)
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
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseSize)).Decode(&jsonMeta); err != nil {
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

// filterNPMIndex filters an NPM registry index, preserving all unknown fields.
// Pointer receiver kept for consistency with filterPyPIIndex so method values
// of either can be assigned to the filterFunc variable in updatePackageCache.
func (p *Proxy) filterNPMIndex(pkg string, data []byte) ([]byte, error) {
	if p.isNPMBypassed(pkg) {
		log.Printf("[BYPASS] NPM package %s is on the bypass list", pkg)
		return data, nil
	}
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

func (p *Proxy) handlePyPI(w http.ResponseWriter, r *http.Request) {
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
	cacheFile := filepath.Join(p.cacheDir, "pypi", url.QueryEscape(normPkg)+".json")

	// Check Cache
	if data, err := readCache(cacheFile); err == nil {
		servePyPICached(w, r, data)
		return
	}

	log.Printf("[FETCH] PyPI upstream index for: %s", normPkg)

	// Fetch from PyPI simple JSON endpoint
	req, _ := http.NewRequest("GET", fmt.Sprintf("https://pypi.org/simple/%s/", normPkg), nil)
	req.Header.Set("Accept", "application/vnd.pypi.simple.v1+json")

	resp, err := p.fetchUpstream(req)
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

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	if err != nil {
		http.Error(w, "Read Error", http.StatusInternalServerError)
		return
	}

	// Filter and fail closed if filtering fails
	filteredBody, err := p.filterPyPIIndex(normPkg, body)
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

func (p *Proxy) handleNPM(w http.ResponseWriter, r *http.Request) {
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

	cacheFile := filepath.Join(p.cacheDir, "npm", url.QueryEscape(pkg)+".json")

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

	resp, err := p.fetchUpstream(req)
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

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
	if err != nil {
		http.Error(w, "Read Error", http.StatusInternalServerError)
		return
	}

	filteredBody, err := p.filterNPMIndex(pkg, body)
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

	dir := filepath.Dir(cacheFile)
	if err := os.MkdirAll(dir, 0755); err != nil {
		log.Printf("Failed to create cache directory: %v", err)
		return
	}

	// Atomic cache write via unique temp file in the same directory + Rename.
	// Unique names (vs cacheFile + ".tmp") avoid corruption when two requests
	// for the same package race into writeCache concurrently.
	tmp, err := os.CreateTemp(dir, "cache-*.tmp")
	if err != nil {
		log.Printf("Failed to create temporary cache file: %v", err)
		return
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if err := os.Chmod(tmpName, 0600); err != nil {
		log.Printf("Failed to chmod temporary cache: %v", err)
		tmp.Close()
		return
	}
	if _, err := tmp.Write(data); err != nil {
		log.Printf("Failed to write temporary cache: %v", err)
		tmp.Close()
		return
	}
	if err := tmp.Close(); err != nil {
		log.Printf("Failed to close temporary cache: %v", err)
		return
	}
	if err := os.Rename(tmpName, cacheFile); err != nil {
		log.Printf("Failed to commit cache atomically: %v", err)
	}
}

// --- CACHE INSPECTION & MAINTENANCE ---

func (p *Proxy) cacheStats(subdir string) (int64, int) {
	var total int64
	var count int
	root := filepath.Join(p.cacheDir, subdir)
	filepath.WalkDir(root, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d == nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
			count++
		}
		return nil
	})
	return total, count
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGT"[exp])
}

func (p *Proxy) printCacheSize() {
	npmBytes, npmCount := p.cacheStats("npm")
	pypiBytes, pypiCount := p.cacheStats("pypi")
	fmt.Printf("Cache directory: %s\n", p.cacheDir)
	fmt.Printf("  npm:   %10s  (%d files)\n", humanBytes(npmBytes), npmCount)
	fmt.Printf("  pypi:  %10s  (%d files)\n", humanBytes(pypiBytes), pypiCount)
	fmt.Printf("  total: %10s  (%d files)\n", humanBytes(npmBytes+pypiBytes), npmCount+pypiCount)
}

func (p *Proxy) cleanCache() {
	for _, sub := range []string{"npm", "pypi"} {
		dir := filepath.Join(p.cacheDir, sub)
		if err := os.RemoveAll(dir); err != nil {
			log.Printf("Failed to remove %s: %v", dir, err)
			continue
		}
		fmt.Printf("Removed %s\n", dir)
	}
}

// --- DAILY UPDATE ROUTINE ---

func (p *Proxy) runDailyUpdate() {
	log.Printf("Starting daily metadata cache update in: %s", p.cacheDir)

	pypiFiles, _ := filepath.Glob(filepath.Join(p.cacheDir, "pypi", "*.json"))
	for _, file := range pypiFiles {
		escapedPkg := strings.TrimSuffix(filepath.Base(file), ".json")
		pkg, err := url.QueryUnescape(escapedPkg)
		if err == nil {
			p.updatePackageCache("pypi", pkg)
			time.Sleep(50 * time.Millisecond)
		}
	}

	npmFiles, _ := filepath.Glob(filepath.Join(p.cacheDir, "npm", "*.json"))
	for _, file := range npmFiles {
		escapedPkg := strings.TrimSuffix(filepath.Base(file), ".json")
		pkg, err := url.QueryUnescape(escapedPkg)
		if err == nil {
			p.updatePackageCache("npm", pkg)
			time.Sleep(50 * time.Millisecond)
		}
	}
	log.Printf("Daily update complete.")
}

func (p *Proxy) updatePackageCache(registryType, pkg string) {
	log.Printf("[UPDATE] Fetching latest metadata for: %s (%s)", pkg, registryType)
	var targetUrl string
	var filterFunc func(string, []byte) ([]byte, error)
	var cacheFile string

	if registryType == "pypi" {
		normPkg := normalizePEP503(pkg)
		targetUrl = fmt.Sprintf("https://pypi.org/simple/%s/", normPkg)
		filterFunc = p.filterPyPIIndex
		cacheFile = filepath.Join(p.cacheDir, "pypi", url.QueryEscape(normPkg)+".json")
	} else {
		upstreamPath := pkg
		if strings.Contains(pkg, "/") {
			parts := strings.SplitN(pkg, "/", 2)
			upstreamPath = parts[0] + "%2F" + parts[1]
		}
		targetUrl = "https://registry.npmjs.org/" + upstreamPath
		filterFunc = p.filterNPMIndex
		cacheFile = filepath.Join(p.cacheDir, "npm", url.QueryEscape(pkg)+".json")
	}

	req, _ := http.NewRequest("GET", targetUrl, nil)
	if registryType == "pypi" {
		req.Header.Set("Accept", "application/vnd.pypi.simple.v1+json")
	}

	resp, err := p.fetchUpstream(req)
	if err != nil {
		log.Printf("Update request failed for %s: %v", pkg, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("Update response error for %s: %d", pkg, resp.StatusCode)
		return
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize))
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
	cacheSizeFlag := flag.Bool("cache-size", false, "Print cache size and exit")
	cacheCleanFlag := flag.Bool("cache-clean", false, "Delete all cached metadata and exit")
	bypassFlag := flag.String("bypass", "", "Comma-separated list of package page URLs to bypass quarantine (e.g. 'https://www.npmjs.com/package/@belsar-ai/joplin-mcp')")
	addException := flag.String("add-exception", "", "Add a package URL to the quarantine bypass list (e.g. 'https://pypi.org/project/pandas/')")
	removeException := flag.String("remove-exception", "", "Remove a package URL from the quarantine bypass list")
	listExceptions := flag.Bool("list-exceptions", false, "List all package URLs in the quarantine bypass list")
	flag.Parse()

	if *addException != "" {
		if err := modifyException(*addException, true); err != nil {
			log.Fatalf("Error adding exception: %v", err)
		}
		return
	}

	if *removeException != "" {
		if err := modifyException(*removeException, false); err != nil {
			log.Fatalf("Error removing exception: %v", err)
		}
		return
	}

	if *listExceptions {
		if err := printExceptions(); err != nil {
			log.Fatalf("Error listing exceptions: %v", err)
		}
		return
	}

	bypassEnv := os.Getenv("SHRIMP_BYPASS")
	bypassVal := *bypassFlag
	if bypassVal == "" {
		bypassVal = bypassEnv
	}

	npmBypass := make(map[string]bool)
	pypiBypass := make(map[string]bool)

	// Parse bypass CLI flag / Environment variable
	if bypassVal != "" {
		for _, item := range strings.Split(bypassVal, ",") {
			item = strings.TrimSpace(item)
			if item == "" {
				continue
			}
			reg, pkg, err := parseExceptionURL(item)
			if err != nil {
				log.Printf("Warning: invalid bypass URL '%s': %v", item, err)
				continue
			}
			if reg == "npm" {
				npmBypass[strings.ToLower(pkg)] = true
			} else if reg == "pypi" {
				pypiBypass[normalizePEP503(pkg)] = true
			}
		}
	}

	p := &Proxy{
		httpClient:     &http.Client{Timeout: 15 * time.Second},
		cacheDir:       getCacheDir(),
		npmBypassList:  npmBypass,
		pypiBypassList: pypiBypass,
	}

	if *updateFlag {
		p.runDailyUpdate()
		return
	}

	if *cacheSizeFlag {
		p.printCacheSize()
		return
	}

	if *cacheCleanFlag {
		p.cleanCache()
		return
	}

	log.Printf("[START] Starting shrimp-basket quarantine proxy...")
	log.Printf("[START] Cache Directory: %s", p.cacheDir)

	listener, err := net.Listen("tcp", "127.0.0.1:"+defaultPort)
	if err != nil {
		log.Fatalf("Failed to listen on 127.0.0.1:%s: %v", defaultPort, err)
	}
	log.Printf("[START] Listening on 127.0.0.1:%s", defaultPort)

	mux := http.NewServeMux()
	mux.HandleFunc("/simple/", p.handlePyPI)
	mux.HandleFunc("/", p.handleNPM)

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
