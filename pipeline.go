package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

type App struct {
	cfg   *ConfigStore
	log   *LogHub
	store *Store
	start time.Time
	port  int

	mu       sync.Mutex
	progress Progress
	cancel   context.CancelFunc
	running  bool
	nextRun  time.Time
}

type Progress struct {
	Status  string    `json:"status"` // IDLE FETCHING ENRICHING DONE ERROR CANCELLED
	Current int       `json:"current"`
	Total   int       `json:"total"`
	Percent int       `json:"percent"`
	Item    string    `json:"item"`
	Trigger string    `json:"trigger"`
	Started time.Time `json:"started"`
	Failed  int       `json:"failed"`
}

var ErrBusy = errors.New("a run is already in progress")

func (a *App) Progress() Progress {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.progress
}

func (a *App) setProgress(f func(p *Progress)) {
	a.mu.Lock()
	f(&a.progress)
	if a.progress.Total > 0 {
		a.progress.Percent = a.progress.Current * 100 / a.progress.Total
	}
	a.mu.Unlock()
}

func (a *App) IsRunning() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.running
}

func (a *App) begin(trigger string) (context.Context, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.running {
		return nil, ErrBusy
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	a.running = true
	a.progress = Progress{Status: "FETCHING", Trigger: trigger, Started: time.Now()}
	return ctx, nil
}

func (a *App) end(status string) {
	a.mu.Lock()
	a.running = false
	if a.cancel != nil {
		a.cancel()
		a.cancel = nil
	}
	a.progress.Status = status
	a.progress.Item = ""
	a.mu.Unlock()
}

func (a *App) Stop() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.running && a.cancel != nil {
		a.cancel()
		return true
	}
	return false
}

// StartRun launches a pipeline run in the background.
// force ignores the cache TTL; full disables reuse of already-enriched items.
func (a *App) StartRun(trigger string, force, full bool) error {
	ctx, err := a.begin(trigger)
	if err != nil {
		return err
	}
	go a.runPipeline(ctx, trigger, force, full)
	return nil
}

// ── feed window: which links currently make up feed.xml, so a cache hit can
// rebuild the feed without re-fetching anything. Stored as a small settings blob. ──

type feedWindow struct {
	Links []string  `json:"links"`
	At    time.Time `json:"at"`
}

func (a *App) loadFeedWindow() (feedWindow, bool) {
	v, ok := a.store.db.GetSetting("feed_window")
	if !ok {
		return feedWindow{}, false
	}
	var fw feedWindow
	if json.Unmarshal([]byte(v), &fw) != nil {
		return feedWindow{}, false
	}
	return fw, true
}

func (a *App) saveFeedWindow(links []string) {
	b, _ := json.Marshal(feedWindow{Links: links, At: time.Now()})
	a.store.db.SetSetting("feed_window", string(b))
}

