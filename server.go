package main

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var errTooLarge = errors.New("file is larger than MAX_OBJECT_MB")

// Server ties the source, the memory cache and the stats together.
type Server struct {
	cfg    Config
	cache  *AssetCache
	stats  *Stats
	source Source
	flight flightGroup
	start  time.Time
	now    func() time.Time

	// Switched from the dashboard; when off every request goes to the source.
	enabled atomic.Bool

	// Paths known to be too large to cache go straight to streaming.
	tooLargeMu sync.Mutex
	tooLarge   map[string]time.Time
}

func NewServer(cfg Config, source Source) *Server {
	s := &Server{
		cfg:      cfg,
		cache:    NewAssetCache(cfg),
		stats:    NewStats(time.Now()),
		source:   source,
		start:    time.Now(),
		now:      time.Now,
		tooLarge: map[string]time.Time{},
	}
	s.enabled.Store(true)
	return s
}

func (s *Server) maxObject() int64 { return int64(s.cfg.MaxObjectMB) << 20 }

// cleanAssetPath turns "/asset/products/1/a.jpg" into "products/1/a.jpg", refusing traversal.
func cleanAssetPath(urlPath string) (string, bool) {
	raw := strings.TrimPrefix(urlPath, "/asset")
	raw = strings.TrimPrefix(raw, "/")
	if raw == "" || strings.Contains(raw, "..") || strings.Contains(raw, "\\") || strings.ContainsRune(raw, 0) {
		return "", false
	}
	clean := filepath.ToSlash(filepath.Clean(raw))
	if clean == "." || strings.HasPrefix(clean, "/") {
		return "", false
	}
	return clean, true
}

func (s *Server) handleAsset(w http.ResponseWriter, r *http.Request) {
	path, ok := cleanAssetPath(r.URL.Path)
	if !ok {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}

	started := s.now()
	cw := &countingWriter{ResponseWriter: w}
	t, isTransform := parseTransform(r.URL.Query(), path)

	var outcome, contentType string
	switch {
	case !s.enabled.Load() && isTransform && transformEnabled:
		outcome, contentType = s.serveUncachedVariant(cw, r, path, t)
	case !s.enabled.Load():
		outcome, contentType = s.stream(cw, r, path)
	case isTransform && !transformEnabled:
		// No libvips in this build: fall back to the original.
		outcome, contentType = s.serveOriginal(cw, r, path)
	case isTransform:
		outcome, contentType = s.serveVariant(cw, r, path, t)
	default:
		outcome, contentType = s.serveOriginal(cw, r, path)
	}

	ua := r.UserAgent()
	device, browser := classifyUA(ua)
	rec := RequestRecord{
		Time:       started,
		Path:       path,
		Outcome:    outcome,
		Status:     cw.statusCode(),
		Bytes:      cw.bytes,
		DurationMs: float64(time.Since(started).Microseconds()) / 1000,
		ClientIP:   s.clientIP(r),
		Referer:    r.Referer(),
		UserAgent:  truncate(ua, 300),
		Device:     device,
		Browser:    browser,
	}
	if isTransform && transformEnabled {
		rec.Variant = t.Key()
	}
	s.stats.Record(rec, contentType)
}

// ─── Originals ──────────────────────────────────────────────────

func (s *Server) serveOriginal(w http.ResponseWriter, r *http.Request, path string) (outcome, contentType string) {
	if s.isTooLarge(path) {
		return s.stream(w, r, path)
	}

	e, outcome, err := s.getOriginal(path)
	switch {
	case errors.Is(err, errTooLarge):
		return s.stream(w, r, path)
	case err != nil:
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
		return OutcomeError, ""
	}
	s.writeEntry(w, r, e, outcome)
	return outcome, e.ContentType
}

// getOriginal returns the cached original, fetching or revalidating it when needed.
func (s *Server) getOriginal(path string) (*CacheEntry, string, error) {
	key := originalKey(path)
	if e, ok := s.cache.Get(key); ok && e.Fresh(s.now()) {
		if e.Status == http.StatusNotFound {
			return e, OutcomeNotFound, nil
		}
		return e, OutcomeHit, nil
	}
	res := s.flight.Do(key, func() flightResult { return s.refreshOriginal(path) })
	return res.entry, res.outcome, res.err
}

