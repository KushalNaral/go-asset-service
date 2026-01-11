package main

import (
	"os"
	"testing"
	"time"
)

func TestLoadConfig_FromEnv(t *testing.T) {
	os.Clearenv()
	t.Setenv("PORT", ":9000")
	t.Setenv("BASE_PATH", "/test/storage")
	t.Setenv("CACHE_SIZE", "512")
	t.Setenv("CACHE_TTL_HOURS", "6")

	cfg := LoadConfig()

	if cfg.Port != ":9000" {
		t.Errorf("port = %s, want :9000", cfg.Port)
	}
	if cfg.BasePath != "/test/storage" {
		t.Errorf("base path = %s, want /test/storage", cfg.BasePath)
	}
	if cfg.CacheSize != 512 {
		t.Errorf("cache size = %d, want 512", cfg.CacheSize)
	}
	if cfg.DefaultCacheTTL != 6*time.Hour {
		t.Errorf("ttl = %v, want 6h", cfg.DefaultCacheTTL)
	}
}

func TestLoadConfig_InvalidValues(t *testing.T) {
	os.Clearenv()
	t.Setenv("CACHE_SIZE", "invalid")
	t.Setenv("CACHE_TTL_HOURS", "-5")

	cfg := LoadConfig()

	// Should fall back to defaults when invalid
	if cfg.CacheSize != 1024 {
		t.Errorf("expected default cache size on invalid input, got %d", cfg.CacheSize)
	}
	if cfg.DefaultCacheTTL != 24*time.Hour {
		t.Errorf("expected default TTL on invalid input, got %v", cfg.DefaultCacheTTL)
	}
}
