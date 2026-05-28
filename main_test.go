package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"testing"
	"time"
)

type mockTransport struct {
	roundTrip func(*http.Request) (*http.Response, error)
}

func (m *mockTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return m.roundTrip(req)
}

func TestFilterNPMIndexCases(t *testing.T) {
	now := time.Now()
	recentTime := now.Add(-2 * 24 * time.Hour).Format(time.RFC3339Nano)
	oldTime := now.Add(-10 * 24 * time.Hour).Format(time.RFC3339Nano)
	milisecondTime := now.Add(-8 * 24 * time.Hour).Format("2006-01-02T15:04:05.999Z")

	tests := []struct {
		name          string
		pkgName       string
		inputJSON     string
		expectError   bool
		verifyIndex   func(*testing.T, map[string]interface{})
	}{
		{
			name:    "Mixed versions and latest tag rewrite",
			pkgName: "foo",
			inputJSON: `{
				"name": "foo",
				"dist-tags": {
					"latest": "2.0.0",
					"stable": "1.0.0"
				},
				"versions": {
					"1.0.0": {},
					"2.0.0": {}
				},
				"time": {
					"created": "2026-05-01T00:00:00Z",
					"modified": "2026-05-24T00:00:00Z",
					"1.0.0": "` + oldTime + `",
					"2.0.0": "` + recentTime + `"
				}
			}`,
			expectError: false,
			verifyIndex: func(t *testing.T, index map[string]interface{}) {
				versions := getMap(index, "versions")
				if _, ok := versions["1.0.0"]; !ok {
					t.Errorf("expected 1.0.0 to be kept")
				}
				if _, ok := versions["2.0.0"]; ok {
					t.Errorf("expected 2.0.0 to be filtered")
				}
				distTags := getMap(index, "dist-tags")
				if distTags["latest"] != "2.0.0" {
					t.Errorf("expected latest tag to remain unmodified at 2.0.0, got %v", distTags["latest"])
				}
				if distTags["stable"] != "1.0.0" {
					t.Errorf("expected stable tag to remain 1.0.0, got %v", distTags["stable"])
				}
			},
		},
		{
			name:    "All quarantined",
			pkgName: "foo",
			inputJSON: `{
				"name": "foo",
				"dist-tags": {
					"latest": "1.0.0"
				},
				"versions": {
					"1.0.0": {}
				},
				"time": {
					"1.0.0": "` + recentTime + `"
				}
			}`,
			expectError: false,
			verifyIndex: func(t *testing.T, index map[string]interface{}) {
				versions := getMap(index, "versions")
				if len(versions) != 0 {
					t.Errorf("expected versions to be empty")
				}
				distTags := getMap(index, "dist-tags")
				if distTags["latest"] != "1.0.0" {
					t.Errorf("expected latest tag to remain unchanged when no safe versions exist")
				}
			},
		},
		{
			name:    "Malformed timestamp fails closed",
			pkgName: "foo",
			inputJSON: `{
				"name": "foo",
				"dist-tags": {
					"latest": "1.0.0"
				},
				"versions": {
					"1.0.0": {}
				},
				"time": {
					"1.0.0": "not-a-timestamp"
				}
			}`,
			expectError: false,
			verifyIndex: func(t *testing.T, index map[string]interface{}) {
				versions := getMap(index, "versions")
				if len(versions) != 0 {
					t.Errorf("expected 1.0.0 to be filtered due to malformed timestamp")
				}
			},
		},
		{
			name:    "Missing time map fails closed",
			pkgName: "foo",
			inputJSON: `{
				"name": "foo",
				"dist-tags": {
					"latest": "1.0.0"
				},
				"versions": {
					"1.0.0": {}
				}
			}`,
			expectError: true,
		},
		{
			name:    "Millisecond timestamp handling",
			pkgName: "@scope/foo",
			inputJSON: `{
				"name": "@scope/foo",
				"dist-tags": {
					"latest": "1.0.0"
				},
				"versions": {
					"1.0.0": {}
				},
				"time": {
					"1.0.0": "` + milisecondTime + `"
				}
			}`,
			expectError: false,
			verifyIndex: func(t *testing.T, index map[string]interface{}) {
				versions := getMap(index, "versions")
				if _, ok := versions["1.0.0"]; !ok {
					t.Errorf("expected millisecond timestamp version to be parsed correctly and kept")
				}
			},
		},
	}

	p := &Proxy{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			output, err := p.filterNPMIndex(tt.pkgName, []byte(tt.inputJSON))
			if tt.expectError {
				if err == nil {
					t.Errorf("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("filterNPMIndex failed: %v", err)
			}

			var index map[string]interface{}
			if err := json.Unmarshal(output, &index); err != nil {
				t.Fatalf("failed to unmarshal output: %v", err)
			}
			if tt.verifyIndex != nil {
				tt.verifyIndex(t, index)
			}
		})
	}
}