func (s *Server) refreshOriginal(path string) flightResult {
	key := originalKey(path)
	old, _ := s.cache.Get(key)
	now := s.now()
	if old != nil && old.Fresh(now) { // another request refreshed it meanwhile
		if old.Status == http.StatusNotFound {
			return flightResult{old, OutcomeNotFound, nil}
		}
		return flightResult{old, OutcomeHit, nil}
	}

	fr := FetchRequest{Path: path}
	if old != nil && old.Status == http.StatusOK {
		fr.ETag, fr.LastModified = old.UpstreamETag, old.UpstreamLastModified
	}

	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.OriginTimeout+5*time.Second)
	defer cancel()
	t0 := time.Now()
	res, err := s.source.Fetch(ctx, fr)
	var body []byte
	if err == nil && res.Status == http.StatusOK {
		if res.Size > s.maxObject() {
			res.Close()
			s.markTooLarge(path)
			s.stats.RecordOrigin(res.Status, 0, time.Since(t0).Milliseconds(), nil, now)
			return flightResult{nil, "", errTooLarge}
		}
		body, err = io.ReadAll(io.LimitReader(res.Body, s.maxObject()+1))
		res.Close()
		if err == nil && int64(len(body)) > s.maxObject() {
			s.markTooLarge(path)
			s.stats.RecordOrigin(res.Status, int64(len(body)), time.Since(t0).Milliseconds(), nil, now)
			return flightResult{nil, "", errTooLarge}
		}
	}
	fetchMs := time.Since(t0).Milliseconds()
	status := 0
	if res != nil {
		status = res.Status
	}
	s.stats.RecordOrigin(status, int64(len(body)), fetchMs, err, now)

	if err == nil && status != http.StatusOK && status != http.StatusNotModified && status != http.StatusNotFound {
		err = fmt.Errorf("source answered %d", status)
	}
	if err != nil {
		log.Printf("fetch %s: %v", path, err)
		if old != nil && old.Status == http.StatusOK && now.Before(old.ExpiresAt.Add(s.cfg.StaleIfError)) {
			return flightResult{old, OutcomeStale, nil}
		}
		return flightResult{nil, OutcomeError, err}
	}

	switch status {
	case http.StatusNotModified:
		if old == nil || old.Status != http.StatusOK {
			return flightResult{nil, OutcomeError, errors.New("source answered 304 without a cached copy")}
		}
		s.cache.Renew(old, now, s.cfg.DefaultCacheTTL)
		return flightResult{old, OutcomeRevalidated, nil}

	case http.StatusNotFound:
		s.cache.RemovePath(path) // variants of a deleted file go too
		e := &CacheEntry{
			Key: key, Path: path, Status: http.StatusNotFound, Source: s.cfg.SourceName(), FetchMs: fetchMs,
			CreatedAt: now, ExpiresAt: now.Add(s.cfg.NegativeTTL), LastValidated: now,
		}
		s.cache.Set(e)
		return flightResult{e, OutcomeNotFound, nil}
	}

	e := &CacheEntry{
		Key: key, Path: path, Status: http.StatusOK, Body: body,
		ContentType:          contentTypeFor(path, res.ContentType, body),
		ETag:                 etagOf(body),
		UpstreamETag:         res.ETag,
		UpstreamLastModified: res.LastModified,
		Source:               s.cfg.SourceName(), FetchMs: fetchMs,
		CreatedAt: now, ExpiresAt: now.Add(s.cfg.DefaultCacheTTL), LastValidated: now,
	}
	s.cache.Set(e)
	return flightResult{e, OutcomeMiss, nil}
}

// ─── Variants (resized) ─────────────────────────────────────────