func (a *App) runPipeline(ctx context.Context, trigger string, force, full bool) {
	cfg := a.cfg.Get()
	rec := RunRecord{Trigger: trigger, Started: time.Now()}
	finish := func(status, errMsg string) {
		rec.Status, rec.Error = status, errMsg
		rec.Finished = time.Now()
		rec.Duration = rec.Finished.Sub(rec.Started).Seconds()
		a.store.AddRun(rec)
		ps := map[string]string{"ok": "DONE", "cache": "DONE", "error": "ERROR", "cancelled": "CANCELLED"}[status]
		if status == "error" {
			a.notifyFailure(cfg, "Enrichment run", errMsg)
		}
		a.end(ps)
	}
	defer func() {
		if r := recover(); r != nil {
			a.log.Error("Pipeline panic: %v", r)
			finish("error", "panic")
		}
	}()

	if fw, ok := a.loadFeedWindow(); !force && ok && len(fw.Links) > 0 && cfg.CacheTTLHours > 0 && time.Since(fw.At) < time.Duration(cfg.CacheTTLHours*float64(time.Hour)) {
		a.log.Info("Cache is fresh (< %gh), reusing cached data", cfg.CacheTTLHours)
		var rel []Release
		for _, link := range fw.Links {
			if r, ok := a.store.db.GetRelease(link); ok {
				rel = append(rel, r)
			}
		}
		if len(rel) > 0 {
			if err := a.GenerateFeed(rel); err != nil {
				a.log.Error("Feed build failed: %v", err)
				finish("error", err.Error())
				return
			}
			a.saveFeedWindow(fw.Links)
			a.setProgress(func(p *Progress) { p.Percent = 100 })
			a.log.Info("RSS rebuilt from cache (%d items)", len(rel))
			rec.Items = len(rel)
			finish("cache", "")
			return
		}
	}

	a.log.Info("============================================================")
	a.log.Info("Starting F95Zone RSS enrichment pipeline (trigger: %s, mode: %s)", trigger, cfg.FetchMode)

	items, err := a.fetchListingItems(ctx, cfg, cfg.ScheduledPages)
	if err != nil || len(items) == 0 {
		msg := "no items found on the listing pages"
		if err != nil {
			msg = err.Error()
		}
		a.log.Error("%s", msg)
		finish("error", msg)
		return
	}
	rec.Items = len(items)
	a.log.Info("Listing: %d item(s) across %d page(s) to enrich", len(items), cfg.ScheduledPages)
	a.setProgress(func(p *Progress) { p.Total = len(items); p.Status = "ENRICHING" })

	var fetcher Fetcher
	var out []Release
	needFetcher := func() error {
		if fetcher != nil {
			return nil
		}
		if cfg.FetchMode == "browser" {
			a.log.Info("Launching headless Chromium...")
			f, err := newBrowserFetcher(ctx, cfg)
			if err != nil {
				return err
			}
			fetcher = f
		} else {
			fetcher = newHTTPFetcher(cfg)
		}
		return nil
	}
	defer func() {
		if fetcher != nil {
			fetcher.Close()
		}
	}()

	for i := range items {
		if ctx.Err() != nil {
			break
		}
		it := items[i]
		existing, hadExisting := a.store.db.GetRelease(it.Link)
		if hadExisting {
			// fetchListingItems only carries Link/Title/Labels/Engine/Version - the
			// original discovery date lives in the DB row and must be carried
			// forward explicitly, or every already-known release would lose its
			// Published date the moment it's seen again on a listing page.
			it.PubDateRaw, it.PubDateISO = existing.PubDateRaw, existing.PubDateISO
		}
		a.log.Info("[%d/%d] %s", i+1, len(items), trunc(it.Title, 50))
		a.setProgress(func(p *Progress) { p.Current = i + 1; p.Item = trunc(it.Title, 60) })

		reused := false
		if cfg.ReuseUnchanged && !full && hadExisting && existing.ParserVer == parserVersion &&
			existing.Title == it.Title && existing.ExtraDescription != "" && existing.EnrichError == "" &&
			existing.Overview != "" && len(existing.Tags) > 0 {
			it.ExtraDescription, it.ImageURLs, it.HeaderImage, it.EnrichedAt = existing.ExtraDescription, existing.ImageURLs, existing.HeaderImage, existing.EnrichedAt
			it.Tags, it.ThreadUpdated, it.ReleaseDate = existing.Tags, existing.ThreadUpdated, existing.ReleaseDate
			it.Developer, it.Censored, it.OS, it.Language, it.Store = existing.Developer, existing.Censored, existing.OS, existing.Language, existing.Store
			if existing.Version != "" {
				it.Version = existing.Version
			}
			it.ParserVer = parserVersion
			reused = true
			rec.Reused++
			a.log.Info("  Unchanged since last run, reusing enrichment")
		} else if hadExisting && (existing.Overview == "" || len(existing.Tags) == 0) && existing.EnrichError == "" {
			a.log.Info("  Re-scraping: missing tags or overview from a previous run")
		}
		if !reused {
			if err := needFetcher(); err != nil {
				a.log.Error("%v", err)
				finish("error", err.Error())
				return
			}
			if err := a.enrichRelease(ctx, fetcher, &it, cfg); err != nil {
				if ctx.Err() != nil {
					break
				}
				a.log.Warn("Enrichment failed for %s: %v", trunc(it.Link, 60), err)
				it.EnrichError = err.Error()
				rec.Failed++
				a.setProgress(func(p *Progress) { p.Failed = rec.Failed })
			} else {
				rec.Enriched++
			}
		}
		for _, u := range it.ImageURLs {
			if ctx.Err() != nil {
				break
			}
			if _, isNew, err := a.downloadImage(ctx, cfg, u, it.Link); err != nil {
				if ctx.Err() == nil {
					a.log.Warn("Image download error: %v", err)
				}
			} else if isNew {
				rec.ImagesNew++
			}
		}
		a.store.SaveManifest()

		a.checkAndNotify(cfg, hadExisting, existing, it, &rec)
		if err := a.store.db.UpsertRelease(it, "feed"); err != nil {
			a.log.Warn("Could not save %s to database: %v", trunc(it.Link, 60), err)
		}
		out = append(out, it)

		if !reused && i < len(items)-1 {
			select {
			case <-ctx.Done():
			case <-time.After(time.Duration(cfg.RateLimitMS) * time.Millisecond):
			}
		}
	}

	if ctx.Err() != nil {
		a.log.Warn("Run cancelled after %d/%d items - feed left untouched (progress already saved to the database)", len(out), len(items))
		finish("cancelled", "")
		return
	}
	// Every item failing enrichment usually means scraping is broken (site layout
	// change, expired session cookie, Cloudflare block) rather than N unrelated
	// thread-level errors, so it's worth a failure alert even though the run
	// otherwise "succeeds" (feed still built from whatever data existed before).
	if rec.Items > 0 && rec.Enriched == 0 && rec.Reused == 0 && rec.Failed == rec.Items {
		a.notifyFailure(cfg, "Enrichment run", fmt.Sprintf("all %d items failed to enrich - check the site cookie and live log", rec.Failed))
	}
	if err := a.GenerateFeed(out); err != nil {
		a.log.Error("Feed build failed: %v", err)
		finish("error", err.Error())
		return
	}
	links := make([]string, len(out))
	for i, r := range out {
		links[i] = r.Link
	}
	a.saveFeedWindow(links)

	a.setProgress(func(p *Progress) { p.Percent = 100 })
	el := time.Since(rec.Started)
	a.log.Info("Pipeline complete! %d items in %.1fs (%d scraped, %d reused, %d failed, %d new, %d updated, %d new images)",
		len(out), el.Seconds(), rec.Enriched, rec.Reused, rec.Failed, rec.New, rec.Updated, rec.ImagesNew)
	a.log.Info("============================================================")
	finish("ok", "")
}

