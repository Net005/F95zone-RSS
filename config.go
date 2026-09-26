package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// normalizeSiteCookie fixes the single most common way a pasted site_cookie
// value breaks silently: cookie pairs joined with "," instead of the "; "
// an HTTP Cookie header actually requires (easy to end up with when copying
// individual cookie rows out of a browser's dev tools one at a time and
// joining them by hand). A raw header copied straight from the Network tab
// already uses "; " and is left untouched. Without this, a comma-joined
// value is sent as a single malformed Cookie header - the server parses it
// as one giant cookie and every pair after the first comma (commonly
// xf_session) is silently dropped, so the session never looks logged in no
// matter how fresh the cookie is.
func normalizeSiteCookie(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.Contains(raw, ";") {
		return raw
	}
	parts := strings.Split(raw, ",")
	if len(parts) < 2 {
		return raw
	}
	pairRe := regexp.MustCompile(`^[A-Za-z0-9_\-]+=.+$`)
	for i, p := range parts {
		p = strings.TrimSpace(p)
		if !pairRe.MatchString(p) {
			return raw // doesn't look like comma-joined cookie pairs - leave it alone
		}
		parts[i] = p
	}
	return strings.Join(parts, "; ")
}

const (
	appVersion       = "6.5.6"
	defaultPort      = 6069
	f95BaseURL       = "https://f95zone.to"
	defaultRSSSource = f95BaseURL + "/sam/latest_alpha/latest_data.php?cmd=rss&cat=games&rows=90"
	defaultUA        = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/121.0.0.0 Safari/537.36"
	// defaultBackfillTemplate is the Angular "latest alpha" listing (not the plain RSS endpoint,
	// which has no paging). It's a client-rendered page, so backfill always uses the headless
	// browser to load it. "%d" is replaced with the page number.
	defaultBackfillTemplate = f95BaseURL + "/sam/latest_alpha/#/cat=games/page=%d"

	// parserVersion is bumped whenever enrichment extraction logic changes (tag/engine/version
	// parsing, spoiler handling, ...). Rows enriched by an older parser are always re-scraped once,
	// regardless of reuse_unchanged, so an upgrade like this one corrects existing history.
	// v3 (6.4.0): tags now merge in the thread header's own tag list, engine/label now read from
	// the thread's own prefix badges, and the Overview paragraph is extracted as its own field.
	// v4 (6.5.0): the site_cookie comma-vs-semicolon bug (fixed in normalizeSiteCookie) meant a
	// lot of existing rows were enriched as a logged-out guest even with a cookie configured, so
	// this bump forces one more re-scrape pass to pick up Genre/tags/Overview now that login works.
	// v5 (6.5.1): Overview now keeps its paragraph breaks (stripHTMLParagraphs) instead of being
	// flattened into one run-on line - bumped so existing rows get reformatted, not just new ones.
	// v6 (6.5.2): Changelog and Downloads/Links are now extracted as their own fields.
	// v7 (6.5.6): thread_updated_iso is now populated on every enrichment so the default
	// sort can reflect the thread's real last-update time (matching F95zone's own
	// latest-alpha listing) instead of freezing on the release's original publish date.
	parserVersion = 7
)

