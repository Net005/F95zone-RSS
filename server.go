package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

//go:embed web
var webFS embed.FS

type Server struct {
	app *App
	db  *DB
	thr throttle
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	sub, _ := fs.Sub(webFS, "web")

	// public: feed, images, health
	mux.HandleFunc("GET /feed.xml", s.feed)
	mux.HandleFunc("GET /rss", s.feed)
	mux.HandleFunc("GET /f95zone/images/{file}", s.image)
	mux.HandleFunc("GET /images/{file}", s.image)
	mux.HandleFunc("GET /health", s.health)

	// login + static assets are public; the panel itself is behind the session
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("GET /api/auth/status", s.authStatus)
	mux.HandleFunc("POST /api/auth/login", s.login)
	mux.HandleFunc("POST /api/auth/setup", s.setup)
	mux.Handle("GET /ui/", http.StripPrefix("/ui/", http.FileServerFS(sub)))
	mux.Handle("GET /{$}", s.auth(http.HandlerFunc(s.indexPage)))
	api := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.auth(h)) }
	api("POST /api/auth/logout", s.logout)
	api("POST /api/account/password", s.changePassword)
	api("GET /api/status", s.status)
	api("GET /api/releases", s.releases)
	api("GET /api/release", s.releaseDetail)
	api("GET /api/tags", s.tags)
	api("GET /api/engines", s.engines)
	api("POST /api/releases/watch", s.setWatched)
	api("POST /api/f95/check-login", s.checkLogin)
	api("GET /api/history", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, s.app.store.History()) })
	api("GET /api/config", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, s.app.cfg.Get()) })
	api("PUT /api/config", s.putConfig)
	api("POST /api/run", s.run)
	api("POST /api/stop", s.stop)
	api("POST /api/reenrich", s.reenrich)
	api("POST /api/feed/rebuild", s.rebuild)
	api("POST /api/cache/clear", s.clearCache)
	api("POST /api/images/purge", s.purge)
	api("POST /api/backfill", s.backfill)
	api("GET /api/notifications", s.notifications)
	api("POST /api/notifications/read", s.notifRead)
	api("POST /api/notifications/clear", s.notifClear)
	api("POST /api/pushover/test", s.pushoverTest)
	api("GET /api/logs", s.logsTail)
	api("GET /api/logs/stream", s.logsStream)
	api("GET /api/logs/download", s.logsDownload)
	api("POST /api/logs/clear", func(w http.ResponseWriter, r *http.Request) {
		s.app.log.Clear()
		s.app.log.Info("Log cleared from web UI")
		writeJSON(w, 200, map[string]any{"ok": true})
	})
	return mux
}

// ── helpers ──

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]any{"ok": false, "error": err.Error()})
}

func readBody(r *http.Request, v any) {
	r.Body = http.MaxBytesReader(nil, r.Body, 1<<20)
	json.NewDecoder(r.Body).Decode(v)
}

func (s *Server) coverURL(r Release) string {
	h := r.HeaderImage
	if h == "" && len(r.ImageURLs) > 0 {
		h = r.ImageURLs[0]
	}
	if h == "" {
		return ""
	}
	h = hqURL(h)
	if n := imgFilename(h); s.app.store.ImageExists(n) {
		return "/f95zone/images/" + n
	}
	return h
}

// ── public endpoints ──

func (s *Server) feed(w http.ResponseWriter, r *http.Request) {
	b, err := os.ReadFile(s.app.store.rssFile)
	if err != nil {
		http.Error(w, "No feed available yet - first run may still be running.", 503)
		return
	}
	w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
	w.Write(b)
}

