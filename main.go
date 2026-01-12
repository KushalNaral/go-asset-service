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
	// Determine file extension
	// ───────────────────────────────────────────────
	ext := strings.ToLower(filepath.Ext(cleanPath))

	// Supported image extensions (expand as needed for your libvips)
	supportedImageExts := map[string]bool{
		".jpg":  true,
		".jpeg": true,
		".png":  true,
		".webp": true,
		".avif": true,
		".heic": true,
		".heif": true,
		".tiff": true,
		".tif":  true,
		".gif":  true,
	}

	// ───────────────────────────────────────────────
	// Parse query params once
	// ───────────────────────────────────────────────
	q := r.URL.Query()

	// Check if ANY transformation is requested
	hasTransform := false
	if q.Has("w") || q.Has("h") || q.Has("q") || q.Has("format") || q.Has("fit") {
		hasTransform = true
	}

	// ───────────────────────────────────────────────
	// DEFAULT BEHAVIOR: serve original file directly
	// (no params OR non-image file)
	// ───────────────────────────────────────────────
	if !hasTransform || !supportedImageExts[ext] {
		data, err := os.ReadFile(originalFullPath)
		if err != nil {
			if os.IsNotExist(err) {
				http.NotFound(w, r)
			} else {
				log.Printf("cannot read original file %s: %v", originalFullPath, err)
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
			return
		}

		// Determine content type
		contentType := http.DetectContentType(data)
		// Improve for common video types
		if contentType == "application/octet-stream" {
			switch ext {
			case ".mp4":
				contentType = "video/mp4"
			case ".webm":
				contentType = "video/webm"
			case ".mov":
				contentType = "video/quicktime"
			case ".m4v":
				contentType = "video/x-m4v"
			case ".avi":
				contentType = "video/x-msvideo"
			case ".mkv":
				contentType = "video/x-matroska"
			}
		}

		// Headers for originals
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		w.Header().Set("Accept-Ranges", "bytes") // good for video seeking

		// ETag for conditional requests
		hash := md5.Sum(data)
		etag := fmt.Sprintf("\"%s\"", hex.EncodeToString(hash[:]))
		w.Header().Set("ETag", etag)

		if match := r.Header.Get("If-None-Match"); match == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}

		w.Header().Set("X-Cache", "BYPASS")
		w.WriteHeader(http.StatusOK)
		w.Write(data)
		return
	}

	// ───────────────────────────────────────────────
	// Only reach here if: it's an image AND transformation requested
	// ───────────────────────────────────────────────

	width, _ := strconv.Atoi(q.Get("w"))
	height, _ := strconv.Atoi(q.Get("h"))

	quality := 85
	if qs := q.Get("q"); qs != "" {
		if qi, err := strconv.Atoi(qs); err == nil && qi >= 1 && qi <= 100 {
			quality = qi
		}
	}

	format := "jpeg"
	formatStr := q.Get("format")
	switch formatStr {
	case "webp", "avif", "png", "jpeg":
		format = formatStr
	}

	fit := q.Get("fit")
	if fit == "" {
		fit = "cover"
	}

	// Build cache key **only** when transforming
	cacheKey := cache.cacheKeyWithTransform(r, cleanPath, width, height, quality, format, fit)

	// Try cache
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
	// Process image
	// ───────────────────────────────────────────────
	originalData, err := bimg.Read(originalFullPath)
	if err != nil {
		if os.IsNotExist(err) {
			http.NotFound(w, r)
		} else {
			log.Printf("read original error: %v path=%s", err, originalFullPath)
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
		return
	}

	img := bimg.NewImage(originalData)

	options := bimg.Options{
		Width:         width,
		Height:        height,
		Quality:       quality,
		StripMetadata: true,
		NoAutoRotate:  false,
		Interlace:     true,
		Gravity:       bimg.GravityCentre,
	}

	switch fit {
	case "contain":
		options.Crop = false
	case "cover", "crop":
		options.Crop = true
	case "scale-down":
		options.Enlarge = false
	default:
		options.Crop = true
	}

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

	processed, err := img.Process(options)
	if err != nil {
		log.Printf("image processing error: %v path=%s options=%+v", err, cleanPath, options)
		http.Error(w, "cannot process image", http.StatusInternalServerError)
		return
	}

	result := processed

	contentType := "image/jpeg"
	switch format {
	case "webp":
		contentType = "image/webp"
	case "avif":
		contentType = "image/avif"
	case "png":
		contentType = "image/png"
	}

	hash := md5.Sum(result)
	etag := fmt.Sprintf("\"%s\"", hex.EncodeToString(hash[:]))

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("Vary", "Accept")
	w.Header().Set("X-Cache", "MISS")

	if match := r.Header.Get("If-None-Match"); match == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	// Cache it
	entry := CacheEntry{
		Body:       result,
		Headers:    w.Header().Clone(),
		ETag:       etag,
		StatusCode: http.StatusOK,
		CreatedAt:  time.Now(),
	}
	cache.Set(cacheKey, entry)

	w.WriteHeader(http.StatusOK)
	w.Write(result)
}
