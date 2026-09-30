package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	cfg := LoadConfig()
	srv := NewServer(cfg, NewSource(cfg))
	srv.stats.Load(cfg.StatsFile)
	srv.loadSettings()

	log.SetPrefix("Asset Service : \t")
	log.Printf("Starting on http://0.0.0.0%s", cfg.Port)
	log.Printf("Originals from %s: %s", cfg.SourceName(), srv.source.Describe())
	log.Printf("Cache: %d entries / %d MB, TTL %v, max object %d MB, resizing %v",
		cfg.CacheSize, cfg.CacheMemoryMB, cfg.DefaultCacheTTL, cfg.MaxObjectMB, transformEnabled)
	if cfg.AdminKey == "" {
		log.Printf("ADMIN_KEY is not set: the admin API and dashboard are off")
	}

	httpServer := &http.Server{
		Addr:              cfg.Port,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go srv.background(ctx)

	go func() {
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()

	<-ctx.Done()
	log.Printf("Shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
	if err := srv.stats.Save(cfg.StatsFile); err != nil {
		log.Printf("stats: save failed: %v", err)
	}
}

func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", healthHandler)
	mux.HandleFunc("GET /asset/", s.handleAsset) // GET also matches HEAD

	mux.HandleFunc("GET /admin/overview", s.requireAdmin(s.handleOverview))
	mux.HandleFunc("GET /admin/entries", s.requireAdmin(s.handleEntries))
	mux.HandleFunc("GET /admin/paths", s.requireAdmin(s.handlePaths))
	mux.HandleFunc("GET /admin/path", s.requireAdmin(s.handlePath))
	mux.HandleFunc("GET /admin/requests", s.requireAdmin(s.handleRequests))
	mux.HandleFunc("POST /admin/purge", s.requireAdmin(s.handlePurge))
	mux.HandleFunc("POST /admin/warm", s.requireAdmin(s.handleWarm))
	mux.HandleFunc("POST /admin/stats/reset", s.requireAdmin(s.handleStatsReset))
	mux.HandleFunc("GET /admin/health", s.requireAdmin(s.handleHealth))
	mux.HandleFunc("GET /admin/settings", s.requireAdmin(s.handleGetSettings))
	mux.HandleFunc("POST /admin/settings", s.requireAdmin(s.handleSetSettings))

	// Legacy: GET /admin/clear-cache?secret=...
	mux.HandleFunc("GET /admin/clear-cache", func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.AdminKey == "" || subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("secret")), []byte(s.cfg.AdminKey)) != 1 {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		before := s.cache.Len()
		s.cache.Purge()
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "Cache cleared successfully\nBefore: %d entries\nAfter: %d entries\n", before, s.cache.Len())
	})

	mux.HandleFunc("GET /", rootHandler)
	return mux
}

// background sweeps dead entries every minute and saves stats every five.
func (s *Server) background(ctx context.Context) {
	sweep := time.NewTicker(time.Minute)
	save := time.NewTicker(5 * time.Minute)
	defer sweep.Stop()
	defer save.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sweep.C:
			s.sweep()
		case <-save.C:
			if err := s.stats.Save(s.cfg.StatsFile); err != nil {
				log.Printf("stats: save failed: %v", err)
			}
		}
	}
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