func (s *Server) image(w http.ResponseWriter, r *http.Request) {
	name := filepath.Base(r.PathValue("file"))
	p := filepath.Join(s.app.store.imagesDir, name)
	if _, err := os.Stat(p); err != nil {
		http.NotFound(w, r)
		return
	}
	if m, ok := mimeMap[strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))]; ok {
		w.Header().Set("Content-Type", m)
	}
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeFile(w, r, p)
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	a := s.app
	cfg := a.cfg.Get()
	p := a.Progress()
	stats := a.store.db.Stats()
	cacheAge := "N/A"
	if fw, ok := a.loadFeedWindow(); ok {
		cacheAge = fmt.Sprintf("%dh %dm", int(time.Since(fw.At).Hours()), int(time.Since(fw.At).Minutes())%60)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "F95Zone RSS Enricher (Go) v%s\n\nStatus:      %s\nProgress:    %d/%d (%d%%)\n", appVersion, p.Status, p.Current, p.Total, p.Percent)
	if p.Item != "" {
		fmt.Fprintf(w, "Current:     %s\n", p.Item)
	}
	fmt.Fprintf(w, "\nReleases in database: %d\nFeed window age:      %s\nCache TTL:            %gh\n\nPort:  %d\nFeed:  %s/feed.xml\n",
		stats.Total, cacheAge, cfg.CacheTTLHours, a.port, cfg.PublicBaseURL)
}

// ── status ──

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	a := s.app
	cfg := a.cfg.Get()
	stats := a.store.db.Stats()
	out := map[string]any{
		"version":              appVersion,
		"uptime_s":             int(time.Since(a.start).Seconds()),
		"progress":             a.Progress(),
		"running":              a.IsRunning(),
		"scheduler_enabled":    cfg.SchedulerEnabled,
		"schedule_hours":       cfg.ScheduleHours,
		"fetch_mode":           cfg.FetchMode,
		"public_base_url":      cfg.PublicBaseURL,
		"feed_url":             cfg.PublicBaseURL + "/feed.xml",
		"images":               a.store.ImageStats(),
		"unread_notifications": a.store.db.UnreadCount(),
		"cache": map[string]any{
			"items": stats.Total, "with_description": stats.WithDesc, "failed": stats.Failed,
			"needs_reparse": stats.NeedsReparse, "ttl_hours": cfg.CacheTTLHours,
		},
		"now": time.Now(),
	}
	if nr := a.NextRun(); !nr.IsZero() {
		out["next_run"] = nr
	}
	if fw, ok := a.loadFeedWindow(); ok {
		out["cache"].(map[string]any)["mtime"] = fw.At
		out["cache"].(map[string]any)["fresh"] = cfg.CacheTTLHours > 0 && time.Since(fw.At) < time.Duration(cfg.CacheTTLHours*float64(time.Hour))
		out["cache"].(map[string]any)["window_items"] = len(fw.Links)
	}
	if st, err := os.Stat(a.store.rssFile); err == nil {
		out["feed"] = map[string]any{"size": st.Size(), "mtime": st.ModTime()}
	}
	if h := a.store.History(); len(h) > 0 {
		out["last_run"] = h[0]
		okc := 0
		for _, x := range h {
			if x.Status == "ok" || x.Status == "cache" {
				okc++
			}
		}
		out["runs_total"], out["runs_ok"] = len(h), okc
	}
	writeJSON(w, 200, out)
}

// ── releases ──

