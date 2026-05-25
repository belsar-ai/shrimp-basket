package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
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

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			output, err := filterNPMIndex(tt.pkgName, []byte(tt.inputJSON))
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

	oldTransport := pypiClient.Transport
	defer func() { pypiClient.Transport = oldTransport }()

	pypiClient.Transport = &mockTransport{
		roundTrip: func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       io.NopCloser(bytes.NewBufferString(mockJSONResponse)),
				Header:     make(http.Header),
			}, nil
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

	outputBytes, err := filterPyPIIndex("foo", []byte(simpleIndex))
	if err != nil {
		t.Fatalf("filterPyPIIndex failed: %v", err)
	}

	var res PEP691Response
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
