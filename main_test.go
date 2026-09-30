package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var jpegBytes = append(append([]byte{
	0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 0x4A, 0x46, 0x49, 0x46, 0x00, 0x01,
	0x01, 0x01, 0x00, 0x48, 0x00, 0x48, 0x00, 0x00,
}, bytes.Repeat([]byte{0xAA}, 100)...), 0xFF, 0xD9)

// fakeOrigin is a stand-in for the production /storage.
type fakeOrigin struct {
	mu      sync.Mutex
	files   map[string][]byte
	fail    bool
	fetches atomic.Int64
	delay   time.Duration
}

func (o *fakeOrigin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	o.fetches.Add(1)
	if o.delay > 0 {
		time.Sleep(o.delay)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.fail {
		http.Error(w, "boom", http.StatusBadGateway)
		return
	}
	body, ok := o.files[strings.TrimPrefix(r.URL.Path, "/storage/")]
	if !ok {
		http.NotFound(w, r)
		return
	}
	etag := etagOf(body)
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	http.ServeContent(w, r, r.URL.Path, time.Unix(1700000000, 0), bytes.NewReader(body))
}

func newTestServer(t *testing.T, cfg Config) (*Server, *fakeOrigin, *time.Time) {
	t.Helper()
	origin := &fakeOrigin{files: map[string][]byte{"products/shoe.jpg": jpegBytes}}
	upstream := httptest.NewServer(origin)
	t.Cleanup(upstream.Close)

	cfg.OriginURL = upstream.URL + "/storage"
	cfg.OriginTimeout = 5 * time.Second
	if cfg.DefaultCacheTTL == 0 {
		cfg.DefaultCacheTTL = time.Hour
	}
	if cfg.NegativeTTL == 0 {
		cfg.NegativeTTL = time.Minute
	}
	if cfg.StaleIfError == 0 {
		cfg.StaleIfError = 24 * time.Hour
	}
	if cfg.BrowserMaxAge == 0 {
		cfg.BrowserMaxAge = time.Hour
	}
	if cfg.MaxObjectMB == 0 {
		cfg.MaxObjectMB = 1
	}
	cfg.CacheSize, cfg.CacheMemoryMB, cfg.AdminKey, cfg.TrustProxy = 100, 16, "test-key", true

	s := NewServer(cfg, NewSource(cfg))
	now := time.Now()
	s.now = func() time.Time { return now }
	return s, origin, &now
}

func get(t *testing.T, h http.Handler, path string, headers ...string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w.Result()
}

func TestMissThenHit(t *testing.T) {
	s, origin, _ := newTestServer(t, Config{})
	h := s.Routes()

	r1 := get(t, h, "/asset/products/shoe.jpg")
	body, _ := io.ReadAll(r1.Body)
	if r1.StatusCode != 200 || r1.Header.Get("X-Cache") != OutcomeMiss || !bytes.Equal(body, jpegBytes) {
		t.Fatalf("first: status %d cache %q len %d", r1.StatusCode, r1.Header.Get("X-Cache"), len(body))
	}
	if ct := r1.Header.Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("content type %q", ct)
	}

	r2 := get(t, h, "/asset/products/shoe.jpg")
	if r2.Header.Get("X-Cache") != OutcomeHit {
		t.Fatalf("second: cache %q", r2.Header.Get("X-Cache"))
	}
	if origin.fetches.Load() != 1 {
		t.Errorf("origin fetched %d times, want 1", origin.fetches.Load())
	}

	// Browser revalidation is answered from memory.
	r3 := get(t, h, "/asset/products/shoe.jpg", "If-None-Match", r1.Header.Get("ETag"))
	if r3.StatusCode != http.StatusNotModified {
		t.Errorf("conditional: status %d", r3.StatusCode)
	}

	// Range requests are answered from memory too.
	r4 := get(t, h, "/asset/products/shoe.jpg", "Range", "bytes=0-9")
	if r4.StatusCode != http.StatusPartialContent {
		t.Errorf("range: status %d", r4.StatusCode)
	}
}