type relSummary struct {
	Title      string   `json:"title"`
	Link       string   `json:"link"`
	Pub        string   `json:"pub"`
	Labels     []string `json:"labels"`
	Tags       []string `json:"tags"`
	Engine     string   `json:"engine"`
	Version    string   `json:"version"`
	Cover      string   `json:"cover"`
	Images     int      `json:"images"`
	DescLen    int      `json:"desc_len"`
	Error      string   `json:"error,omitempty"`
	EnrichedAt string   `json:"enriched_at,omitempty"`
	Watched    bool     `json:"watched"`
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (s *Server) releases(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	pageSize, _ := strconv.Atoi(q.Get("page_size"))
	f := ReleaseFilter{
		Query: strings.TrimSpace(q.Get("q")), Tags: splitCSV(q.Get("tags")), Engine: q.Get("engine"),
		Failed: q.Get("failed") == "1", WatchedOnly: q.Get("watched") == "1", Sort: q.Get("sort"), Page: page, PageSize: pageSize,
	}
	res, err := s.app.store.db.QueryReleases(f)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	items := make([]relSummary, len(res.Items))
	for i, x := range res.Items {
		items[i] = relSummary{
			Title: x.Title, Link: x.Link, Pub: x.PubDateISO, Labels: x.Labels, Tags: x.Tags, Engine: x.Engine, Version: x.Version,
			Cover: s.coverURL(x), Images: len(x.ImageURLs), DescLen: len(x.ExtraDescription), Error: x.EnrichError, EnrichedAt: x.EnrichedAt,
			Watched: x.Watched,
		}
	}
	writeJSON(w, 200, map[string]any{"total": res.Total, "page": res.Page, "pages": res.Pages, "page_size": f.PageSize, "items": items})
}

func (s *Server) releaseDetail(w http.ResponseWriter, r *http.Request) {
	rel, ok := s.app.store.db.GetRelease(r.URL.Query().Get("link"))
	if !ok {
		writeErr(w, 404, fmt.Errorf("not found"))
		return
	}
	cfg := s.app.cfg.Get()
	// preview should load images from this server, not the public URL
	pv := cfg
	pv.PublicBaseURL = ""
	imgs := []string{}
	for _, u := range rel.ImageURLs {
		u = hqURL(u)
		if n := imgFilename(u); s.app.store.ImageExists(n) {
			imgs = append(imgs, "/f95zone/images/"+n)
		} else {
			imgs = append(imgs, u)
		}
	}
	writeJSON(w, 200, map[string]any{
		"release": rel, "cover": s.coverURL(rel), "images": imgs,
		"feed_html": s.app.BuildDescription(rel, pv),
	})
}

func (s *Server) tags(w http.ResponseWriter, r *http.Request) {
	limit := 300
	if l, _ := strconv.Atoi(r.URL.Query().Get("limit")); l > 0 {
		limit = l
	}
	out := s.app.store.db.TagCounts(limit)
	if out == nil {
		out = []Count{}
	}
	writeJSON(w, 200, out)
}

func (s *Server) engines(w http.ResponseWriter, r *http.Request) {
	out := s.app.store.db.EngineCounts()
	if out == nil {
		out = []Count{}
	}
	writeJSON(w, 200, out)
}

func (s *Server) setWatched(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Link    string
		Watched bool
	}
	readBody(r, &b)
	if strings.TrimSpace(b.Link) == "" {
		writeErr(w, 400, fmt.Errorf("link is required"))
		return
	}
	if err := s.app.store.db.SetWatched(b.Link, b.Watched); err != nil {
		writeErr(w, 404, fmt.Errorf("release not found"))
		return
	}
	verb := "Stopped monitoring"
	if b.Watched {
		verb = "Monitoring"
	}
	s.app.log.Info("%s %s", verb, b.Link)
	writeJSON(w, 200, map[string]any{"ok": true, "watched": b.Watched})
}

// checkLogin probes F95zone with the configured site_cookie and reports
// whether it's actually a working, logged-in session (see CheckF95Login).
func (s *Server) checkLogin(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	res, err := s.app.CheckF95Login(ctx)
	if err != nil && res.Detail == "" {
		writeErr(w, 502, err)
		return
	}
	writeJSON(w, 200, res)
}

// ── actions ──

func (s *Server) putConfig(w http.ResponseWriter, r *http.Request) {
	cfg := s.app.cfg.Get() // start from current so omitted keys are kept
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeErr(w, 400, err)
		return
	}
	if err := s.app.cfg.Set(cfg); err != nil {
		writeErr(w, 400, err)
		return
	}
	s.app.log.Info("Settings saved from web UI")
	writeJSON(w, 200, s.app.cfg.Get())
}

func (s *Server) run(w http.ResponseWriter, r *http.Request) {
	var b struct{ Force, Full bool }
	readBody(r, &b)
	if err := s.app.StartRun("manual", b.Force || b.Full, b.Full); err != nil {
		writeErr(w, 409, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) stop(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"ok": s.app.Stop()})
}