// checkAndNotify compares a freshly processed release against what was stored
// before this run and files a notification (and optionally a Pushover push)
// when its version changed - but only for a release the user has explicitly
// flagged "monitor" in the Releases panel. A release can't be pre-flagged
// before it's ever been seen once, so a title's very first appearance in the
// feed is always silent by design: notifications are opt-in per game, not a
// firehose of everything the source feed happens to carry.
func (a *App) checkAndNotify(cfg Config, hadExisting bool, existing, fresh Release, rec *RunRecord) {
	if !hadExisting || !existing.Watched || !cfg.NotifyUpdate {
		return
	}
	if existing.Version == "" || fresh.Version == "" || existing.Version == fresh.Version {
		return
	}
	kind := "update"
	oldVersion := existing.Version
	if rec != nil {
		rec.Updated++
	}
	cover := fresh.HeaderImage
	if cover == "" && len(fresh.ImageURLs) > 0 {
		cover = fresh.ImageURLs[0]
	}
	if cover != "" {
		cover = hqURL(cover)
		if n := imgFilename(cover); a.store.ImageExists(n) {
			cover = "/f95zone/images/" + n
		}
	}
	n := Notification{
		Link: fresh.Link, Title: fresh.Title, Kind: kind, OldVersion: oldVersion, NewVersion: fresh.Version,
		Cover: cover, Labels: fresh.Labels, Tags: fresh.Tags, CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	id, err := a.store.db.AddNotification(n)
	if err != nil {
		a.log.Warn("Could not save notification for %s: %v", trunc(fresh.Title, 40), err)
		return
	}
	verb := "New release"
	if kind == "update" {
		verb = fmt.Sprintf("Updated %s -> %s", orDefault(oldVersion, "?"), fresh.Version)
	}
	a.log.Info("Notification: %s - %s", verb, trunc(fresh.Title, 60))
	if cfg.PushoverEnabled {
		title := "New: " + fresh.Title
		if kind == "update" {
			title = "Updated: " + fresh.Title
		}
		msg := verb
		if len(fresh.Tags) > 0 {
			msg += "\n" + joinLimited(fresh.Tags, 6)
		}
		if pushErr := SendPushover(cfg, title, msg, fresh.Link); pushErr != "" {
			a.log.Warn("Pushover notification failed: %s", pushErr)
			a.store.db.MarkPushed(id, pushErr)
		} else {
			a.store.db.MarkPushed(id, "")
		}
	}
}

const failureAlertDateKey = "failure_alert_date"
const loginAlertDateKey = "login_alert_date"

// RunLoginCheckScheduler periodically re-probes CheckF95Login independently
// of the scrape schedule, so a cookie that quietly expires is caught (and
// Pushover-alerted) even during a stretch where nothing new needs enriching
// and the main pipeline's own alerting never gets a chance to fire.
func (a *App) RunLoginCheckScheduler(ctx context.Context) {
	for {
		cfg := a.cfg.Get()
		var timer <-chan time.Time
		if cfg.LoginCheckHours > 0 {
			timer = time.After(time.Duration(cfg.LoginCheckHours) * time.Hour)
		}
		select {
		case <-ctx.Done():
			return
		case <-a.cfg.changed:
			continue
		case <-timer:
		}
		cfg = a.cfg.Get()
		if cfg.LoginCheckHours <= 0 || strings.TrimSpace(cfg.SiteCookie) == "" {
			continue
		}
		if a.IsRunning() {
			// a scrape/backfill is already using the browser - skip this tick
			// rather than queue behind it; the next tick will catch up.
			continue
		}
		res, err := a.CheckF95Login(ctx)
		if err != nil {
			a.log.Warn("Periodic login check failed: %v", err)
			continue
		}
		if res.LoggedIn {
			a.log.Info("Periodic login check: still logged in (%s)", res.Detail)
			continue
		}
		a.log.Warn("Periodic login check: %s", res.Detail)
		a.notifyLoginFailure(cfg, res.Detail)
	}
}

// notifyLoginFailure sends a Pushover alert when the periodic login check
// finds the site_cookie no longer works, at most once per UTC day.
func (a *App) notifyLoginFailure(cfg Config, detail string) {
	if !cfg.PushoverEnabled {
		return
	}
	today := time.Now().UTC().Format("2006-01-02")
	if v, ok := a.store.db.GetSetting(loginAlertDateKey); ok && v == today {
		return
	}
	if pushErr := SendPushover(cfg, "F95Zone Release Monitor: login check failed", detail, cfg.PublicBaseURL); pushErr != "" {
		a.log.Warn("Pushover login-failure alert could not be sent: %s", pushErr)
		return
	}
	a.store.db.SetSetting(loginAlertDateKey, today)
	a.log.Info("Pushover login-failure alert sent (further login alerts are suppressed until tomorrow)")
}

// notifyFailure sends a Pushover alert for a pipeline/backfill/auth failure,
// but at most once per UTC day, so a scheduler that keeps retrying every few
// hours doesn't spam the phone with the same underlying problem.
func (a *App) notifyFailure(cfg Config, context, msg string) {
	if !cfg.PushoverEnabled {
		return
	}
	today := time.Now().UTC().Format("2006-01-02")
	if v, ok := a.store.db.GetSetting(failureAlertDateKey); ok && v == today {
		return
	}
	title := "F95Zone RSS: " + context + " failed"
	if pushErr := SendPushover(cfg, title, msg, cfg.PublicBaseURL); pushErr != "" {
		a.log.Warn("Pushover failure alert could not be sent: %s", pushErr)
		return
	}
	a.store.db.SetSetting(failureAlertDateKey, today)
	a.log.Info("Pushover failure alert sent (further failure alerts are suppressed until tomorrow)")
}

func joinLimited(v []string, n int) string {
	if len(v) > n {
		v = v[:n]
	}
	s := ""
	for i, x := range v {
		if i > 0 {
			s += ", "
		}
		s += x
	}
	return s
}

// ReenrichOne re-scrapes a single thread and rebuilds the feed if it's part of it.
func (a *App) ReenrichOne(link string) error {
	existing, ok := a.store.db.GetRelease(link)
	if !ok {
		return errors.New("release not found in the database")
	}
	ctx, err := a.begin("manual")
	if err != nil {
		return err
	}
	go func() {
		cfg := a.cfg.Get()
		status := "IDLE"
		defer func() { a.end(status) }()
		var f Fetcher
		if cfg.FetchMode == "browser" {
			bf, err := newBrowserFetcher(ctx, cfg)
			if err != nil {
				a.log.Error("%v", err)
				status = "ERROR"
				return
			}
			f = bf
		} else {
			f = newHTTPFetcher(cfg)
		}
		defer f.Close()
		a.setProgress(func(p *Progress) {
			p.Status = "ENRICHING"
			p.Total = 1
			p.Current = 1
			p.Item = trunc(existing.Title, 60)
		})
		it := existing
		if err := a.enrichRelease(ctx, f, &it, cfg); err != nil {
			a.log.Warn("Re-enrich failed: %v", err)
			it.EnrichError = err.Error()
			status = "ERROR"
		} else {
			for _, u := range it.ImageURLs {
				if _, _, err := a.downloadImage(ctx, cfg, u, link); err != nil {
					a.log.Warn("Image download error: %v", err)
				}
			}
			a.store.SaveManifest()
			status = "DONE"
		}
		a.checkAndNotify(cfg, true, existing, it, nil)
		if err := a.store.db.UpsertRelease(it, existing.DiscoveredVia); err != nil {
			a.log.Warn("Could not save %s: %v", trunc(link, 60), err)
		}
		if fw, ok := a.loadFeedWindow(); ok {
			for _, l := range fw.Links {
				if l == link {
					if rel := loadWindow(a, fw.Links); rel != nil {
						a.GenerateFeed(rel)
					}
					break
				}
			}
		}
	}()
	return nil
}

func loadWindow(a *App, links []string) []Release {
	var out []Release
	for _, l := range links {
		if r, ok := a.store.db.GetRelease(l); ok {
			out = append(out, r)
		}
	}
	return out
}

// RebuildFeed rebuilds feed.xml from the current feed window without touching the network.
func (a *App) RebuildFeed() (int, error) {
	if a.IsRunning() {
		return 0, ErrBusy
	}
	fw, ok := a.loadFeedWindow()
	if !ok || len(fw.Links) == 0 {
		return 0, errors.New("no feed window yet - run a scrape first")
	}
	rel := loadWindow(a, fw.Links)
	if len(rel) == 0 {
		return 0, errors.New("the releases in the feed window are no longer in the database")
	}
	return len(rel), a.GenerateFeed(rel)
}

// ── scheduler: same semantics as cron hour="*/N" (minute 0, UTC) ──

func nextCronTime(now time.Time, n int) time.Time {
	t := now.UTC().Truncate(time.Hour).Add(time.Hour)
	for t.Hour()%n != 0 {
		t = t.Add(time.Hour)
	}
	return t
}

func (a *App) NextRun() time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.nextRun
}

