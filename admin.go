package main

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

const version = "2.0.0"

// requireAdmin guards the admin API with ADMIN_KEY (X-Admin-Key or Authorization: Bearer).
func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.AdminKey == "" {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"message": "admin API is off: set ADMIN_KEY"})
			return
		}
		key := r.Header.Get("X-Admin-Key")
		if key == "" {
			key = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
		if subtle.ConstantTimeCompare([]byte(key), []byte(s.cfg.AdminKey)) != 1 {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "unauthorized"})
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		next(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// ─── GET /admin/overview ────────────────────────────────────────

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	now := s.now()
	cacheStats := s.cache.Stats(now)

	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	s.stats.mu.Lock()
	t := s.stats.Totals
	o := t.Outcomes
	hits := o[OutcomeHit] + o[OutcomeRevalidated] + o[OutcomeStale]
	misses := o[OutcomeMiss] + o[OutcomeBypass]

	hours := series(s.stats.Hours, now, time.Hour, 48)
	resp := map[string]any{
		"service": map[string]any{
			"version":           version,
			"enabled":           s.enabled.Load(),
			"started_at":        s.start,
			"uptime_seconds":    int64(now.Sub(s.start).Seconds()),
			"source":            s.cfg.SourceName(),
			"source_location":   s.source.Describe(),
			"transform_enabled": transformEnabled,
			"go_version":        runtime.Version(),
			"goroutines":        runtime.NumGoroutine(),
			"heap_bytes":        mem.HeapAlloc,
			"sys_bytes":         mem.Sys,
			"hostname":          hostname(),
			"stats_persisted":   s.cfg.StatsFile != "",
		},
		"config": map[string]any{
			"cache_ttl_seconds":       int64(s.cfg.DefaultCacheTTL.Seconds()),
			"negative_ttl_seconds":    int64(s.cfg.NegativeTTL.Seconds()),
			"stale_if_error_seconds":  int64(s.cfg.StaleIfError.Seconds()),
			"browser_max_age_seconds": int64(s.cfg.BrowserMaxAge.Seconds()),
			"max_object_mb":           s.cfg.MaxObjectMB,
			"cache_memory_mb":         s.cfg.CacheMemoryMB,
			"cache_size":              s.cfg.CacheSize,
		},
		"since": s.stats.Since,
		"cache": cacheStats,
		"totals": map[string]any{
			"requests":        t.Requests,
			"bytes_served":    t.BytesServed,
			"hits":            hits,
			"misses":          misses,
			"hit_ratio":       ratio(hits, hits+misses),
			"byte_hit_ratio":  byteHitRatio(t.BytesServed, t.OriginBytes),
			"outcomes":        t.Outcomes,
			"status_codes":    t.StatusCodes,
			"avg_ms":          avg(t.DurationMsTotal, t.Requests),
			"p50_ms":          percentile(t.Latency, 0.50),
			"p95_ms":          percentile(t.Latency, 0.95),
			"p99_ms":          percentile(t.Latency, 0.99),
			"latency_bounds":  latencyBoundsMs,
			"latency_buckets": t.Latency,
		},
		"origin": map[string]any{
			"fetches":        t.OriginFetches,
			"not_modified":   t.OriginNotModified,
			"not_found":      t.OriginNotFound,
			"errors":         t.OriginErrors,
			"bytes":          t.OriginBytes,
			"avg_ms":         avg(float64(t.OriginMsTotal), t.OriginFetches),
			"last_error":     s.stats.LastOriginError,
			"last_error_at":  s.stats.LastOriginErrorAt,
			"bytes_saved":    max(t.BytesServed-t.OriginBytes, 0),
			"requests_saved": hits,
		},
		"transforms": map[string]any{
			"count":  t.Transforms,
			"errors": t.TransformErrors,
			"avg_ms": avg(float64(t.TransformMsTotal), t.Transforms),
		},
		"series": map[string]any{
			"minutes": series(s.stats.Minutes, now, time.Minute, 60),
			"hours":   hours,
			"days":    days(s.stats.Hours, now, 14),
		},
		"top": map[string]any{
			"referer_hosts": s.stats.RefererHosts.Top(10),
			"referer_pages": s.stats.RefererPages.Top(15),
			"clients":       s.stats.Clients.Top(15),
			"devices":       s.stats.Devices.Top(10),
			"browsers":      s.stats.Browsers.Top(10),
			"bots":          s.stats.Bots.Top(10),
			"content_types": s.stats.ContentTypes.Top(10),
			"folders":       s.stats.Folders.Top(10),
			"clients_seen":  len(s.stats.Clients.Items),
			"paths_seen":    len(s.stats.Paths),
		},
		"top_paths": s.pathRows(func(p *PathStat) bool { return true }, "requests", 10),
		"missing":   s.pathRows(func(p *PathStat) bool { return p.LastStatus == http.StatusNotFound }, "not_found", 20),
	}
	s.stats.mu.Unlock()

	writeJSON(w, http.StatusOK, resp)
}

