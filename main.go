package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

func main() {
	envInt := func(k string, d int) int {
		if v, err := strconv.Atoi(os.Getenv(k)); err == nil {
			return v
		}
		return d
	}
	envStr := func(k, d string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return d
	}
	port := flag.Int("port", envInt("PORT", defaultPort), "HTTP port")
	base := flag.String("base", envStr("F95_BASE_DIR", "."), "base directory (holds config/, data/, logs/)")
	health := flag.Bool("healthcheck", false, "probe http://127.0.0.1:PORT/health and exit (for Docker HEALTHCHECK)")
	flag.Parse()
	if *health {
		c := http.Client{Timeout: 4 * time.Second}
		resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/health", *port))
		if err != nil || resp.StatusCode != 200 {
			os.Exit(1)
		}
		os.Exit(0)
	}

	for _, d := range []string{"config", "data/images", "logs"} {
		if err := os.MkdirAll(filepath.Join(*base, d), 0o755); err != nil {
			fmt.Fprintln(os.Stderr, "cannot create", d, err)
			os.Exit(1)
		}
	}
	os.Remove(filepath.Join(*base, "data", "scrape.lock")) // stale lock from the Python version

	logs := NewLogHub(filepath.Join(*base, "logs", "f95zone.log"))
	db, err := OpenDB(filepath.Join(*base, "data", "f95zone.db"))
	if err != nil {
		logs.Error("cannot open database: %v", err)
		os.Exit(1)
	}
	cfg := NewConfigStore(db, filepath.Join(*base, "config", "settings.json"), func(l, m string) { logs.Log(l, "%s", m) })
	app := &App{cfg: cfg, log: logs, store: NewStore(filepath.Join(*base, "data")), start: time.Now(), port: *port}
	app.progress = Progress{Status: "IDLE"}

	c := cfg.Get()
	logs.Info("============================================================")
	logs.Info("F95Zone RSS Enricher (Go) v%s", appVersion)
	logs.Info("Base dir: %s", *base)
	logs.Info("Fetch mode: %s | Cache TTL: %g hours | Public URL: %s", c.FetchMode, c.CacheTTLHours, c.PublicBaseURL)
	logs.Info("============================================================")

	bootstrapAdmin(db, os.Getenv("ADMIN_USER"), os.Getenv("ADMIN_PASSWORD"), logs)
	srv := &Server{app: app, db: db}
	if db.UserCount() == 0 {
		logs.Warn("No administrator account yet - open the web UI to create one (first visitor wins, do this before exposing the port).")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go app.RunScheduler(ctx)

	hs := &http.Server{Addr: fmt.Sprintf(":%d", *port), Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		logs.Info("Shutdown signal received")
		app.Stop()
		sc, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		hs.Shutdown(sc)
	}()
	logs.Info("HTTP server on port %d", *port)
	logs.Info("  UI:   http://localhost:%d/", *port)
	logs.Info("  RSS:  %s/feed.xml", c.PublicBaseURL)
	if err := hs.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		logs.Error("server: %v", err)
		os.Exit(1)
	}
}