func (a *App) RunScheduler(ctx context.Context) {
	if cfg := a.cfg.Get(); cfg.RunOnStart {
		go func() {
			time.Sleep(time.Second)
			if err := a.StartRun("startup", false, false); err != nil {
				a.log.Warn("Startup run skipped: %v", err)
			}
		}()
	}
	for {
		cfg := a.cfg.Get()
		var timer <-chan time.Time
		a.mu.Lock()
		if cfg.SchedulerEnabled {
			a.nextRun = nextCronTime(time.Now(), cfg.ScheduleHours)
			timer = time.After(time.Until(a.nextRun))
		} else {
			a.nextRun = time.Time{}
		}
		a.mu.Unlock()
		if cfg.SchedulerEnabled {
			a.log.Info("Scheduler: every %d hours, next run %s", cfg.ScheduleHours, a.nextRun.Local().Format("2006-01-02 15:04 MST"))
		} else {
			a.log.Info("Scheduler paused")
		}
		select {
		case <-ctx.Done():
			return
		case <-a.cfg.changed:
			continue
		case <-timer:
			if err := a.StartRun("schedule", false, false); err != nil {
				a.log.Warn("Scheduled run skipped: %v", err)
			}
		}
	}
}

// ── backfill: harvest older releases from the client-rendered listing pages ──