func TestRevalidateAfterExpiry(t *testing.T) {
	s, origin, now := newTestServer(t, Config{DefaultCacheTTL: time.Minute})
	h := s.Routes()
	get(t, h, "/asset/products/shoe.jpg")

	*now = now.Add(2 * time.Minute)
	r := get(t, h, "/asset/products/shoe.jpg")
	if r.Header.Get("X-Cache") != OutcomeRevalidated {
		t.Fatalf("after expiry: %q", r.Header.Get("X-Cache"))
	}
	if origin.fetches.Load() != 2 {
		t.Errorf("fetches %d, want 2 (one conditional)", origin.fetches.Load())
	}

	// A changed file replaces the cached copy.
	origin.mu.Lock()
	origin.files["products/shoe.jpg"] = append([]byte{0xFF, 0xD8, 0xFF}, bytes.Repeat([]byte{1}, 50)...)
	origin.mu.Unlock()
	*now = now.Add(2 * time.Minute)
	r = get(t, h, "/asset/products/shoe.jpg")
	body, _ := io.ReadAll(r.Body)
	if r.Header.Get("X-Cache") != OutcomeMiss || len(body) != 53 {
		t.Fatalf("changed file: %q len %d", r.Header.Get("X-Cache"), len(body))
	}
}

func TestServeStaleWhenOriginFails(t *testing.T) {
	s, origin, now := newTestServer(t, Config{DefaultCacheTTL: time.Minute})
	h := s.Routes()
	get(t, h, "/asset/products/shoe.jpg")

	origin.mu.Lock()
	origin.fail = true
	origin.mu.Unlock()
	*now = now.Add(2 * time.Minute)

	r := get(t, h, "/asset/products/shoe.jpg")
	if r.StatusCode != 200 || r.Header.Get("X-Cache") != OutcomeStale {
		t.Fatalf("stale: status %d cache %q", r.StatusCode, r.Header.Get("X-Cache"))
	}

	r = get(t, h, "/asset/products/other.jpg")
	if r.StatusCode != http.StatusBadGateway {
		t.Fatalf("uncached with failing origin: status %d", r.StatusCode)
	}
	if s.stats.Totals.OriginErrors == 0 || s.stats.LastOriginError == "" {
		t.Error("origin error not recorded")
	}
}

func TestNotFoundIsRemembered(t *testing.T) {
	s, origin, now := newTestServer(t, Config{})
	h := s.Routes()

	for i := 0; i < 3; i++ {
		if r := get(t, h, "/asset/nope.jpg", "Referer", "https://newmadanfurnishers.com/product/sofa"); r.StatusCode != 404 {
			t.Fatalf("status %d", r.StatusCode)
		}
	}
	if origin.fetches.Load() != 1 {
		t.Errorf("fetches %d, want 1", origin.fetches.Load())
	}
	*now = now.Add(2 * time.Minute)
	get(t, h, "/asset/nope.jpg")
	if origin.fetches.Load() != 2 {
		t.Errorf("after negative TTL: fetches %d, want 2", origin.fetches.Load())
	}

	p := s.stats.Paths["nope.jpg"]
	if p == nil || p.NotFound != 4 || p.Referers["newmadanfurnishers.com/product/sofa"] != 3 {
		t.Fatalf("path stat %+v", p)
	}
}

func TestConcurrentMissesShareOneFetch(t *testing.T) {
	s, origin, _ := newTestServer(t, Config{})
	origin.delay = 50 * time.Millisecond
	h := s.Routes()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if r := get(t, h, "/asset/products/shoe.jpg"); r.StatusCode != 200 {
				t.Errorf("status %d", r.StatusCode)
			}
		}()
	}
	wg.Wait()
	if n := origin.fetches.Load(); n != 1 {
		t.Errorf("origin fetched %d times, want 1", n)
	}
}

func TestLargeFilesStreamThrough(t *testing.T) {
	s, origin, _ := newTestServer(t, Config{MaxObjectMB: 1})
	big := bytes.Repeat([]byte("v"), 2<<20)
	origin.files["videos/intro.mp4"] = big
	h := s.Routes()

	r := get(t, h, "/asset/videos/intro.mp4")
	body, _ := io.ReadAll(r.Body)
	if r.Header.Get("X-Cache") != OutcomeBypass || len(body) != len(big) {
		t.Fatalf("big: cache %q len %d", r.Header.Get("X-Cache"), len(body))
	}
	if r.Header.Get("Content-Type") != "video/mp4" {
		t.Errorf("content type %q", r.Header.Get("Content-Type"))
	}

	r = get(t, h, "/asset/videos/intro.mp4", "Range", "bytes=100-199")
	body, _ = io.ReadAll(r.Body)
	if r.StatusCode != http.StatusPartialContent || len(body) != 100 || r.Header.Get("Content-Range") == "" {
		t.Fatalf("range: status %d len %d", r.StatusCode, len(body))
	}
	if s.cache.Len() != 0 {
		t.Errorf("big file was cached")
	}
}