func (s *Server) serveVariant(w http.ResponseWriter, r *http.Request, path string, t TransformOptions) (outcome, contentType string) {
	key := variantKey(path, t.Key())
	if e, ok := s.cache.Get(key); ok && e.Fresh(s.now()) {
		s.writeEntry(w, r, e, OutcomeHit)
		return OutcomeHit, e.ContentType
	}

	res := s.flight.Do(key, func() flightResult { return s.refreshVariant(path, key, t) })
	switch {
	case errors.Is(res.err, errTooLarge):
		http.Error(w, "image too large to resize", http.StatusUnprocessableEntity)
		return OutcomeError, ""
	case errors.Is(res.err, errTransform):
		http.Error(w, "cannot process image", http.StatusInternalServerError)
		return OutcomeError, ""
	case res.err != nil:
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
		return OutcomeError, ""
	}
	s.writeEntry(w, r, res.entry, res.outcome)
	return res.outcome, res.entry.ContentType
}

var errTransform = errors.New("cannot process image")

// serveUncachedVariant resizes straight from the source while the cache is switched off.
func (s *Server) serveUncachedVariant(w http.ResponseWriter, r *http.Request, path string, t TransformOptions) (outcome, contentType string) {
	now := s.now()
	w.Header().Set("X-Cache", OutcomeBypass)
	t0 := time.Now()
	res, err := s.source.Fetch(r.Context(), FetchRequest{Path: path})
	var body []byte
	if err == nil && res.Status == http.StatusOK {
		body, err = io.ReadAll(io.LimitReader(res.Body, s.maxObject()+1))
		res.Close()
	}
	status := 0
	if res != nil {
		status = res.Status
	}
	s.stats.RecordOrigin(status, int64(len(body)), time.Since(t0).Milliseconds(), err, now)
	switch {
	case err != nil:
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
		return OutcomeError, ""
	case status == http.StatusNotFound:
		http.Error(w, "not found", http.StatusNotFound)
		return OutcomeNotFound, ""
	case int64(len(body)) > s.maxObject():
		http.Error(w, "image too large to resize", http.StatusUnprocessableEntity)
		return OutcomeError, ""
	}

	t1 := time.Now()
	out, err := transformImage(body, t)
	s.stats.RecordTransform(time.Since(t1).Milliseconds(), err)
	if err != nil {
		http.Error(w, "cannot process image", http.StatusInternalServerError)
		return OutcomeError, ""
	}
	e := &CacheEntry{Status: http.StatusOK, Body: out, ContentType: t.ContentType(), ETag: etagOf(out), CreatedAt: now, LastValidated: now}
	s.writeEntry(w, r, e, OutcomeBypass)
	return OutcomeBypass, e.ContentType
}

func (s *Server) refreshVariant(path, key string, t TransformOptions) flightResult {
	old, _ := s.cache.Get(key)
	now := s.now()
	if old != nil && old.Fresh(now) {
		return flightResult{old, OutcomeHit, nil}
	}

	orig, origOutcome, err := s.getOriginal(path)
	if err != nil {
		if old != nil && now.Before(old.ExpiresAt.Add(s.cfg.StaleIfError)) {
			return flightResult{old, OutcomeStale, nil}
		}
		return flightResult{nil, OutcomeError, err}
	}
	if orig.Status == http.StatusNotFound {
		return flightResult{orig, OutcomeNotFound, nil}
	}
	if old != nil && old.SourceETag == orig.ETag {
		// The original has not changed: the resized copy is still right.
		s.cache.Renew(old, now, s.cfg.DefaultCacheTTL)
		return flightResult{old, OutcomeRevalidated, nil}
	}
	if origOutcome == OutcomeStale && old != nil {
		return flightResult{old, OutcomeStale, nil}
	}

	t0 := time.Now()
	out, err := transformImage(orig.Body, t)
	ms := time.Since(t0).Milliseconds()
	s.stats.RecordTransform(ms, err)
	if err != nil {
		log.Printf("transform %s %s: %v", path, t.Key(), err)
		return flightResult{nil, OutcomeError, errTransform}
	}

	e := &CacheEntry{
		Key: key, Path: path, Variant: t.Key(), Status: http.StatusOK, Body: out,
		ContentType: t.ContentType(), ETag: etagOf(out),
		UpstreamLastModified: orig.UpstreamLastModified,
		SourceETag:           orig.ETag, OriginalSize: int64(len(orig.Body)),
		Source: "transform", FetchMs: ms,
		CreatedAt: now, ExpiresAt: orig.ExpiresAt, LastValidated: now,
	}
	s.cache.Set(e)
	return flightResult{e, OutcomeMiss, nil}
}

