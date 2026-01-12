package main

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/h2non/bimg"
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
	mux.HandleFunc("GET /admin/clear-cache", func(w http.ResponseWriter, r *http.Request) {
		secret := r.URL.Query().Get("secret")
		if secret != config.ClearKey {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		before := cache.lru.Len()
		cache.lru.Purge()
		after := cache.lru.Len()

		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "Cache cleared successfully\nBefore: %d entries\nAfter: %d entries\n", before, after)
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
	originalFullPath := filepath.Join(basePath, cleanPath)

	// ───────────────────────────────────────────────
	// Parse transformation parameters
	// ───────────────────────────────────────────────
	q := r.URL.Query()

	width, _ := strconv.Atoi(q.Get("w"))
	height, _ := strconv.Atoi(q.Get("h"))

	quality := 85
	if qs := q.Get("q"); qs != "" {
		if qi, err := strconv.Atoi(qs); err == nil && qi >= 1 && qi <= 100 {
			quality = qi
		}
	}

	// Supported output formats
	format := "jpeg" // default
	formatStr := q.Get("format")
	switch formatStr {
	case "webp", "avif", "png", "jpeg":
		format = formatStr
	}

	// Optional: fit mode (cover, contain, scale-down, etc.)
	fit := q.Get("fit")
	if fit == "" {
		fit = "cover" // most common default for product images
	}

	// ───────────────────────────────────────────────
	// Build cache key that includes ALL transformation params
	// ───────────────────────────────────────────────
	cacheKey := cache.cacheKeyWithTransform(r, cleanPath, width, height, quality, format, fit)

	// Try cache first
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

	// ───────────────────────────────────────────────
	// MISS - process the image
	// ───────────────────────────────────────────────

	// 1. Read original file
	originalData, err := bimg.Read(originalFullPath)
	if err != nil {
		if os.IsNotExist(err) {
			http.NotFound(w, r)
		} else {
			log.Printf("read original error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
		return
	}

	img := bimg.NewImage(originalData)

	// 2. Prepare processing options
	options := bimg.Options{
		Width:         width,
		Height:        height,
		Quality:       quality,
		StripMetadata: true, // remove metadata (EXIF, etc.)
		NoAutoRotate:  false,
		Interlace:     true, // progressive JPEG/WebP
		Gravity:       bimg.GravityCentre,
	}

	// Fit mode
	switch fit {
	case "contain":
		options.Crop = false
	case "cover":
		options.Crop = true
	case "scale-down":
		options.Enlarge = false
	case "crop":
		options.Crop = true
	default:
		options.Crop = true // cover is most common default
	}

	// Output format
	switch format {
	case "webp":
		options.Type = bimg.WEBP
	case "avif":
		options.Type = bimg.AVIF
	case "png":
		options.Type = bimg.PNG
	default:
		options.Type = bimg.JPEG
	}

	// 3. Process the image
	processed, err := img.Process(options)
	if err != nil {
		log.Printf("image processing error: %v", err)
		http.Error(w, "cannot process image", http.StatusInternalServerError)
		return
	}

	result := processed

	// 4. Determine correct Content-Type
	contentType := "image/jpeg"
	switch format {
	case "webp":
		contentType = "image/webp"
	case "avif":
		contentType = "image/avif"
	case "png":
		contentType = "image/png"
	}

	// 5. Generate ETag from processed image
	hash := md5.Sum(result)
	etag := fmt.Sprintf("\"%s\"", hex.EncodeToString(hash[:]))

	// 6. Set response headers
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("Vary", "Accept") // important for format negotiation
	w.Header().Set("X-Cache", "MISS")

	// Conditional request (If-None-Match)
	if match := r.Header.Get("If-None-Match"); match == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	// 7. Cache the result
	entry := CacheEntry{
		Body:       result,
		Headers:    w.Header().Clone(),
		ETag:       etag,
		StatusCode: http.StatusOK,
		CreatedAt:  time.Now(),
	}
	cache.Set(cacheKey, entry)

	// 8. Send response
	w.WriteHeader(http.StatusOK)
	w.Write(result)
}