// Config mirrors the original settings.json keys and adds a few new ones.
// Missing keys keep their defaults, so old settings files keep working.
type Config struct {
	ScheduleHours int `json:"schedule_hours"`
	// RSSSource is kept only so older settings.json/database values still decode; scheduled/manual
	// runs no longer fetch it (see ScheduledPages) - F95zone's source RSS is capped at ~90 rows and
	// doesn't reflect the real listing order, so every run walks the paged listing API instead.
	RSSSource string `json:"rss_source"`
	// BackfillURLTemplate is likewise kept only for old settings.json/database compatibility -
	// backfill and scheduled runs both discover threads via F95zone's own listDataURLTemplate JSON
	// API now (plain HTTP, no login or headless browser needed for discovery), not this template.
	// ScheduledPages: how many pages of F95zone's own listing API (same one Dashboard's manual
	// backfill uses) a normal scheduled/manual run walks to discover both new and already-known
	// threads (existing threads are re-checked for a version bump, not skipped).
	ScheduledPages int `json:"scheduled_pages"`
	// LoginCheckHours: how often a background check re-probes CheckF95Login independently of
	// the scrape schedule, so a cookie that quietly expired is caught (and Pushover-alerted)
	// even during a stretch with nothing new to enrich. 0 disables the periodic check.
	LoginCheckHours  int     `json:"login_check_hours"`
	MaxImages        int     `json:"max_images_per_release"`
	RateLimitMS      int     `json:"rate_limit_ms"`
	CacheTTLHours    float64 `json:"cache_ttl_hours"`
	PublicBaseURL    string  `json:"public_base_url"`
	FetchMode        string  `json:"fetch_mode"` // "http" or "browser"
	ChromePath       string  `json:"chrome_path"`
	UserAgent        string  `json:"user_agent"`
	RunOnStart       bool    `json:"run_on_start"`
	SchedulerEnabled bool    `json:"scheduler_enabled"`
	ReuseUnchanged   bool    `json:"reuse_unchanged"`

	// SiteCookie: raw "Cookie:" header value copied from a logged-in browser session
	// (e.g. "xf_user=...; xf_session=..."). Used for both HTTP and browser fetch modes so
	// enrichment and backfill see the same content a logged-in member would.
	SiteCookie string `json:"site_cookie"`

	BackfillURLTemplate string `json:"backfill_url_template"`

	// NotifyNew is kept only so older settings.json/database values still decode;
	// notifications are gated on a release's "watched" flag now (see checkAndNotify
	// in pipeline.go), and a release can't be watched before it's been seen once,
	// so a "new release" notification can no longer fire.
	NotifyNew    bool `json:"notify_new"`
	NotifyUpdate bool `json:"notify_update"`

	PushoverEnabled  bool   `json:"pushover_enabled"`
	PushoverUserKey  string `json:"pushover_user_key"`
	PushoverAPIToken string `json:"pushover_api_token"`
	PushoverPriority int    `json:"pushover_priority"` // -2..2, Pushover's own range

	// HoverSlideshow*: hovering a release tile in the Releases grid cycles
	// through its screenshots in a loop, reverting to the static cover on
	// mouse-leave. Purely a front-end feature; these just persist the toggle
	// and per-image delay like any other setting.
	HoverSlideshowEnabled bool `json:"hover_slideshow_enabled"`
	HoverSlideshowDelayMS int  `json:"hover_slideshow_delay_ms"`
	// HoverSlideshowStartDelayMS: how long the mouse has to stay over a tile before the
	// slideshow takes over from the static cover - distinct from HoverSlideshowDelayMS, which
	// is the interval BETWEEN frames once it's running. Without this, just passing the mouse
	// over a tile on the way to something else starts fetching/cycling images for nothing.
	HoverSlideshowStartDelayMS int `json:"hover_slideshow_start_delay_ms"`
}

func defaultConfig() Config {
	return Config{
		ScheduleHours:              12,
		RSSSource:                  defaultRSSSource,
		ScheduledPages:             3,
		LoginCheckHours:            6,
		MaxImages:                  8,
		RateLimitMS:                800,
		CacheTTLHours:              2,
		PublicBaseURL:              "https://f95-rss.bondt.network",
		FetchMode:                  "http",
		UserAgent:                  defaultUA,
		RunOnStart:                 true,
		SchedulerEnabled:           true,
		ReuseUnchanged:             true,
		BackfillURLTemplate:        defaultBackfillTemplate,
		NotifyNew:                  true,
		NotifyUpdate:               true,
		HoverSlideshowEnabled:      true,
		HoverSlideshowDelayMS:      900,
		HoverSlideshowStartDelayMS: 400,
	}
}