func TestInvalidPaths(t *testing.T) {
	s, _, _ := newTestServer(t, Config{})
	h := s.Routes()
	for _, p := range []string{"/asset/", "/asset/../secret.txt", "/asset/a/..%2F..%2Fetc/passwd"} {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.URL.Path = strings.ReplaceAll(p, "%2F", "/")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest && w.Code != http.StatusNotFound && w.Code != http.StatusMovedPermanently && w.Code != http.StatusTemporaryRedirect {
			t.Errorf("%s: status %d", p, w.Code)
		}
	}
	if _, ok := cleanAssetPath("/asset/../x"); ok {
		t.Error("traversal accepted")
	}
}

func TestDiskSource(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "products"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "products", "shoe.jpg"), jpegBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := Config{BasePath: dir, CacheSize: 10, CacheMemoryMB: 1, MaxObjectMB: 1, DefaultCacheTTL: time.Minute, NegativeTTL: time.Minute, BrowserMaxAge: time.Hour}
	s := NewServer(cfg, NewSource(cfg))
	h := s.Routes()

	r := get(t, h, "/asset/products/shoe.jpg")
	if r.StatusCode != 200 || r.Header.Get("X-Cache") != OutcomeMiss {
		t.Fatalf("disk: status %d cache %q", r.StatusCode, r.Header.Get("X-Cache"))
	}
	if r := get(t, h, "/asset/products"); r.StatusCode != 404 {
		t.Errorf("directory: status %d", r.StatusCode)
	}
	if start, end, ok := parseSingleRange("bytes=-10", 100); !ok || start != 90 || end != 99 {
		t.Errorf("suffix range %d-%d %v", start, end, ok)
	}
}

