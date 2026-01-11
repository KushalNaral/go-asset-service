package main

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var (
	config Config
	cache  *AssetCache
)

func main() {
	config = LoadConfig()
	cache = NewAssetCache(config)

	log.SetPrefix("Asset Service : \t")
	log.Printf("Starting on http://0.0.0.0%s", config.Port)
	log.Printf("Serving files from: %s", config.BasePath)
	log.Printf("Cache size: %d entries, TTL: %v", config.CacheSize, config.DefaultCacheTTL)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", healthHandler)
	mux.HandleFunc("GET /asset/", func(w http.ResponseWriter, r *http.Request) {
		serveAsset(config.BasePath, w, r)
	})
	mux.HandleFunc("GET /", rootHandler)

	log.Fatal(http.ListenAndServe(config.Port, mux))
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "OK")
}

func rootHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" {
		fmt.Fprint(w, "Asset Service running!\nGET /asset/path/to/image.jpg?w=800&h=600")
		return
	}
	http.NotFound(w, r)
}

func serveAsset(basePath string, w http.ResponseWriter, r *http.Request) {
	rawPath := strings.TrimPrefix(r.URL.Path, "/asset")
	rawPath = strings.TrimPrefix(rawPath, "/")

	if rawPath == "" {
		http.Error(w, "file path required", http.StatusBadRequest)
		return
	}

	if strings.Contains(rawPath, "..") || strings.HasPrefix(rawPath, "/") {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	cleanPath := filepath.Clean(rawPath)
	fullPath := filepath.Join(basePath, cleanPath)

	cacheKey := cache.cacheKey(r, cleanPath)

	// Check cache first
	if cached, found := cache.Get(cacheKey); found {
		for k, vv := range cached.Headers {
			for _, v := range vv {
				w.Header().Set(k, v)
			}
		}
		w.Header().Set("X-Cache", "HIT")
		w.WriteHeader(cached.StatusCode)
		w.Write(cached.Body)
		return
	}

	// File system access
	file, err := os.Open(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			http.NotFound(w, r)
			return
		}
		log.Printf("open error: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil || stat.IsDir() {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	// Read once
	body, err := io.ReadAll(file)
	if err != nil {
		log.Printf("read error: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Generate ETag
	hash := md5.Sum(body)
	etag := fmt.Sprintf("\"%s\"", hex.EncodeToString(hash[:]))

	// Set proper headers
	w.Header().Set("Content-Type", http.DetectContentType(body))
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("X-Cache", "MISS")

	// Conditional request support
	if match := r.Header.Get("If-None-Match"); match == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	// Cache the response
	entry := CacheEntry{
		Body:       body,
		Headers:    w.Header().Clone(),
		ETag:       etag,
		StatusCode: http.StatusOK,
		CreatedAt:  time.Now(),
	}
	cache.Set(cacheKey, entry)

	w.WriteHeader(http.StatusOK)
	w.Write(body)
}
