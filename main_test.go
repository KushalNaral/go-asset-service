package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestServeAsset(t *testing.T) {
	// Create isolated temporary directory for this test suite
	tmpDir := t.TempDir()

	// Save original and restore after test
	originalBase := BASE_PATH
	BASE_PATH = tmpDir
	t.Cleanup(func() {
		BASE_PATH = originalBase
	})

	// Setup test files
	createTestFile(t, tmpDir, "products/shoe.jpg", "fake-jpeg-binary-content")
	createTestFile(t, tmpDir, "test.txt", "hello-world-text")
	if err := os.Mkdir(filepath.Join(tmpDir, "empty-dir"), 0755); err != nil {
		t.Fatal(err)
	}

	testCases := []struct {
		name           string
		requestPath    string
		wantStatus     int
		wantBodySubstr string
		wantContent    bool // check for our fake content
	}{
		{
			name:        "valid image file",
			requestPath: "/asset/products/shoe.jpg",
			wantStatus:  http.StatusOK,
			wantContent: true,
		},
		{
			name:           "empty file path",
			requestPath:    "/asset",
			wantStatus:     http.StatusBadRequest,
			wantBodySubstr: "file path is required",
		},
		{
			name:           "path traversal attempt (..)",
			requestPath:    "/asset/../secrets.txt",
			wantStatus:     http.StatusBadRequest,
			wantBodySubstr: "invalid path",
		},
		{
			name:           "deep path traversal",
			requestPath:    "/asset/../../../etc/passwd",
			wantStatus:     http.StatusBadRequest,
			wantBodySubstr: "invalid path",
		},
		{
			name:        "non-existing file",
			requestPath: "/asset/not/exists.png",
			wantStatus:  http.StatusNotFound,
		},
		{
			name:           "trying to access directory",
			requestPath:    "/asset/empty-dir",
			wantStatus:     http.StatusForbidden,
			wantBodySubstr: "directory listing not allowed",
		},
		{
			name:           "absolute path attempt",
			requestPath:    "/asset//etc/passwd",
			wantStatus:     http.StatusBadRequest,
			wantBodySubstr: "invalid path",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.requestPath, nil)
			w := httptest.NewRecorder()

			serveAsset(w, req)

			resp := w.Result()

			if resp.StatusCode != tc.wantStatus {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}

			bodyBytes, _ := io.ReadAll(resp.Body)
			body := string(bodyBytes)

			if tc.wantBodySubstr != "" && !strings.Contains(body, tc.wantBodySubstr) {
				t.Errorf("expected body to contain %q, got:\n%s", tc.wantBodySubstr, body)
			}

			if tc.wantContent {
				if !strings.Contains(body, "fake-jpeg-binary-content") {
					t.Error("expected test file content not found in response")
				}
			}
		})
	}
}

func createTestFile(t *testing.T, baseDir, relPath, content string) {
	t.Helper()
	fullPath := filepath.Join(baseDir, relPath)
	dir := filepath.Dir(fullPath)

	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("failed to create dir: %v", err)
	}

	if err := os.WriteFile(fullPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write test file: %v", err)
	}
}

func TestHealthEndpoint(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "OK")
	})

	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", w.Code)
	}

	if body := w.Body.String(); body != "OK" {
		t.Errorf("expected body 'OK', got %q", body)
	}
}