func TestAdminAPI(t *testing.T) {
	s, _, _ := newTestServer(t, Config{})
	h := s.Routes()
	get(t, h, "/asset/products/shoe.jpg", "Referer", "https://newmadanfurnishers.com/", "User-Agent",
		"Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1",
		"X-Real-IP", "27.34.1.2")
	get(t, h, "/asset/products/shoe.jpg", "User-Agent", "facebookexternalhit/1.1")

	if r := get(t, h, "/admin/overview"); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no key: status %d", r.StatusCode)
	}

	r := get(t, h, "/admin/overview", "X-Admin-Key", "test-key")
	if r.StatusCode != 200 {
		t.Fatalf("overview: status %d", r.StatusCode)
	}
	var ov struct {
		Totals struct {
			Requests int64   `json:"requests"`
			HitRatio float64 `json:"hit_ratio"`
		} `json:"totals"`
		Cache CacheStats `json:"cache"`
		Top   struct {
			Devices []CountItem `json:"devices"`
			Bots    []CountItem `json:"bots"`
			Clients []CountItem `json:"clients"`
		} `json:"top"`
	}
	if err := json.NewDecoder(r.Body).Decode(&ov); err != nil {
		t.Fatal(err)
	}
	if ov.Totals.Requests != 2 || ov.Totals.HitRatio != 0.5 || ov.Cache.Originals != 1 {
		t.Errorf("overview %+v", ov)
	}
	if len(ov.Top.Bots) != 1 || ov.Top.Bots[0].Key != "Facebook preview" {
		t.Errorf("bots %+v", ov.Top.Bots)
	}
	if !hasKey(ov.Top.Clients, "27.34.1.2") {
		t.Errorf("clients %+v", ov.Top.Clients)
	}

	for _, path := range []string{"/admin/entries", "/admin/paths?status=cached", "/admin/path?path=products/shoe.jpg", "/admin/requests"} {
		if r := get(t, h, path, "Authorization", "Bearer test-key"); r.StatusCode != 200 {
			t.Errorf("%s: status %d", path, r.StatusCode)
		}
	}

	req := httptest.NewRequest(http.MethodPost, "/admin/purge", strings.NewReader(`{"path":"products/shoe.jpg"}`))
	req.Header.Set("X-Admin-Key", "test-key")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"removed":1`) || s.cache.Len() != 0 {
		t.Fatalf("purge: %d %s", w.Code, w.Body.String())
	}

	req = httptest.NewRequest(http.MethodPost, "/admin/warm", strings.NewReader(`{"paths":["/storage/products/shoe.jpg"]}`))
	req.Header.Set("X-Admin-Key", "test-key")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 || s.cache.Len() != 1 {
		t.Fatalf("warm: %d %s", w.Code, w.Body.String())
	}
}

func TestStatsPersist(t *testing.T) {
	s, _, _ := newTestServer(t, Config{})
	h := s.Routes()
	get(t, h, "/asset/products/shoe.jpg")
	get(t, h, "/asset/products/shoe.jpg")

	file := filepath.Join(t.TempDir(), "stats.json")
	if err := s.stats.Save(file); err != nil {
		t.Fatal(err)
	}
	restored := NewStats(time.Now())
	restored.Load(file)
	if restored.Totals.Requests != 2 || restored.Paths["products/shoe.jpg"].Hits != 1 || len(restored.Recent) != 2 {
		t.Fatalf("restored %+v", restored.Totals)
	}
}

func TestCacheEvictsByBytes(t *testing.T) {
	c := NewAssetCache(Config{CacheSize: 100, CacheMemoryMB: 1})
	for i := 0; i < 5; i++ {
		c.Set(&CacheEntry{Key: fmt.Sprint(i), Path: fmt.Sprint(i), Status: 200, Body: make([]byte, 300<<10)})
	}
	if n := c.Len(); n != 3 {
		t.Errorf("entries %d, want 3", n)
	}
	if _, ok := c.Get("0"); ok {
		t.Error("oldest entry survived")
	}
}

func TestClassifyUA(t *testing.T) {
	cases := map[string][2]string{
		"Mozilla/5.0 (Linux; Android 14; SM-S918B) AppleWebKit/537.36 (KHTML, like Gecko) SamsungBrowser/25.0 Chrome/121.0 Mobile Safari/537.36": {"mobile", "Samsung Internet"},
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0 Safari/537.36 Edg/128.0":                  {"desktop", "Edge"},
		"Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)":                                                               {"bot", "Googlebot"},
		"Mozilla/5.0 (iPad; CPU OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1":         {"tablet", "Safari"},
		"": {"bot", "No user agent"},
	}
	for ua, want := range cases {
		if d, b := classifyUA(ua); d != want[0] || b != want[1] {
			t.Errorf("%q → %s/%s, want %s/%s", ua, d, b, want[0], want[1])
		}
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

func hasKey(items []CountItem, key string) bool {
	for _, it := range items {
		if it.Key == key {
			return true
		}
	}
	return false
}

func TestSwitchOffGoesStraightToSource(t *testing.T) {
	s, origin, _ := newTestServer(t, Config{})
	s.cfg.SettingsFile = filepath.Join(t.TempDir(), "settings.json")
	h := s.Routes()

	req := httptest.NewRequest(http.MethodPost, "/admin/settings", strings.NewReader(`{"enabled":false,"purge":true}`))
	req.Header.Set("X-Admin-Key", "test-key")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("settings: %d %s", w.Code, w.Body.String())
	}

	for i := 0; i < 2; i++ {
		r := get(t, h, "/asset/products/shoe.jpg")
		if r.StatusCode != 200 || r.Header.Get("X-Cache") != OutcomeBypass {
			t.Fatalf("off: status %d cache %q", r.StatusCode, r.Header.Get("X-Cache"))
		}
	}
	if origin.fetches.Load() != 2 || s.cache.Len() != 0 {
		t.Errorf("fetches %d entries %d", origin.fetches.Load(), s.cache.Len())
	}

	// The switch survives a restart.
	s2 := NewServer(s.cfg, s.source)
	s2.loadSettings()
	if s2.enabled.Load() {
		t.Error("switch not restored")
	}

	r := get(t, h, "/admin/health", "X-Admin-Key", "test-key")
	var health struct {
		Status  string `json:"status"`
		Enabled bool   `json:"enabled"`
	}
	json.NewDecoder(r.Body).Decode(&health)
	if r.StatusCode != 200 || health.Status != "degraded" || health.Enabled {
		t.Errorf("health %d %+v", r.StatusCode, health)
	}
}
