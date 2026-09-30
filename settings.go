package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// Settings are the switches the dashboard can flip at runtime.
type Settings struct {
	Enabled   bool      `json:"enabled"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (s *Server) loadSettings() {
	if s.cfg.SettingsFile == "" {
		return
	}
	data, err := os.ReadFile(s.cfg.SettingsFile)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("settings: cannot read %s: %v", s.cfg.SettingsFile, err)
		}
		return
	}
	var st Settings
	if err := json.Unmarshal(data, &st); err != nil {
		log.Printf("settings: ignoring unreadable %s: %v", s.cfg.SettingsFile, err)
		return
	}
	s.enabled.Store(st.Enabled)
	if !st.Enabled {
		log.Printf("settings: caching is switched OFF (requests go straight to the source)")
	}
}

func (s *Server) saveSettings(st Settings) error {
	if s.cfg.SettingsFile == "" {
		return nil
	}
	data, _ := json.Marshal(st)
	if err := os.MkdirAll(filepath.Dir(s.cfg.SettingsFile), 0o755); err != nil {
		return err
	}
	tmp := s.cfg.SettingsFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.cfg.SettingsFile)
}

// GET /admin/settings
func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"enabled": s.enabled.Load(), "persisted": s.cfg.SettingsFile != ""})
}

// POST /admin/settings {"enabled": false, "purge": true}
func (s *Server) handleSetSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled *bool `json:"enabled"`
		Purge   bool  `json:"purge"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&req); err != nil || req.Enabled == nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "give enabled: true or false"})
		return
	}
	s.enabled.Store(*req.Enabled)
	removed := 0
	if req.Purge {
		removed = s.cache.Purge()
	}
	log.Printf("settings: caching switched %s from the dashboard", map[bool]string{true: "ON", false: "OFF"}[*req.Enabled])

	resp := map[string]any{"enabled": *req.Enabled, "removed": removed, "persisted": s.cfg.SettingsFile != ""}
	if err := s.saveSettings(Settings{Enabled: *req.Enabled, UpdatedAt: s.now()}); err != nil {
		resp["warning"] = "switched, but not saved (will reset on restart): " + err.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

// GET /admin/health — the service, its source and its storage.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	t0 := time.Now()
	probeErr := s.source.Probe(ctx)
	sourceMs := float64(time.Since(t0).Microseconds()) / 1000

	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	cacheStats := s.cache.Stats(s.now())

	checks := []map[string]any{}
	add := func(name, status, detail string) {
		checks = append(checks, map[string]any{"name": name, "status": status, "detail": detail})
	}

	status := "ok"
	if probeErr != nil {
		status = "down"
		add("source", "error", probeErr.Error())
	} else {
		add("source", "ok", s.source.Describe())
	}

	if !s.enabled.Load() {
		add("caching", "warn", "switched off: every request goes to the source")
		if status == "ok" {
			status = "degraded"
		}
	} else {
		add("caching", "ok", "on")
	}

	if cacheStats.MaxBytes > 0 && float64(cacheStats.Bytes)/float64(cacheStats.MaxBytes) > 0.95 {
		add("memory", "warn", "cache memory is full; least used files are being evicted")
	} else {
		add("memory", "ok", "")
	}

	s.stats.mu.Lock()
	lastErr, lastErrAt := s.stats.LastOriginError, s.stats.LastOriginErrorAt
	s.stats.mu.Unlock()
	if lastErrAt != nil && s.now().Sub(*lastErrAt) < 5*time.Minute {
		add("recent source errors", "warn", lastErr)
		if status == "ok" {
			status = "degraded"
		}
	}

	if s.cfg.StatsFile == "" {
		add("stats", "warn", "not saved: counts reset on restart (set STATS_FILE)")
	} else {
		add("stats", "ok", s.cfg.StatsFile)
	}

	code := http.StatusOK
	if status == "down" {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, map[string]any{
		"status":            status,
		"enabled":           s.enabled.Load(),
		"version":           version,
		"uptime_seconds":    int64(s.now().Sub(s.start).Seconds()),
		"source":            s.cfg.SourceName(),
		"source_location":   s.source.Describe(),
		"source_ms":         sourceMs,
		"transform_enabled": transformEnabled,
		"heap_bytes":        mem.HeapAlloc,
		"goroutines":        runtime.NumGoroutine(),
		"cache_entries":     cacheStats.Entries,
		"cache_bytes":       cacheStats.Bytes,
		"checks":            checks,
		"checked_at":        s.now(),
	})
}