// fetchListingItems walks up to `pages` pages of F95zone's own listing JSON
// API (listDataURLTemplate - the same one backfill uses) and returns every
// thread found, in listing order, deduped only against other links seen in
// this same walk. Unlike backfill's discovery loop this deliberately does
// NOT skip links already in the database - the regular pipeline needs to see
// already-known threads again too, so it can pick up a version bump on one
// (checkAndNotify) and not just brand-new releases.
func (a *App) fetchListingItems(ctx context.Context, cfg Config, pages int) ([]Release, error) {
	if pages < 1 {
		pages = 1
	}
	seen := map[string]bool{}
	var out []Release
	failures := 0
	for page := 1; page <= pages; page++ {
		if ctx.Err() != nil {
			break
		}
		a.setProgress(func(p *Progress) { p.Status = "FETCHING"; p.Item = fmt.Sprintf("listing page %d/%d", page, pages) })
		a.log.Info("Listing: loading page %d/%d", page, pages)
		items, totalPages, err := a.fetchListingDataPage(ctx, cfg, page)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			a.log.Warn("Listing: page %d failed to load: %v", page, err)
			failures++
			if failures >= 2 {
				a.log.Warn("Listing: stopping after repeated page failures")
				break
			}
			continue
		}
		added := 0
		for _, it := range items {
			if seen[it.Link] {
				continue
			}
			seen[it.Link] = true
			out = append(out, it)
			added++
		}
		a.log.Info("Listing: page %d/%d - %d thread link(s)", page, totalPages, added)
		if totalPages > 0 && page >= totalPages {
			break
		}
		if page < pages {
			select {
			case <-ctx.Done():
			case <-time.After(time.Duration(cfg.RateLimitMS) * time.Millisecond):
			}
		}
	}
	return out, nil
}

