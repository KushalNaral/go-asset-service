package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

type Config struct {
	Port string

	// Where originals come from. ORIGIN_URL wins over BASE_PATH:
	// with ORIGIN_URL set the service is a caching reverse proxy, otherwise it reads BASE_PATH.
	OriginURL     string
	OriginHost    string // Host header sent to the origin (empty = the origin URL's host)
	OriginTimeout time.Duration
	BasePath      string

	// Memory cache
	CacheSize       int           // max entries
	CacheMemoryMB   int           // max bytes held, in MB
	MaxObjectMB     int           // larger files are streamed through, never cached
	DefaultCacheTTL time.Duration // how long an entry is fresh before it is revalidated
	NegativeTTL     time.Duration // how long a 404 is remembered
	StaleIfError    time.Duration // how long an expired entry may be served while the origin is failing
	BrowserMaxAge   time.Duration // Cache-Control max-age sent to browsers

	// Admin API (dashboard). Empty = admin endpoints disabled.
	AdminKey string

	// Stats survive restarts when set.
	StatsFile string
	// Dashboard switches (cache on/off) survive restarts when set; defaults next to STATS_FILE.
	SettingsFile string

	// Trust X-Real-IP / X-Forwarded-For (true when behind nginx).
	TrustProxy bool
}

func LoadConfig() Config {
	// Try to load .env file (won't fail if missing)
	_ = godotenv.Load() // silent fail - good for production where env vars are set directly

	ttl := getDuration("CACHE_TTL", 0)
	if ttl <= 0 {
		ttl = time.Duration(getInt("CACHE_TTL_HOURS", 24)) * time.Hour
	}

	adminKey := getEnv("ADMIN_KEY", "")
	if adminKey == "" {
		adminKey = getEnv("CLEAR_KEY", "") // legacy name
	}

	statsFile := getEnv("STATS_FILE", "")
	settingsFile := getEnv("SETTINGS_FILE", "")
	if settingsFile == "" && statsFile != "" {
		settingsFile = filepath.Join(filepath.Dir(statsFile), "settings.json")
	}

	return Config{
		Port:          getEnv("PORT", ":8080"),
		OriginURL:     strings.TrimRight(getEnv("ORIGIN_URL", ""), "/"),
		OriginHost:    getEnv("ORIGIN_HOST", ""),
		OriginTimeout: getDuration("ORIGIN_TIMEOUT", 15*time.Second),
		BasePath:      getEnv("BASE_PATH", "/storage"),

		CacheSize:       getInt("CACHE_SIZE", 1024),
		CacheMemoryMB:   getInt("CACHE_MEMORY_MB", 512),
		MaxObjectMB:     getInt("MAX_OBJECT_MB", 20),
		DefaultCacheTTL: ttl,
		NegativeTTL:     getDuration("NEGATIVE_TTL", time.Minute),
		StaleIfError:    getDuration("STALE_IF_ERROR", 24*time.Hour),
		BrowserMaxAge:   getDuration("BROWSER_MAX_AGE", 7*24*time.Hour),

		AdminKey:     adminKey,
		StatsFile:    statsFile,
		SettingsFile: settingsFile,
		TrustProxy:   getEnv("TRUST_PROXY", "true") != "false",
	}
}

// SourceName is how the dashboard labels where originals come from.
func (c Config) SourceName() string {
	if c.OriginURL != "" {
		return "origin"
	}
	return "disk"
}

func getEnv(key, fallback string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return fallback
}

// getInt returns fallback when the value is missing, invalid or not positive.
func getInt(key string, fallback int) int {
	if n, err := strconv.Atoi(os.Getenv(key)); err == nil && n > 0 {
		return n
	}
	return fallback
}

// getDuration accepts Go durations ("90s", "12h") or plain seconds.
func getDuration(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return d
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	return fallback
}