func (c *Config) Validate() error {
	c.PublicBaseURL = strings.TrimRight(strings.TrimSpace(c.PublicBaseURL), "/")
	c.RSSSource = strings.TrimSpace(c.RSSSource)
	if c.ScheduleHours < 1 || c.ScheduleHours > 24 {
		return errors.New("schedule_hours must be between 1 and 24")
	}
	if c.ScheduledPages < 1 || c.ScheduledPages > 50 {
		return errors.New("scheduled_pages must be between 1 and 50")
	}
	if c.LoginCheckHours < 0 || c.LoginCheckHours > 24 {
		return errors.New("login_check_hours must be between 0 (disabled) and 24")
	}
	if u, err := url.Parse(c.RSSSource); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("rss_source must be a valid http(s) URL")
	}
	if c.PublicBaseURL != "" {
		if u, err := url.Parse(c.PublicBaseURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return errors.New("public_base_url must be a valid http(s) URL")
		}
	}
	if c.MaxImages < 0 || c.MaxImages > 50 {
		return errors.New("max_images_per_release must be between 0 and 50")
	}
	if c.RateLimitMS < 0 || c.RateLimitMS > 60000 {
		return errors.New("rate_limit_ms must be between 0 and 60000")
	}
	if c.CacheTTLHours < 0 || c.CacheTTLHours > 720 {
		return errors.New("cache_ttl_hours must be between 0 and 720")
	}
	if c.FetchMode != "http" && c.FetchMode != "browser" {
		return errors.New(`fetch_mode must be "http" or "browser"`)
	}
	if strings.TrimSpace(c.UserAgent) == "" {
		c.UserAgent = defaultUA
	}
	c.SiteCookie = normalizeSiteCookie(c.SiteCookie)
	if strings.TrimSpace(c.BackfillURLTemplate) == "" {
		c.BackfillURLTemplate = defaultBackfillTemplate
	}
	if !strings.Contains(c.BackfillURLTemplate, "%d") {
		return errors.New("backfill_url_template must contain a %d page placeholder")
	}
	if c.PushoverPriority < -2 || c.PushoverPriority > 2 {
		return errors.New("pushover_priority must be between -2 and 2")
	}
	if c.HoverSlideshowDelayMS < 100 || c.HoverSlideshowDelayMS > 10000 {
		return errors.New("hover_slideshow_delay_ms must be between 100 and 10000")
	}
	if c.HoverSlideshowStartDelayMS < 0 || c.HoverSlideshowStartDelayMS > 5000 {
		return errors.New("hover_slideshow_start_delay_ms must be between 0 and 5000")
	}
	if c.PushoverEnabled && (strings.TrimSpace(c.PushoverUserKey) == "" || strings.TrimSpace(c.PushoverAPIToken) == "") {
		return errors.New("pushover_user_key and pushover_api_token are required when Pushover is enabled")
	}
	return nil
}

type ConfigStore struct {
	mu      sync.RWMutex
	cfg     Config
	db      *DB
	changed chan struct{}
}

const cfgKey = "config"

// NewConfigStore loads settings from the database. A legacy config/settings.json,
// if present, is imported once and renamed to settings.json.imported.
func NewConfigStore(db *DB, legacyPath string, logf func(level, msg string)) *ConfigStore {
	cs := &ConfigStore{db: db, cfg: defaultConfig(), changed: make(chan struct{}, 1)}
	if v, ok := db.GetSetting(cfgKey); ok {
		c := defaultConfig()
		if err := json.Unmarshal([]byte(v), &c); err == nil && c.Validate() == nil {
			cs.cfg = c
		} else {
			logf("ERROR", "stored settings invalid, using defaults")
		}
	} else {
		cs.save()
	}
	if b, err := os.ReadFile(legacyPath); err == nil {
		c := defaultConfig()
		if err := json.Unmarshal(b, &c); err != nil {
			logf("ERROR", fmt.Sprintf("legacy settings file %s could not be parsed, left in place: %v", legacyPath, err))
		} else if err := c.Validate(); err != nil {
			logf("ERROR", fmt.Sprintf("legacy settings file %s is invalid, left in place: %v", legacyPath, err))
		} else {
			cs.cfg = c
			cs.save()
			dst := legacyPath + ".imported"
			if _, err := os.Stat(dst); err == nil {
				dst = fmt.Sprintf("%s.imported.%d", legacyPath, time.Now().Unix())
			}
			if err := os.Rename(legacyPath, dst); err != nil {
				logf("WARN", fmt.Sprintf("settings imported but %s could not be renamed: %v", legacyPath, err))
			} else {
				logf("INFO", fmt.Sprintf("Imported legacy settings from %s (renamed to %s). Settings are now managed in the web UI.", legacyPath, filepath.Base(dst)))
			}
		}
	}
	return cs
}

func (cs *ConfigStore) Get() Config {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.cfg
}

func (cs *ConfigStore) Set(c Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	cs.mu.Lock()
	cs.cfg = c
	err := cs.save()
	cs.mu.Unlock()
	select {
	case cs.changed <- struct{}{}:
	default:
	}
	return err
}

func (cs *ConfigStore) save() error {
	b, _ := json.Marshal(cs.cfg)
	return cs.db.SetSetting(cfgKey, string(b))
}