func (a *App) StartBackfill(pages int) error {
	if pages < 1 {
		pages = 1
	}
	if pages > 200 {
		pages = 200
	}
	ctx, err := a.begin("backfill")
	if err != nil {
		return err
	}
	go a.runBackfill(ctx, pages)
	return nil
}

func (a *App) runBackfill(ctx context.Context, pages int) {
	cfg := a.cfg.Get()
	rec := RunRecord{Trigger: "backfill", Started: time.Now()}
	finish := func(status, errMsg string) {
		rec.Status, rec.Error = status, errMsg
		rec.Finished = time.Now()
		rec.Duration = rec.Finished.Sub(rec.Started).Seconds()
		a.store.AddRun(rec)
		ps := map[string]string{"ok": "DONE", "error": "ERROR", "cancelled": "CANCELLED"}[status]
		if status == "error" {
			a.notifyFailure(cfg, "Backfill", errMsg)
		}
		a.end(ps)
	}
	defer func() {
		if r := recover(); r != nil {
			a.log.Error("Backfill panic: %v", r)
			finish("error", "panic")
		}
	}()

	a.log.Info("============================================================")
	a.log.Info("Starting backfill: up to %d listing page(s)", pages)

	existing := a.store.db.AllLinks()
	seen := map[string]bool{}
	var discovered []Release
	consecutiveEmpty := 0
	failures := 0

	for page := 1; page <= pages; page++ {
		if ctx.Err() != nil {
			break
		}
		a.setProgress(func(p *Progress) { p.Status = "FETCHING"; p.Item = fmt.Sprintf("listing page %d/%d", page, pages) })
		a.log.Info("Backfill: loading listing page %d/%d", page, pages)
		items, totalPages, err := a.fetchListingDataPage(ctx, cfg, page)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			a.log.Warn("Backfill: page %d failed to load: %v", page, err)
			failures++
			if failures >= 2 {
				a.log.Warn("Backfill: stopping after repeated failures")
				break
			}
			continue
		}
		fresh := 0
		for _, it := range items {
			if existing[it.Link] || seen[it.Link] {
				continue
			}
			seen[it.Link] = true
			discovered = append(discovered, it)
			fresh++
		}
		a.log.Info("Backfill: page %d/%d - %d thread link(s) found, %d new", page, totalPages, len(items), fresh)
		if fresh == 0 {
			consecutiveEmpty++
		} else {
			consecutiveEmpty = 0
		}
		if consecutiveEmpty >= 3 {
			a.log.Info("Backfill: 3 pages in a row with nothing new, stopping early")
			break
		}
		if totalPages > 0 && page >= totalPages {
			break
		}
		if page < pages {
			select {
			case <-ctx.Done():
			case <-time.After(time.Duration(cfg.RateLimitMS) * time.Millisecond):
			}
		}
	}

	rec.Items = len(discovered)
	if len(discovered) == 0 {
		a.log.Info("Backfill: no new threads discovered")
		finish("ok", "")
		return
	}
	a.log.Info("Backfill: %d new thread(s) discovered - enriching...", len(discovered))
	a.setProgress(func(p *Progress) { p.Status = "ENRICHING"; p.Total = len(discovered); p.Current = 0 })

	var fetcher Fetcher
	if cfg.FetchMode == "browser" {
		bf, err := newBrowserFetcher(ctx, cfg)
		if err != nil {
			a.log.Error("Backfill: could not start Chromium for enrichment: %v", err)
			finish("error", err.Error())
			return
		}
		defer bf.Close()
		fetcher = bf
	} else {
		fetcher = newHTTPFetcher(cfg)
		defer fetcher.Close()
	}

	for i, it := range discovered {
		if ctx.Err() != nil {
			break
		}
		a.setProgress(func(p *Progress) { p.Current = i + 1; p.Item = trunc(it.Title, 60) })
		a.log.Info("[backfill %d/%d] %s", i+1, len(discovered), trunc(it.Title, 50))
		if err := a.enrichRelease(ctx, fetcher, &it, cfg); err != nil {
			if ctx.Err() != nil {
				break
			}
			a.log.Warn("Backfill enrichment failed for %s: %v", trunc(it.Link, 60), err)
			it.EnrichError = err.Error()
			rec.Failed++
		} else {
			rec.Enriched++
		}
		for _, u := range it.ImageURLs {
			if ctx.Err() != nil {
				break
			}
			if _, isNew, err := a.downloadImage(ctx, cfg, u, it.Link); err == nil && isNew {
				rec.ImagesNew++
			}
		}
		a.store.SaveManifest()
		if err := a.store.db.UpsertRelease(it, "backfill"); err != nil {
			a.log.Warn("Could not save %s: %v", trunc(it.Link, 60), err)
		}
		if i < len(discovered)-1 {
			select {
			case <-ctx.Done():
			case <-time.After(time.Duration(cfg.RateLimitMS) * time.Millisecond):
			}
		}
	}

	if ctx.Err() != nil {
		a.log.Warn("Backfill cancelled - threads enriched so far are already saved")
		finish("cancelled", "")
		return
	}
	el := time.Since(rec.Started)
	a.log.Info("Backfill complete! %d new threads, %d enriched, %d failed in %.1fs", len(discovered), rec.Enriched, rec.Failed, el.Seconds())
	a.log.Info("============================================================")
	finish("ok", "")
}