func TestFilterPyPIIndex(t *testing.T) {
	now := time.Now()
	recentTimeStr := now.Add(-2 * 24 * time.Hour).Format("2006-01-02T15:04:05")
	oldTimeStr := now.Add(-10 * 24 * time.Hour).Format("2006-01-02T15:04:05")

	mockJSONResponse := `{
		"releases": {
			"1.0.0": [
				{
					"filename": "foo-1.0.0.tar.gz",
					"upload_time_iso_8601": "` + oldTimeStr + `Z"
				}
			],
			"2.0.0": [
				{
					"filename": "foo-2.0.0.tar.gz",
					"upload_time_iso_8601": "` + recentTimeStr + `Z"
				}
			]
		}
	}`

	p := &Proxy{
		httpClient: &http.Client{
			Transport: &mockTransport{
				roundTrip: func(req *http.Request) (*http.Response, error) {
					return &http.Response{
						StatusCode: http.StatusOK,
						Body:       io.NopCloser(bytes.NewBufferString(mockJSONResponse)),
						Header:     make(http.Header),
					}, nil
				},
			},
		},
	}

	simpleIndex := `{
		"meta": {},
		"name": "foo",
		"versions": ["1.0.0", "2.0.0"],
		"files": [
			{
				"filename": "foo-1.0.0.tar.gz",
				"url": "https://files.pythonhosted.org/foo-1.0.0.tar.gz",
				"hashes": {"sha256": "1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"},
				"core-metadata": {"sha256": "abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd"},
				"yanked": "critical security bug"
			},
			{
				"filename": "foo-2.0.0.tar.gz",
				"url": "https://files.pythonhosted.org/foo-2.0.0.tar.gz",
				"hashes": {"sha256": "2234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"}
			}
		]
	}`

	outputBytes, err := p.filterPyPIIndex("foo", []byte(simpleIndex))
	if err != nil {
		t.Fatalf("filterPyPIIndex failed: %v", err)
	}

	var res struct {
		Files []PEP691File `json:"files"`
	}
	if err := json.Unmarshal(outputBytes, &res); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}

	if len(res.Files) != 1 {
		t.Errorf("expected exactly 1 file, got %d", len(res.Files))
	} else if res.Files[0].Filename != "foo-1.0.0.tar.gz" {
		t.Errorf("expected kept file to be foo-1.0.0.tar.gz, got %s", res.Files[0].Filename)
	}

	if res.Files[0].Yanked != "critical security bug" {
		t.Errorf("expected Yanked to be 'critical security bug', got %v", res.Files[0].Yanked)
	}
	if res.Files[0].CoreMetadata == nil {
		t.Errorf("expected CoreMetadata to be preserved, got nil")
	}
}

func getMap(m map[string]interface{}, key string) map[string]interface{} {
	val, ok := m[key]
	if !ok {
		return nil
	}
	b, err := json.Marshal(val)
	if err != nil {
		return nil
	}
	var res map[string]interface{}
	json.Unmarshal(b, &res)
	return res
}