// days folds the hourly buckets into calendar days (server time zone).
func days(hours []Bucket, now time.Time, n int) []Bucket {
	out := make([]Bucket, n)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	index := map[int64]int{}
	for i := 0; i < n; i++ {
		d := today.AddDate(0, 0, i-n+1)
		out[i] = Bucket{Start: d.Unix()}
		index[d.Unix()] = i
	}
	for _, h := range hours {
		t := time.Unix(h.Start, 0).In(now.Location())
		day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, now.Location()).Unix()
		if i, ok := index[day]; ok {
			b := &out[i]
			b.Requests += h.Requests
			b.Hits += h.Hits
			b.Misses += h.Misses
			b.NotFound += h.NotFound
			b.Errors += h.Errors
			b.Bytes += h.Bytes
			b.OriginBytes += h.OriginBytes
		}
	}
	return out
}

func ratio(part, total int64) float64 {
	if total == 0 {
		return 0
	}
	return float64(part) / float64(total)
}

func byteHitRatio(served, fromOrigin int64) float64 {
	if served <= 0 {
		return 0
	}
	return max(0, 1-float64(fromOrigin)/float64(served))
}

func avg(total float64, n int64) float64 {
	if n == 0 {
		return 0
	}
	return float64(int64(total/float64(n)*100)) / 100
}

func hostname() string {
	h, _ := os.Hostname()
	return h
}

// ─── Paths ──────────────────────────────────────────────────────

// PathRow is a PathStat for lists: top referers only, plus what is cached for it.
type PathRow struct {
	Path           string    `json:"path"`
	Requests       int64     `json:"requests"`
	Hits           int64     `json:"hits"`
	Misses         int64     `json:"misses"`
	NotFound       int64     `json:"not_found"`
	Errors         int64     `json:"errors"`
	Bytes          int64     `json:"bytes"`
	HitRatio       float64   `json:"hit_ratio"`
	FirstSeen      time.Time `json:"first_seen"`
	LastSeen       time.Time `json:"last_seen"`
	LastStatus     int       `json:"last_status"`
	LastOutcome    string    `json:"last_outcome"`
	TopReferer     string    `json:"top_referer"`
	RefererCount   int       `json:"referer_count"`
	VariantCount   int       `json:"variant_count"`
	Cached         bool      `json:"cached"`
	CachedVariants int       `json:"cached_variants"`
	CachedBytes    int64     `json:"cached_bytes"`
}

// pathRows must be called with s.stats.mu held.
func (s *Server) pathRows(keep func(*PathStat) bool, sortBy string, limit int) []PathRow {
	rows := s.collectPathRows(keep)
	sortPathRows(rows, sortBy, true)
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	return rows
}

func (s *Server) collectPathRows(keep func(*PathStat) bool) []PathRow {
	type cached struct {
		original bool
		variants int
		bytes    int64
	}
	now := s.now()
	byPath := map[string]*cached{}
	for _, e := range s.cache.Snapshot(now) {
		c := byPath[e.Path]
		if c == nil {
			c = &cached{}
			byPath[e.Path] = c
		}
		switch e.Kind {
		case "original":
			c.original = true
		case "variant":
			c.variants++
		}
		c.bytes += int64(e.Bytes)
	}

	rows := make([]PathRow, 0, len(s.stats.Paths))
	for _, p := range s.stats.Paths {
		if !keep(p) {
			continue
		}
		row := PathRow{
			Path: p.Path, Requests: p.Requests, Hits: p.Hits, Misses: p.Misses, NotFound: p.NotFound,
			Errors: p.Errors, Bytes: p.Bytes, HitRatio: ratio(p.Hits, p.Hits+p.Misses),
			FirstSeen: p.FirstSeen, LastSeen: p.LastSeen, LastStatus: p.LastStatus, LastOutcome: p.LastOutcome,
			RefererCount: len(p.Referers), VariantCount: len(p.Variants),
		}
		var best int64
		for ref, n := range p.Referers {
			if n > best || (n == best && ref < row.TopReferer) {
				best, row.TopReferer = n, ref
			}
		}
		if c := byPath[p.Path]; c != nil {
			row.Cached, row.CachedVariants, row.CachedBytes = c.original, c.variants, c.bytes
		}
		rows = append(rows, row)
	}
	return rows
}

