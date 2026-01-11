package main

import (
	"os"
	"strconv"
	"time"

	"github.com/joho/godotenv"
)

type Config struct {
	Port            string
	BasePath        string
	CacheSize       int
	DefaultCacheTTL time.Duration
}

func LoadConfig() Config {
	// Try to load .env file (won't fail if missing)
	_ = godotenv.Load() // silent fail - good for production where env vars are set directly

	cacheSize := 1024 // default
	if v := os.Getenv("CACHE_SIZE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cacheSize = n
		}
	}

	ttl := 24 * time.Hour
	if v := os.Getenv("CACHE_TTL_HOURS"); v != "" {
		if h, err := strconv.Atoi(v); err == nil && h > 0 {
			ttl = time.Duration(h) * time.Hour
		}
	}

	port := getEnv("PORT", ":8080")
	basePath := getEnv("BASE_PATH", "/home/rome/work/ecom/api/storage/app/public")

	return Config{
		Port:            port,
		BasePath:        basePath,
		CacheSize:       cacheSize,
		DefaultCacheTTL: ttl,
	}
}

func getEnv(key, fallback string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return fallback
}
