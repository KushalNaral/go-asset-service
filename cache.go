package main

import (
	"container/list"
	"net/http"
	"strings"
	"sync"
	"time"
)

// CacheEntry is one cached response: an original file, a resized variant, or a remembered 404.
// Body is never modified after Set; the counters are guarded by the cache mutex.
type CacheEntry struct {
	Key     string
	Path    string
	Variant string // "" for originals, e.g. "w=400,h=0,q=85,webp,cover" for transforms

	Status      int // 200, or 404 for a remembered miss
	Body        []byte
	ContentType string
	ETag        string // sent to browsers

	// Validators from the source, used to revalidate once the entry expires.
	UpstreamETag         string
	UpstreamLastModified string
	// For variants: the ETag of the original they were made from.
	SourceETag   string
	OriginalSize int64

	Source  string // origin, disk or transform
	FetchMs int64  // how long the source (or the resize) took

	CreatedAt     time.Time
	ExpiresAt     time.Time
	LastValidated time.Time
	LastAccess    time.Time
	Hits          int64
	Revalidations int64
}

func (e *CacheEntry) size() int64 {
	return int64(len(e.Body) + len(e.Key) + len(e.Path) + 256)
}

func (e *CacheEntry) Fresh(now time.Time) bool { return now.Before(e.ExpiresAt) }

type AssetCache struct {
	mu         sync.Mutex
	ll         *list.List
	items      map[string]*list.Element
	bytes      int64
	maxBytes   int64
	maxEntries int

	evictions int64
	purged    int64
}

func NewAssetCache(config Config) *AssetCache {
	maxEntries := config.CacheSize
	if maxEntries <= 0 {
		maxEntries = 1024
	}
	mb := config.CacheMemoryMB
	if mb <= 0 {
		mb = 512
	}
	return &AssetCache{
		ll:         list.New(),
		items:      map[string]*list.Element{},
		maxBytes:   int64(mb) << 20,
		maxEntries: maxEntries,
	}
}

func originalKey(path string) string { return path }

func variantKey(path, variant string) string { return path + "?" + variant }

// Get returns the entry whether fresh or not; callers decide what to do with a stale one.
func (c *AssetCache) Get(key string) (*CacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		return nil, false
	}
	c.ll.MoveToFront(el)
	return el.Value.(*CacheEntry), true
}

// Touch records that an entry answered a request.
func (c *AssetCache) Touch(e *CacheEntry, now time.Time) {
	c.mu.Lock()
	e.Hits++
	e.LastAccess = now
	c.mu.Unlock()
}

// Renew keeps an entry after the source said it has not changed.
func (c *AssetCache) Renew(e *CacheEntry, now time.Time, ttl time.Duration) {
	c.mu.Lock()
	e.ExpiresAt = now.Add(ttl)
	e.LastValidated = now
	e.Revalidations++
	c.mu.Unlock()
}

// Set stores an entry, keeping the hit count of the one it replaces, and evicts least recently used ones.
func (c *AssetCache) Set(e *CacheEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if el, ok := c.items[e.Key]; ok {
		old := el.Value.(*CacheEntry)
		e.Hits += old.Hits
		e.Revalidations += old.Revalidations
		c.bytes -= old.size()
		el.Value = e
		c.ll.MoveToFront(el)
	} else {
		c.items[e.Key] = c.ll.PushFront(e)
	}
	c.bytes += e.size()

	for (c.bytes > c.maxBytes || c.ll.Len() > c.maxEntries) && c.ll.Len() > 1 {
		c.removeElement(c.ll.Back())
		c.evictions++
	}
}

func (c *AssetCache) removeElement(el *list.Element) {
	e := el.Value.(*CacheEntry)
	c.ll.Remove(el)
	delete(c.items, e.Key)
	c.bytes -= e.size()
}

func (c *AssetCache) Remove(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.removeElement(el)
		c.purged++
		return true
	}
	return false
}