func sortPathRows(rows []PathRow, by string, desc bool) {
	less := func(a, b PathRow) bool {
		switch by {
		case "hits":
			return a.Hits < b.Hits
		case "misses":
			return a.Misses < b.Misses
		case "not_found":
			return a.NotFound < b.NotFound
		case "bytes":
			return a.Bytes < b.Bytes
		case "hit_ratio":
			return a.HitRatio < b.HitRatio
		case "last_seen":
			return a.LastSeen.Before(b.LastSeen)
		case "first_seen":
			return a.FirstSeen.Before(b.FirstSeen)
		case "path":
			return a.Path < b.Path
		default:
			return a.Requests < b.Requests
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if desc {
			return less(rows[j], rows[i])
		}
		return less(rows[i], rows[j])
	})
}

// GET /admin/paths?search=&status=all|ok|missing|errors|cached|uncached&sort=&dir=&page=&per_page=
func (s *Server) handlePaths(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	search := strings.ToLower(strings.TrimSpace(q.Get("search")))
	status := q.Get("status")

	s.stats.mu.Lock()
	rows := s.collectPathRows(func(p *PathStat) bool {
		if search != "" && !strings.Contains(strings.ToLower(p.Path), search) {
			return false
		}
		switch status {
		case "ok":
			return p.LastStatus < 400
		case "missing":
			return p.LastStatus == http.StatusNotFound
		case "errors":
			return p.LastStatus >= 500
		}
		return true
	})
	s.stats.mu.Unlock()

	if status == "cached" || status == "uncached" {
		filtered := rows[:0]
		for _, row := range rows {
			if (row.Cached || row.CachedVariants > 0) == (status == "cached") {
				filtered = append(filtered, row)
			}
		}
		rows = filtered
	}

	sortPathRows(rows, q.Get("sort"), q.Get("dir") != "asc")
	page, perPage, from, to := paginate(q, len(rows))
	writeJSON(w, http.StatusOK, map[string]any{"data": rows[from:to], "total": len(rows), "page": page, "per_page": perPage})
}

// GET /admin/path?path=products/1/a.webp — everything about one file.
func (s *Server) handlePath(w http.ResponseWriter, r *http.Request) {
	path, ok := cleanAssetPath(r.URL.Query().Get("path"))
	if !ok {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "path is required"})
		return
	}
	now := s.now()

	var entries []EntryInfo
	for _, e := range s.cache.Snapshot(now) {
		if e.Path == path {
			entries = append(entries, e)
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Variant < entries[j].Variant })

	s.stats.mu.Lock()
	var stat *PathStat
	var referers, variants []CountItem
	if p := s.stats.Paths[path]; p != nil {
		copied := *p
		copied.Referers, copied.Variants = nil, nil
		stat = &copied
		referers = sortedCounts(p.Referers)
		variants = sortedCounts(p.Variants)
	}
	recent := s.recentWhere(func(rec RequestRecord) bool { return rec.Path == path }, 100)
	s.stats.mu.Unlock()

	if entries == nil {
		entries = []EntryInfo{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"path": path, "stat": stat, "entries": entries,
		"referers": referers, "variants": variants, "recent": recent,
	})
}

