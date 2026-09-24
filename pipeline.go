package main

import (
	"context"
	"errors"
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

// begin claims the single-run slot.
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

func (a *App) runPipeline(ctx context.Context, trigger string, force, full bool) {
	cfg := a.cfg.Get()
	rec := RunRecord{Trigger: trigger, Started: time.Now()}
	finish := func(status, errMsg string) {
		rec.Status, rec.Error = status, errMsg
		rec.Finished = time.Now()
		rec.Duration = rec.Finished.Sub(rec.Started).Seconds()
		a.store.AddRun(rec)
		ps := map[string]string{"ok": "DONE", "cache": "DONE", "error": "ERROR", "cancelled": "CANCELLED"}[status]
		a.end(ps)
	}
	defer func() {
		if r := recover(); r != nil {
			a.log.Error("Pipeline panic: %v", r)
			finish("error", "panic")
		}
	}()

	if !force && a.store.CacheFresh(cfg.CacheTTLHours) {
		a.log.Info("Cache is fresh (< %gh), reusing cached data", cfg.CacheTTLHours)
		rel := a.store.Releases()
		if len(rel) > 0 {
			if err := a.GenerateFeed(rel); err != nil {
				a.log.Error("Feed build failed: %v", err)
				finish("error", err.Error())
				return
			}
			a.setProgress(func(p *Progress) { p.Percent = 100 })
			a.log.Info("RSS rebuilt from cache (%d items)", len(rel))
			rec.Items = len(rel)
			finish("cache", "")
			return
		}
	}

	a.log.Info("============================================================")
	a.log.Info("Starting F95Zone RSS enrichment pipeline (trigger: %s, mode: %s)", trigger, cfg.FetchMode)

	items, err := a.fetchSourceRSS(ctx, cfg)
	if err != nil || len(items) == 0 {
		msg := "no items in source RSS feed"
		if err != nil {
			msg = err.Error()
		}
		a.log.Error("%s", msg)
		finish("error", msg)
		return
	}
	rec.Items = len(items)
	a.log.Info("Source feed: %d items to enrich", len(items))
	a.setProgress(func(p *Progress) { p.Total = len(items); p.Status = "ENRICHING" })

	var prev map[string]Release
	if cfg.ReuseUnchanged && !full {
		prev = map[string]Release{}
		for _, r := range a.store.Releases() {
			prev[r.Link] = r
		}
	}

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
		a.log.Info("[%d/%d] %s", i+1, len(items), trunc(it.Title, 50))
		a.setProgress(func(p *Progress) { p.Current = i + 1; p.Item = trunc(it.Title, 60) })

		reused := false
		if old, ok := prev[it.Link]; ok && old.Title == it.Title && old.ExtraDescription != "" && old.EnrichError == "" {
			it.ExtraDescription, it.ImageURLs, it.HeaderImage, it.EnrichedAt = old.ExtraDescription, old.ImageURLs, old.HeaderImage, old.EnrichedAt
			reused = true
			rec.Reused++
			a.log.Info("  Unchanged since last run, reusing enrichment")
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
		out = append(out, it)

		if !reused && i < len(items)-1 {
			select {
			case <-ctx.Done():
			case <-time.After(time.Duration(cfg.RateLimitMS) * time.Millisecond):
			}
		}
	}

	if ctx.Err() != nil {
		a.log.Warn("Run cancelled after %d/%d items - feed and cache left untouched", len(out), len(items))
		finish("cancelled", "")
		return
	}
	if err := a.GenerateFeed(out); err != nil {
		a.log.Error("Feed build failed: %v", err)
		finish("error", err.Error())
		return
	}
	a.setProgress(func(p *Progress) { p.Percent = 100 })
	el := time.Since(rec.Started)
	a.log.Info("Pipeline complete! %d items in %.1fs (%d scraped, %d reused, %d failed, %d new images)",
		len(out), el.Seconds(), rec.Enriched, rec.Reused, rec.Failed, rec.ImagesNew)
	a.log.Info("============================================================")
	finish("ok", "")
}

// ReenrichOne re-scrapes a single thread and rebuilds the feed.
func (a *App) ReenrichOne(link string) error {
	if _, ok := a.store.Find(link); !ok {
		return errors.New("release not found in cache")
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
		rels := a.store.Releases()
		for i := range rels {
			if rels[i].Link != link {
				continue
			}
			a.setProgress(func(p *Progress) {
				p.Status = "ENRICHING"
				p.Total = 1
				p.Current = 1
				p.Item = trunc(rels[i].Title, 60)
			})
			if err := a.enrichRelease(ctx, f, &rels[i], cfg); err != nil {
				a.log.Warn("Re-enrich failed: %v", err)
				rels[i].EnrichError = err.Error()
				status = "ERROR"
			} else {
				for _, u := range rels[i].ImageURLs {
					if _, _, err := a.downloadImage(ctx, cfg, u, link); err != nil {
						a.log.Warn("Image download error: %v", err)
					}
				}
				a.store.SaveManifest()
				status = "DONE"
			}
			if err := a.GenerateFeed(rels); err != nil {
				a.log.Error("Feed build failed: %v", err)
			}
			return
		}
	}()
	return nil
}

func (a *App) RebuildFeed() (int, error) {
	if a.IsRunning() {
		return 0, ErrBusy
	}
	rel := a.store.Releases()
	if len(rel) == 0 {
		return 0, errors.New("cache is empty - run a scrape first")
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
