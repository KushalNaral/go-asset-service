package main

import (
	"crypto/md5"
	"encoding/hex"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
)

type CacheEntry struct {
	Body       []byte
	Headers    http.Header
	ETag       string
	StatusCode int
	CreatedAt  time.Time
}

type AssetCache struct {
	lru    *lru.Cache[string, CacheEntry]
	ttl    time.Duration
	config Config
}

func NewAssetCache(config Config) *AssetCache {
	cache, err := lru.New[string, CacheEntry](config.CacheSize)
	if err != nil {
		panic(err)
	}

	return &AssetCache{
		lru:    cache,
		ttl:    config.DefaultCacheTTL,
		config: config,
	}
}

func (c *AssetCache) cacheKey(r *http.Request, cleanPath string) string {
	// Deterministic key: path + sorted query params
	q := r.URL.Query()
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var sb strings.Builder
	sb.WriteString("v1:") // bump version to invalidate all cache if needed
	sb.WriteString(cleanPath)
	for _, k := range keys {
		sb.WriteString("|")
		sb.WriteString(k)
		sb.WriteString("=")
		sb.WriteString(q.Get(k))
	}

	hash := md5.Sum([]byte(sb.String()))
	return hex.EncodeToString(hash[:])
}

func (c *AssetCache) Get(key string) (CacheEntry, bool) {
	entry, ok := c.lru.Get(key)
	if !ok || time.Since(entry.CreatedAt) > c.ttl {
		if ok {
			c.lru.Remove(key)
		}
		return CacheEntry{}, false
	}
	return entry, true
}

func (c *AssetCache) Set(key string, entry CacheEntry) {
	c.lru.Add(key, entry)
}

func (c *AssetCache) cacheKeyWithTransform(r *http.Request, cleanPath string, w, h, q int, format, fit string) string {
	var sb strings.Builder
	sb.WriteString("v2:") // version bump when changing logic
	sb.WriteString(cleanPath)
	sb.WriteString("|w=")
	sb.WriteString(strconv.Itoa(w))
	sb.WriteString("|h=")
	sb.WriteString(strconv.Itoa(h))
	sb.WriteString("|q=")
	sb.WriteString(strconv.Itoa(q))
	sb.WriteString("|fmt=")
	sb.WriteString(format)
	sb.WriteString("|fit=")
	sb.WriteString(fit)

	// Optional: include Accept header for future auto-format
	// sb.WriteString("|accept=" + r.Header.Get("Accept"))

	hash := md5.Sum([]byte(sb.String()))
	return hex.EncodeToString(hash[:])
}
