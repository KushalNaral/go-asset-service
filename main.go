package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

var (
	// Change this to your actual storage path or make it configurable via env
	BASE_PATH = "/home/rome/work/ecom/api/storage/app/public"
	PORT      = ":8080"
)

func main() {
	log.SetPrefix("Asset Service : \t")
	log.Printf("Starting on http://localhost%s", PORT)

	mux := http.NewServeMux()

	// Health check
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "OK")
	})

	// Main asset endpoint: GET /asset/products/shoe.jpg etc.
	mux.HandleFunc("GET /asset/", serveAsset)

	// Nice root message
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			fmt.Fprint(w, "Asset Service is running.\nUse: GET /asset/path/to/your/file.jpg")
			return
		}
		http.NotFound(w, r)
	})

	log.Fatal(http.ListenAndServe(PORT, mux))
}

func serveAsset(w http.ResponseWriter, r *http.Request) {
	// Extract file path after "/asset/"
	rawPath := strings.TrimPrefix(r.URL.Path, "/asset")
	rawPath = strings.TrimPrefix(rawPath, "/") // clean extra leading slash

	if rawPath == "" {
		http.Error(w, "file path is required", http.StatusBadRequest)
		return
	}

	// Basic path traversal protection
	if strings.Contains(rawPath, "..") || strings.HasPrefix(rawPath, "/") {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	cleanPath := filepath.Clean(rawPath)
	fullPath := filepath.Join(BASE_PATH, cleanPath)

	// Check existence & type
	info, err := os.Stat(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			http.NotFound(w, r)
			return
		}
		log.Printf("stat error: %v - path: %s", err, fullPath)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	if info.IsDir() {
		http.Error(w, "directory listing not allowed", http.StatusForbidden)
		return
	}

	// Serve the original file (later we'll add caching + processing)
	http.ServeFile(w, r, fullPath)
}