// RemoveWhere drops every entry match accepts and returns how many went.
func (c *AssetCache) RemoveWhere(match func(*CacheEntry) bool) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for el := c.ll.Front(); el != nil; {
		next := el.Next()
		if match(el.Value.(*CacheEntry)) {
			c.removeElement(el)
			n++
		}
		el = next
	}
	c.purged += int64(n)
	return n
}

// RemovePath drops an original together with all of its variants.
func (c *AssetCache) RemovePath(path string) int {
	return c.RemoveWhere(func(e *CacheEntry) bool { return e.Path == path })
}

func (c *AssetCache) RemovePrefix(prefix string) int {
	return c.RemoveWhere(func(e *CacheEntry) bool { return strings.HasPrefix(e.Path, prefix) })
}

func (c *AssetCache) Purge() int {
	return c.RemoveWhere(func(*CacheEntry) bool { return true })
}

func (c *AssetCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}

// EntryInfo is an entry without its body, for the dashboard.
type EntryInfo struct {
	Key           string     `json:"key"`
	Path          string     `json:"path"`
	Variant       string     `json:"variant"`
	Kind          string     `json:"kind"` // original, variant, missing
	Status        int        `json:"status"`
	ContentType   string     `json:"content_type"`
	Bytes         int        `json:"bytes"`
	OriginalBytes int64      `json:"original_bytes,omitempty"`
	Source        string     `json:"source"`
	FetchMs       int64      `json:"fetch_ms"`
	Hits          int64      `json:"hits"`
	Revalidations int64      `json:"revalidations"`
	CreatedAt     time.Time  `json:"created_at"`
	ExpiresAt     time.Time  `json:"expires_at"`
	LastValidated time.Time  `json:"last_validated_at"`
	LastAccess    *time.Time `json:"last_hit_at"`
	Fresh         bool       `json:"fresh"`
	ETag          string     `json:"etag"`
}

func (c *AssetCache) Snapshot(now time.Time) []EntryInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]EntryInfo, 0, c.ll.Len())
	for el := c.ll.Front(); el != nil; el = el.Next() {
		out = append(out, infoOf(el.Value.(*CacheEntry), now))
	}
	return out
}

func infoOf(e *CacheEntry, now time.Time) EntryInfo {
	kind := "original"
	if e.Status == http.StatusNotFound {
		kind = "missing"
	} else if e.Variant != "" {
		kind = "variant"
	}
	info := EntryInfo{
		Key: e.Key, Path: e.Path, Variant: e.Variant, Kind: kind, Status: e.Status,
		ContentType: e.ContentType, Bytes: len(e.Body), OriginalBytes: e.OriginalSize,
		Source: e.Source, FetchMs: e.FetchMs, Hits: e.Hits, Revalidations: e.Revalidations,
		CreatedAt: e.CreatedAt, ExpiresAt: e.ExpiresAt, LastValidated: e.LastValidated,
		Fresh: e.Fresh(now), ETag: e.ETag,
	}
	if !e.LastAccess.IsZero() {
		t := e.LastAccess
		info.LastAccess = &t
	}
	return info
}

type CacheStats struct {
	Entries    int   `json:"entries"`
	MaxEntries int   `json:"max_entries"`
	Bytes      int64 `json:"bytes"`
	MaxBytes   int64 `json:"max_bytes"`
	Originals  int   `json:"originals"`
	Variants   int   `json:"variants"`
	Missing    int   `json:"missing"`
	Stale      int   `json:"stale"`
	Evictions  int64 `json:"evictions"`
	Purged     int64 `json:"purged"`
}

func (c *AssetCache) Stats(now time.Time) CacheStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := CacheStats{
		Entries: c.ll.Len(), MaxEntries: c.maxEntries, Bytes: c.bytes, MaxBytes: c.maxBytes,
		Evictions: c.evictions, Purged: c.purged,
	}
	for el := c.ll.Front(); el != nil; el = el.Next() {
		e := el.Value.(*CacheEntry)
		switch {
		case e.Status == http.StatusNotFound:
			s.Missing++
		case e.Variant != "":
			s.Variants++
		default:
			s.Originals++
		}
		if !e.Fresh(now) {
			s.Stale++
		}
	}
	return s
}