func sortedCounts(m map[string]int64) []CountItem {
	out := make([]CountItem, 0, len(m))
	for k, v := range m {
		out = append(out, CountItem{Key: k, Count: v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// ─── GET /admin/entries ─────────────────────────────────────────

// GET /admin/entries?search=&kind=original|variant|missing&state=fresh|stale&sort=&dir=&page=&per_page=
func (s *Server) handleEntries(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	search := strings.ToLower(strings.TrimSpace(q.Get("search")))
	kind, state := q.Get("kind"), q.Get("state")

	all := s.cache.Snapshot(s.now())
	rows := all[:0]
	for _, e := range all {
		if search != "" && !strings.Contains(strings.ToLower(e.Key), search) {
			continue
		}
		if kind != "" && kind != "all" && e.Kind != kind {
			continue
		}
		if (state == "fresh" && !e.Fresh) || (state == "stale" && e.Fresh) {
			continue
		}
		rows = append(rows, e)
	}

	by, desc := q.Get("sort"), q.Get("dir") != "asc"
	less := func(a, b EntryInfo) bool {
		switch by {
		case "bytes":
			return a.Bytes < b.Bytes
		case "created":
			return a.CreatedAt.Before(b.CreatedAt)
		case "expires":
			return a.ExpiresAt.Before(b.ExpiresAt)
		case "last_hit":
			return lastHit(a).Before(lastHit(b))
		case "fetch_ms":
			return a.FetchMs < b.FetchMs
		case "path":
			return a.Key < b.Key
		default:
			return a.Hits < b.Hits
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if desc {
			return less(rows[j], rows[i])
		}
		return less(rows[i], rows[j])
	})

	page, perPage, from, to := paginate(q, len(rows))
	writeJSON(w, http.StatusOK, map[string]any{"data": rows[from:to], "total": len(rows), "page": page, "per_page": perPage})
}

func lastHit(e EntryInfo) time.Time {
	if e.LastAccess == nil {
		return time.Time{}
	}
	return *e.LastAccess
}

func paginate(q map[string][]string, total int) (page, perPage, from, to int) {
	get := func(k string) string {
		if v := q[k]; len(v) > 0 {
			return v[0]
		}
		return ""
	}
	page, _ = strconv.Atoi(get("page"))
	perPage, _ = strconv.Atoi(get("per_page"))
	page = max(page, 1)
	if perPage <= 0 {
		perPage = 50
	}
	perPage = min(perPage, 500)
	from = min((page-1)*perPage, total)
	to = min(from+perPage, total)
	return
}

// ─── GET /admin/requests ────────────────────────────────────────

// GET /admin/requests?limit=&outcome=&device=&search= — newest first.
func (s *Server) handleRequests(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > recentCapacity {
		limit = 200
	}
	outcome, device := q.Get("outcome"), q.Get("device")
	search := strings.ToLower(strings.TrimSpace(q.Get("search")))

	s.stats.mu.Lock()
	rows := s.recentWhere(func(rec RequestRecord) bool {
		if outcome != "" && rec.Outcome != outcome {
			return false
		}
		if device != "" && rec.Device != device {
			return false
		}
		if search != "" &&
			!strings.Contains(strings.ToLower(rec.Path), search) &&
			!strings.Contains(strings.ToLower(rec.Referer), search) &&
			!strings.Contains(rec.ClientIP, search) {
			return false
		}
		return true
	}, limit)
	s.stats.mu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{"data": rows})
}

// recentWhere walks the ring newest first; call with s.stats.mu held.
func (s *Server) recentWhere(keep func(RequestRecord) bool, limit int) []RequestRecord {
	out := []RequestRecord{}
	n := len(s.stats.Recent)
	for i := 0; i < n && len(out) < limit; i++ {
		idx := (s.stats.RecentNext - 1 - i + n) % n
		if rec := s.stats.Recent[idx]; keep(rec) {
			out = append(out, rec)
		}
	}
	return out
}

// ─── Actions ────────────────────────────────────────────────────

type purgeRequest struct {
	All    bool   `json:"all"`
	Path   string `json:"path"`
	Prefix string `json:"prefix"`
	Key    string `json:"key"`
}

// POST /admin/purge {"all":true} | {"path":"products/1/a.jpg"} | {"prefix":"products/1/"} | {"key":"..."}
func (s *Server) handlePurge(w http.ResponseWriter, r *http.Request) {
	var req purgeRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "invalid JSON"})
		return
	}

	removed := 0
	switch {
	case req.All:
		removed = s.cache.Purge()
		s.tooLargeMu.Lock()
		clear(s.tooLarge)
		s.tooLargeMu.Unlock()
	case req.Key != "":
		if s.cache.Remove(req.Key) {
			removed = 1
		}
	case req.Path != "":
		path, ok := cleanAssetPath(req.Path)
		if !ok {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid path"})
			return
		}
		removed = s.cache.RemovePath(path)
		s.unmarkTooLarge(path)
	case req.Prefix != "":
		prefix := strings.TrimPrefix(strings.TrimSpace(req.Prefix), "/")
		if prefix == "" || strings.Contains(prefix, "..") {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid prefix"})
			return
		}
		removed = s.cache.RemovePrefix(prefix)
	default:
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "give all, path, prefix or key"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"removed": removed})
}

// POST /admin/warm {"paths":["products/1/a.jpg", ...]} — fetch files into the cache ahead of visitors.
func (s *Server) handleWarm(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Paths []string `json:"paths"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil || len(req.Paths) == 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "give paths"})
		return
	}
	if len(req.Paths) > 200 {
		req.Paths = req.Paths[:200]
	}
	type result struct {
		Path    string `json:"path"`
		Outcome string `json:"outcome"`
		Bytes   int    `json:"bytes"`
		Error   string `json:"error,omitempty"`
	}
	results := make([]result, 0, len(req.Paths))
	for _, raw := range req.Paths {
		path, ok := cleanAssetPath(strings.TrimPrefix(raw, "/storage"))
		if !ok {
			results = append(results, result{Path: raw, Outcome: OutcomeError, Error: "invalid path"})
			continue
		}
		e, outcome, err := s.getOriginal(path)
		res := result{Path: path, Outcome: outcome}
		if err != nil {
			res.Outcome, res.Error = OutcomeError, err.Error()
		} else {
			res.Bytes = len(e.Body)
		}
		results = append(results, res)
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": results})
}

// POST /admin/stats/reset
func (s *Server) handleStatsReset(w http.ResponseWriter, r *http.Request) {
	s.stats.Reset(s.now())
	if err := s.stats.Save(s.cfg.StatsFile); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": fmt.Sprintf("reset, but saving failed: %v", err)})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"reset": true})
}
