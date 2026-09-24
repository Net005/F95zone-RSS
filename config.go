package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	appVersion       = "5.0.0"
	defaultPort      = 6069
	f95BaseURL       = "https://f95zone.to"
	defaultRSSSource = f95BaseURL + "/sam/latest_alpha/latest_data.php?cmd=rss&cat=games&rows=90"
	defaultUA        = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/121.0.0.0 Safari/537.36"
)

// Config mirrors the original settings.json keys and adds a few new ones.
// Missing keys keep their defaults, so old settings files keep working.
type Config struct {
	ScheduleHours    int     `json:"schedule_hours"`
	RSSSource        string  `json:"rss_source"`
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
}

func defaultConfig() Config {
	return Config{
		ScheduleHours:    12,
		RSSSource:        defaultRSSSource,
		MaxImages:        8,
		RateLimitMS:      800,
		CacheTTLHours:    2,
		PublicBaseURL:    "https://f95-rss.bondt.network",
		FetchMode:        "http",
		UserAgent:        defaultUA,
		RunOnStart:       true,
		SchedulerEnabled: true,
		ReuseUnchanged:   true,
	}
}

func (c *Config) Validate() error {
	c.PublicBaseURL = strings.TrimRight(strings.TrimSpace(c.PublicBaseURL), "/")
	c.RSSSource = strings.TrimSpace(c.RSSSource)
	if c.ScheduleHours < 1 || c.ScheduleHours > 24 {
		return errors.New("schedule_hours must be between 1 and 24")
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

func atomicWrite(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