func (s *Server) reenrich(w http.ResponseWriter, r *http.Request) {
	var b struct{ Link string }
	readBody(r, &b)
	if err := s.app.ReenrichOne(b.Link); err != nil {
		code := 400
		if err == ErrBusy {
			code = 409
		}
		writeErr(w, code, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) rebuild(w http.ResponseWriter, r *http.Request) {
	n, err := s.app.RebuildFeed()
	if err != nil {
		writeErr(w, 409, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "items": n})
}

func (s *Server) clearCache(w http.ResponseWriter, r *http.Request) {
	if s.app.IsRunning() {
		writeErr(w, 409, ErrBusy)
		return
	}
	if err := s.app.store.db.DeleteAllReleases(); err != nil {
		writeErr(w, 500, err)
		return
	}
	s.app.store.db.SetSetting("feed_window", "")
	s.app.log.Warn("Release history cleared from web UI (feed.xml kept until next run)")
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) purge(w http.ResponseWriter, r *http.Request) {
	if s.app.IsRunning() {
		writeErr(w, 409, ErrBusy)
		return
	}
	n, sz := s.app.store.PurgeOrphans()
	s.app.log.Info("Purged %d orphaned images (%.1f MB)", n, float64(sz)/1e6)
	writeJSON(w, 200, map[string]any{"ok": true, "removed": n, "bytes": sz})
}

func (s *Server) backfill(w http.ResponseWriter, r *http.Request) {
	var b struct{ Pages int }
	readBody(r, &b)
	if b.Pages < 1 {
		b.Pages = 5
	}
	if err := s.app.StartBackfill(b.Pages); err != nil {
		writeErr(w, 409, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "pages": b.Pages})
}

// ── notifications ──

func (s *Server) notifications(w http.ResponseWriter, r *http.Request) {
	unread := r.URL.Query().Get("unread") == "1"
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	out := s.app.store.db.Notifications(unread, limit)
	writeJSON(w, 200, map[string]any{"items": out, "unread": s.app.store.db.UnreadCount()})
}

func (s *Server) notifRead(w http.ResponseWriter, r *http.Request) {
	var b struct {
		ID   int64
		All  bool
		Read *bool
	}
	readBody(r, &b)
	read := true
	if b.Read != nil {
		read = *b.Read
	}
	if b.All {
		s.app.store.db.MarkAllNotificationsRead()
	} else if b.ID != 0 {
		s.app.store.db.MarkNotificationRead(b.ID, read)
	}
	writeJSON(w, 200, map[string]any{"ok": true, "unread": s.app.store.db.UnreadCount()})
}

func (s *Server) notifClear(w http.ResponseWriter, r *http.Request) {
	var b struct {
		ID  int64
		All bool
	}
	readBody(r, &b)
	if b.All {
		s.app.store.db.DeleteAllNotifications()
		s.app.log.Info("All notifications cleared from web UI")
	} else if b.ID != 0 {
		s.app.store.db.DeleteNotification(b.ID)
	}
	writeJSON(w, 200, map[string]any{"ok": true, "unread": s.app.store.db.UnreadCount()})
}

func (s *Server) pushoverTest(w http.ResponseWriter, r *http.Request) {
	cfg := s.app.cfg.Get()
	if errStr := SendPushover(cfg, "F95Zone RSS Control", "Test notification - Pushover is working.", cfg.PublicBaseURL); errStr != "" {
		writeErr(w, 400, fmt.Errorf("%s", errStr))
		return
	}
	s.app.log.Info("Sent a test Pushover notification")
	writeJSON(w, 200, map[string]any{"ok": true})
}

// ── logs ──

func (s *Server) logsTail(w http.ResponseWriter, r *http.Request) {
	n, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if n <= 0 || n > 5000 {
		n = 500
	}
	writeJSON(w, 200, s.app.log.Since(0, n))
}

func (s *Server) logsStream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", 500)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no") // nginx/Caddy: don't buffer
	last := int64(0)
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		last, _ = strconv.ParseInt(v, 10, 64)
	} else if v := r.URL.Query().Get("since"); v != "" {
		last, _ = strconv.ParseInt(v, 10, 64)
	}
	ch, unsub := s.app.log.Subscribe()
	defer unsub()
	send := func(e LogEntry) {
		b, _ := json.Marshal(e)
		fmt.Fprintf(w, "id: %d\ndata: %s\n\n", e.ID, b)
		last = e.ID
	}
	for _, e := range s.app.log.Since(last, 5000) {
		send(e)
	}
	fl.Flush()
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case e := <-ch:
			if e.ID > last {
				send(e)
			}
			fl.Flush()
		case <-tick.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		}
	}
}

func (s *Server) logsDownload(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="f95zone.log"`)
	http.ServeFile(w, r, s.app.log.Path())
}