// ─── Writing responses ──────────────────────────────────────────

func (s *Server) writeEntry(w http.ResponseWriter, r *http.Request, e *CacheEntry, outcome string) {
	now := s.now()
	s.cache.Touch(e, now)
	h := w.Header()
	h.Set("X-Cache", outcome)
	h.Set("Access-Control-Allow-Origin", "*")

	if e.Status == http.StatusNotFound {
		h.Set("Cache-Control", fmt.Sprintf("public, max-age=%d", int(s.cfg.NegativeTTL.Seconds())))
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	h.Set("Content-Type", e.ContentType)
	h.Set("ETag", e.ETag)
	h.Set("Cache-Control", fmt.Sprintf("public, max-age=%d", int(s.cfg.BrowserMaxAge.Seconds())))
	h.Set("Age", strconv.Itoa(int(now.Sub(e.LastValidated).Seconds())))

	modTime := e.CreatedAt
	if t, err := http.ParseTime(e.UpstreamLastModified); err == nil {
		modTime = t
	}
	// ServeContent answers Range, If-None-Match, If-Modified-Since and HEAD.
	http.ServeContent(w, r, "", modTime, bytes.NewReader(e.Body))
}

// stream passes a large file through without caching it, forwarding Range for video seeking.
func (s *Server) stream(w http.ResponseWriter, r *http.Request, path string) (outcome, contentType string) {
	now := s.now()
	t0 := time.Now()
	res, err := s.source.Fetch(r.Context(), FetchRequest{
		Path:         path,
		Range:        r.Header.Get("Range"),
		ETag:         r.Header.Get("If-None-Match"),
		LastModified: r.Header.Get("If-Modified-Since"),
	})
	status := 0
	if res != nil {
		status = res.Status
	}
	s.stats.RecordOrigin(status, 0, time.Since(t0).Milliseconds(), err, now)
	h := w.Header()
	h.Set("X-Cache", OutcomeBypass)
	if err != nil {
		http.Error(w, "upstream unavailable", http.StatusBadGateway)
		return OutcomeError, ""
	}
	defer res.Close()

	switch res.Status {
	case http.StatusNotFound:
		s.unmarkTooLarge(path)
		http.Error(w, "not found", http.StatusNotFound)
		return OutcomeNotFound, ""
	case http.StatusNotModified:
		w.WriteHeader(http.StatusNotModified)
		return OutcomeBypass, ""
	}

	contentType = contentTypeFor(path, res.ContentType, nil)
	h.Set("Content-Type", contentType)
	h.Set("Accept-Ranges", "bytes")
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Cache-Control", fmt.Sprintf("public, max-age=%d", int(s.cfg.BrowserMaxAge.Seconds())))
	if res.ETag != "" {
		h.Set("ETag", res.ETag)
	}
	if res.LastModified != "" {
		h.Set("Last-Modified", res.LastModified)
	}
	if res.ContentRange != "" {
		h.Set("Content-Range", res.ContentRange)
	}
	if res.Size >= 0 {
		h.Set("Content-Length", strconv.FormatInt(res.Size, 10))
	}
	w.WriteHeader(res.Status)
	if r.Method != http.MethodHead {
		n, _ := io.Copy(w, res.Body)
		s.stats.RecordOriginBytes(n, now)
	}
	return OutcomeBypass, contentType
}

func (s *Server) isTooLarge(path string) bool {
	s.tooLargeMu.Lock()
	defer s.tooLargeMu.Unlock()
	until, ok := s.tooLarge[path]
	if ok && s.now().After(until) {
		delete(s.tooLarge, path)
		return false
	}
	return ok
}

func (s *Server) markTooLarge(path string) {
	s.tooLargeMu.Lock()
	s.tooLarge[path] = s.now().Add(s.cfg.DefaultCacheTTL)
	s.tooLargeMu.Unlock()
}

func (s *Server) unmarkTooLarge(path string) {
	s.tooLargeMu.Lock()
	delete(s.tooLarge, path)
	s.tooLargeMu.Unlock()
}

// sweep drops entries that are past any use: expired 404s and entries beyond the stale window.
func (s *Server) sweep() int {
	now := s.now()
	return s.cache.RemoveWhere(func(e *CacheEntry) bool {
		if e.Status == http.StatusNotFound {
			return !e.Fresh(now)
		}
		return now.After(e.ExpiresAt.Add(s.cfg.StaleIfError))
	})
}

func (s *Server) clientIP(r *http.Request) string {
	if s.cfg.TrustProxy {
		if ip := strings.TrimSpace(r.Header.Get("X-Real-IP")); ip != "" {
			return ip
		}
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			return strings.TrimSpace(strings.Split(xff, ",")[0])
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ─── Helpers ────────────────────────────────────────────────────

var extraTypes = map[string]string{
	".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".png": "image/png", ".webp": "image/webp",
	".avif": "image/avif", ".gif": "image/gif", ".svg": "image/svg+xml", ".ico": "image/x-icon",
	".heic": "image/heic", ".heif": "image/heif", ".tif": "image/tiff", ".tiff": "image/tiff",
	".mp4": "video/mp4", ".webm": "video/webm", ".mov": "video/quicktime", ".m4v": "video/x-m4v",
	".avi": "video/x-msvideo", ".mkv": "video/x-matroska", ".pdf": "application/pdf",
	".woff": "font/woff", ".woff2": "font/woff2", ".ttf": "font/ttf", ".json": "application/json",
	".css": "text/css; charset=utf-8", ".js": "text/javascript; charset=utf-8", ".txt": "text/plain; charset=utf-8",
}

// contentTypeFor trusts the source unless it is generic, then the extension, then the bytes.
func contentTypeFor(path, fromSource string, body []byte) string {
	if fromSource != "" && !strings.HasPrefix(fromSource, "application/octet-stream") {
		return fromSource
	}
	ext := strings.ToLower(filepath.Ext(path))
	if t, ok := extraTypes[ext]; ok {
		return t
	}
	if t := mime.TypeByExtension(ext); t != "" {
		return t
	}
	if len(body) > 0 {
		return http.DetectContentType(body)
	}
	return "application/octet-stream"
}

func etagOf(b []byte) string {
	sum := md5.Sum(b)
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// countingWriter remembers the status and body size for the stats.
type countingWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (c *countingWriter) WriteHeader(code int) {
	if c.status == 0 {
		c.status = code
	}
	c.ResponseWriter.WriteHeader(code)
}

func (c *countingWriter) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	n, err := c.ResponseWriter.Write(b)
	c.bytes += int64(n)
	return n, err
}

func (c *countingWriter) statusCode() int {
	if c.status == 0 {
		return http.StatusOK
	}
	return c.status
}

// ─── Request coalescing ─────────────────────────────────────────

// flightGroup makes concurrent misses for the same key share one fetch.
type flightGroup struct {
	mu sync.Mutex
	m  map[string]*flightCall
}

type flightCall struct {
	wg  sync.WaitGroup
	res flightResult
}

type flightResult struct {
	entry   *CacheEntry
	outcome string
	err     error
}

func (g *flightGroup) Do(key string, fn func() flightResult) flightResult {
	g.mu.Lock()
	if g.m == nil {
		g.m = map[string]*flightCall{}
	}
	if c, ok := g.m[key]; ok {
		g.mu.Unlock()
		c.wg.Wait()
		return c.res
	}
	c := &flightCall{}
	c.wg.Add(1)
	g.m[key] = c
	g.mu.Unlock()

	c.res = fn()
	c.wg.Done()

	g.mu.Lock()
	delete(g.m, key)
	g.mu.Unlock()
	return c.res
}