func TestBypassQuarantine(t *testing.T) {
	t.Setenv("SHRIMP_EXCEPTIONS_FILE", filepath.Join(t.TempDir(), "none.txt"))
	p := &Proxy{
		npmBypassList: map[string]bool{
			"@belsar-ai/joplin-mcp": true,
		},
		pypiBypassList: map[string]bool{
			"exempt-pkg": true,
		},
	}

	// 1. Check matched cases
	if !p.isNPMBypassed("@belsar-ai/joplin-mcp") {
		t.Errorf("expected @belsar-ai/joplin-mcp to be bypassed on NPM")
	}
	if !p.isPyPIBypassed("exempt-pkg") {
		t.Errorf("expected exempt-pkg to be bypassed on PyPI")
	}
	if !p.isPyPIBypassed("Exempt_Pkg") { // matches normalized (exempt-pkg)
		t.Errorf("expected Exempt_Pkg to match normalized name on PyPI")
	}

	// 2. Check non-matched cases
	if p.isNPMBypassed("other-pkg") {
		t.Errorf("expected other-pkg NOT to be bypassed on NPM")
	}
}

func TestParseExceptionURL(t *testing.T) {
	tests := []struct {
		url      string
		wantReg  string
		wantPkg  string
		wantFail bool
	}{
		{"https://www.npmjs.com/package/@belsar-ai/joplin-mcp", "npm", "@belsar-ai/joplin-mcp", false},
		{"https://npmjs.com/package/express", "npm", "express", false},
		{"https://pypi.org/project/pandas/", "pypi", "pandas", false},
		{"https://pypi.org/project/requests", "pypi", "requests", false},
		{"https://github.com/some/repo", "", "", true},
		{"invalid-url", "", "", true},
		// Invalid NPM cases
		{"https://www.npmjs.com/package/express/v/1.0.0", "", "", true},
		{"https://www.npmjs.com/package/@scope", "", "", true},
		{"https://www.npmjs.com/package/@scope/", "", "", true},
		// Invalid PyPI cases
		{"https://pypi.org/project/pandas/subpath", "", "", true},
	}

	for _, tt := range tests {
		reg, pkg, err := parseExceptionURL(tt.url)
		if tt.wantFail {
			if err == nil {
				t.Errorf("expected URL %s to fail parsing, but it succeeded", tt.url)
			}
		} else {
			if err != nil {
				t.Errorf("unexpected error parsing %s: %v", tt.url, err)
			}
			if reg != tt.wantReg || pkg != tt.wantPkg {
				t.Errorf("parseExceptionURL(%s) = (%s, %s), want (%s, %s)", tt.url, reg, pkg, tt.wantReg, tt.wantPkg)
			}
		}
	}
}

func TestExceptionsFileOperationsHermetic(t *testing.T) {
	// Create a temporary file path
	tmpDir := t.TempDir()
	tmpFile := filepath.Join(tmpDir, "test_exceptions.txt")

	// Set env var to override the config file path
	t.Setenv("SHRIMP_EXCEPTIONS_FILE", tmpFile)

	// 1. Verify initially it loads empty maps
	npm, pypi, err := loadExceptions()
	if err != nil {
		t.Fatalf("loadExceptions failed: %v", err)
	}
	if len(npm) != 0 || len(pypi) != 0 {
		t.Errorf("expected empty exceptions maps, got npm=%v pypi=%v", npm, pypi)
	}

	// 2. Add an exception
	err = modifyException("https://www.npmjs.com/package/@belsar-ai/joplin-mcp", true)
	if err != nil {
		t.Fatalf("modifyException add failed: %v", err)
	}

	// 3. Load again and verify
	npm, pypi, err = loadExceptions()
	if err != nil {
		t.Fatalf("loadExceptions failed: %v", err)
	}
	if !npm["@belsar-ai/joplin-mcp"] {
		t.Errorf("expected npm package @belsar-ai/joplin-mcp to be bypassed")
	}

	// 4. Remove the exception
	err = modifyException("https://www.npmjs.com/package/@belsar-ai/joplin-mcp", false)
	if err != nil {
		t.Fatalf("modifyException remove failed: %v", err)
	}

	// 5. Load again and verify
	npm, pypi, err = loadExceptions()
	if err != nil {
		t.Fatalf("loadExceptions failed: %v", err)
	}
	if npm["@belsar-ai/joplin-mcp"] {
		t.Errorf("expected npm package @belsar-ai/joplin-mcp NOT to be bypassed after removal")
	}
}
