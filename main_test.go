package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestServeAsset(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a fake-but-detectable-as-image file
	createFakeImageFile(t, tmpDir, "products/shoe.jpg")

	// Initialize cache to avoid nil panic
	testCache := NewAssetCache(Config{
		CacheSize:       64,
		DefaultCacheTTL: 10 * time.Minute,
	})
	originalCache := cache
	cache = testCache
	t.Cleanup(func() { cache = originalCache })

	testCases := []struct {
		name       string
		path       string
		wantStatus int
		checkBody  bool // whether to check body at all
		wantHeader map[string]string
	}{
		{
			name:       "valid_image_-_should_serve_file",
			path:       "/asset/products/shoe.jpg",
			wantStatus: http.StatusOK,
			checkBody:  true, // we'll check length > 0 instead of content
			wantHeader: map[string]string{
				"Content-Type":  "image/jpeg",
				"Cache-Control": "public, max-age=31536000, immutable",
				"X-Cache":       "MISS",
			},
		},
		{
			name:       "non-existing file",
			path:       "/asset/not/exists.jpg",
			wantStatus: http.StatusNotFound,
			checkBody:  false,
		},
		{
			name:       "invalid_path_-_traversal",
			path:       "/asset/../secret.txt",
			wantStatus: http.StatusBadRequest,
			checkBody:  false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			w := httptest.NewRecorder()

			serveAsset(tmpDir, w, req)

			resp := w.Result()

			if resp.StatusCode != tc.wantStatus {
				t.Errorf("expected status %d, got %d", tc.wantStatus, resp.StatusCode)
			}

			body, _ := io.ReadAll(resp.Body)

			if tc.checkBody {
				if len(body) == 0 {
					t.Error("expected non-empty body for successful image response")
				}
				// Optional: check minimal size if you want to be stricter
				if len(body) < 50 {
					t.Errorf("image response body too small (%d bytes)", len(body))
				}
			}

			for k, want := range tc.wantHeader {
				got := resp.Header.Get(k)
				if !strings.Contains(got, want) {
					t.Errorf("header %s expected to contain %q, got %q", k, want, got)
				}
			}
		})
	}
}

// Helper: creates a tiny file that DetectContentType recognizes as JPEG
func createFakeImageFile(t *testing.T, baseDir, relPath string) {
	t.Helper()
	fullPath := filepath.Join(baseDir, relPath)

	if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
		t.Fatalf("failed to create dir: %v", err)
	}

	// Minimal valid-ish JPEG structure (enough for DetectContentType)
	jpegStart := []byte{
		0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, // SOI + APP0
		0x4A, 0x46, 0x49, 0x46, 0x00, 0x01, // JFIF identifier
		0x01, 0x01, 0x00, 0x48, 0x00, 0x48, 0x00, 0x00,
	}

	jpegEnd := []byte{0xFF, 0xD9} // EOI

	data := append(jpegStart, bytes.Repeat([]byte{0xAA}, 100)...)
	data = append(data, jpegEnd...)

	if err := os.WriteFile(fullPath, data, 0644); err != nil {
		t.Fatalf("failed to write fake jpeg: %v", err)
	}
}

func TestHealthEndpoint(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	healthHandler(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if w.Body.String() != "OK" {
		t.Errorf("expected 'OK', got %q", w.Body.String())
	}
}
