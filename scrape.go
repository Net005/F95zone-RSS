package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/mail"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

// ──────────────────────────────────────────────────────────────
//  Source RSS
// ──────────────────────────────────────────────────────────────

type srcFeed struct {
	Channel struct {
		Items []struct {
			Title       string   `xml:"title"`
			Link        string   `xml:"link"`
			PubDate     string   `xml:"pubDate"`
			Description string   `xml:"description"`
			Categories  []string `xml:"category"`
		} `xml:"item"`
	} `xml:"channel"`
}

// parseFieldDate turns a thread metadata field's date text (e.g. the
// "Thread updated" or "Release date" field, always in the plain YYYY-MM-DD
// shape XenForo renders those in) into the same sortable ISO format
// PubDateISO uses, or "" if it doesn't look like a date at all.
func parseFieldDate(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.UTC().Format("2006-01-02T15:04:05-07:00")
	}
	return ""
}

func parseRSSDate(s string) string {
	t, err := mail.ParseDate(s)
	if err != nil {
		t = time.Now().UTC()
	}
	return t.Format("2006-01-02T15:04:05-07:00")
}

// fetchSourceRSS is no longer called by the pipeline or the login probe (see
// fetchListingItems) - F95zone's source RSS caps out at ~90 rows and doesn't
// reflect the real listing order, so every run now walks the same paged
// listing backfill uses instead. Left in place only in case cfg.RSSSource is
// ever useful again as a lightweight sanity check.
func (a *App) fetchSourceRSS(ctx context.Context, cfg Config) ([]Release, error) {
	a.log.Info("Fetching source RSS: %s", cfg.RSSSource)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", cfg.RSSSource, nil)
	req.Header.Set("User-Agent", cfg.UserAgent)
	req.Header.Set("Accept", "application/rss+xml, application/xml, text/xml")
	if cfg.SiteCookie != "" {
		req.Header.Set("Cookie", cfg.SiteCookie)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("RSS fetch error: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("RSS fetch failed: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	var f srcFeed
	if err := xml.Unmarshal(body, &f); err != nil {
		return nil, fmt.Errorf("XML parse error: %w", err)
	}
	a.log.Info("Found %d items in source feed", len(f.Channel.Items))
	var out []Release
	for _, it := range f.Channel.Items {
		r := Release{
			Title:             orDefault(strings.TrimSpace(it.Title), "Untitled"),
			Link:              strings.TrimSpace(it.Link),
			PubDateRaw:        strings.TrimSpace(it.PubDate),
			SourceDescription: strings.TrimSpace(it.Description),
			ImageURLs:         []string{},
		}
		r.PubDateISO = parseRSSDate(r.PubDateRaw)
		r.Labels, r.Engine, r.Version = parseTitleBrackets(r.Title)
		out = append(out, r)
	}
	return out, nil
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// ──────────────────────────────────────────────────────────────
//  Title bracket parsing: status labels, engine, version guess
// ──────────────────────────────────────────────────────────────

var bracketRe = regexp.MustCompile(`\[(.*?)\]`)

// engineNames mirrors F95zone's own "Prefix: Engine" list (ADRIFT, Flash, Godot, HTML,
// Java, Others, QSP, RAGS, RPGM, Ren'Py, Tads, Unity, Unreal Engine, WebGL, Wolf RPG),
// so the Engine filter's values match what the site itself uses as thread prefixes.
var engineNames = map[string]string{
	"rpgm": "RPGM", "rpg maker": "RPGM", "rpg maker mv": "RPGM", "rpg maker vx": "RPGM",
	"unity": "Unity", "ren'py": "Ren'Py", "renpy": "Ren'Py", "qsp": "QSP", "html": "HTML",
	"rags": "RAGS", "java": "Java", "flash": "Flash", "adrift": "ADRIFT", "wolf rpg": "Wolf RPG",
	"unreal engine": "Unreal Engine", "unreal": "Unreal Engine", "webgl": "WebGL", "godot": "Godot",
	"tads": "Tads", "others": "Others",
	// Not on F95zone's own engine list, but seen in the wild as title-bracket tokens -
	// kept so they're still recognized as an engine rather than mis-sorted as a label.
	"gamemaker": "Others", "ocean": "Others",
}

var statusWords = map[string]string{
	"update": "UPDATE", "new": "NEW", "completed": "Completed", "onhold": "Onhold", "on-hold": "Onhold",
	"abandoned": "Abandoned", "siterip": "SiteRip", "collection": "Collection", "poll": "Poll", "cheat mod": "Cheat Mod",
}

var versionGuessRe = regexp.MustCompile(`(?i)^(v|ver\.?|version)?\s*\d`)

// parseTitleBrackets reads every "[...]" token in a release title and sorts it
// into a status label, the engine, or a version guess (overridden later by the
// thread's own "Version:" field when present).
func parseTitleBrackets(title string) (labels []string, engine, version string) {
	seen := map[string]bool{}
	for _, m := range bracketRe.FindAllStringSubmatch(title, -1) {
		t := strings.TrimSpace(m[1])
		if t == "" {
			continue
		}
		lower := strings.ToLower(t)
		if canon, ok := engineNames[lower]; ok {
			engine = canon
			continue
		}
		if canon, ok := statusWords[lower]; ok {
			if !seen[canon] {
				labels = append(labels, canon)
				seen[canon] = true
			}
			continue
		}
		if versionGuessRe.MatchString(t) {
			version = t
			continue
		}
		if !seen[t] {
			labels = append(labels, t)
			seen[t] = true
		}
	}
	return
}

// ──────────────────────────────────────────────────────────────
//  Page fetchers (plain HTTP or headless Chromium)
// ──────────────────────────────────────────────────────────────

type Fetcher interface {
	Page(ctx context.Context, url string) (string, error)
	Close()
}

type httpFetcher struct {
	client *http.Client
	ua     string
	cookie string
}

func newHTTPFetcher(cfg Config) *httpFetcher {
	jar, _ := cookiejar.New(nil)
	return &httpFetcher{client: &http.Client{Jar: jar, Timeout: 60 * time.Second}, ua: cfg.UserAgent, cookie: cfg.SiteCookie}
}

func (f *httpFetcher) Page(ctx context.Context, u string) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	req.Header.Set("User-Agent", f.ua)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	if f.cookie != "" {
		req.Header.Set("Cookie", f.cookie)
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return "", err
	}
	body := string(b)
	if resp.StatusCode != 200 {
		if strings.Contains(body, "Just a moment") || strings.Contains(body, "cf-chl") {
			return "", fmt.Errorf("HTTP %d (Cloudflare challenge) - switch fetch_mode to \"browser\"", resp.StatusCode)
		}
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if strings.Contains(body, "Just a moment...") && !strings.Contains(body, "bbWrapper") {
		return "", errors.New("Cloudflare challenge - switch fetch_mode to \"browser\"")
	}
	return body, nil
}

func (f *httpFetcher) Close() {}

type browserFetcher struct {
	ctx         context.Context
	cancel      context.CancelFunc
	allocCancel context.CancelFunc
}

// siteCookies turns a raw "name=value; name2=value2" Cookie header into cookie
// params chromedp can install for the domain, so a logged-in session is visible
// to every page the browser loads (including the Angular listing used by backfill).
func siteCookies(raw string) []*network.CookieParam {
	var out []*network.CookieParam
	for _, part := range strings.Split(raw, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 || strings.TrimSpace(kv[0]) == "" {
			continue
		}
		out = append(out, &network.CookieParam{
			Name: strings.TrimSpace(kv[0]), Value: strings.TrimSpace(kv[1]),
			Domain: ".f95zone.to", Path: "/", Secure: true,
		})
	}
	return out
}

func newBrowserFetcher(parent context.Context, cfg Config) (*browserFetcher, error) {
	opts := append([]chromedp.ExecAllocatorOption{}, chromedp.DefaultExecAllocatorOptions[:]...)
	opts = append(opts,
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("disable-setuid-sandbox", true),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.Flag("disable-gpu", true),
		chromedp.WindowSize(1920, 1080),
		chromedp.UserAgent(cfg.UserAgent),
	)
	if p := firstNonEmpty(cfg.ChromePath, os.Getenv("CHROME_PATH")); p != "" {
		opts = append(opts, chromedp.ExecPath(p))
	}
	alloc, allocCancel := chromedp.NewExecAllocator(parent, opts...)
	ctx, cancel := chromedp.NewContext(alloc)
	if err := chromedp.Run(ctx); err != nil {
		cancel()
		allocCancel()
		return nil, fmt.Errorf("could not start Chromium: %w", err)
	}
	if cookies := siteCookies(cfg.SiteCookie); len(cookies) > 0 {
		setCookies := make([]chromedp.Action, 0, len(cookies)+1)
		setCookies = append(setCookies, network.Enable())
		for _, c := range cookies {
			cp := c
			setCookies = append(setCookies, chromedp.ActionFunc(func(ctx context.Context) error {
				return network.SetCookie(cp.Name, cp.Value).WithDomain(cp.Domain).WithPath(cp.Path).WithSecure(cp.Secure).Do(ctx)
			}))
		}
		if err := chromedp.Run(ctx, setCookies...); err != nil {
			cancel()
			allocCancel()
			return nil, fmt.Errorf("could not set session cookies: %w", err)
		}
	}
	return &browserFetcher{ctx: ctx, cancel: cancel, allocCancel: allocCancel}, nil
}

func (b *browserFetcher) Page(ctx context.Context, u string) (string, error) {
	tctx, cancel := context.WithTimeout(b.ctx, 75*time.Second)
	defer cancel()
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	var html string
	err := chromedp.Run(tctx,
		chromedp.Navigate(u),
		chromedp.WaitReady(".message-body, .bbWrapper", chromedp.ByQuery),
		chromedp.Sleep(2*time.Second),
		chromedp.OuterHTML("html", &html, chromedp.ByQuery),
	)
	return html, err
}

func (b *browserFetcher) Close() {
	b.cancel()
	b.allocCancel()
}

// ──────────────────────────────────────────────────────────────
//  Byparr fetcher: routes every request through a Byparr /
//  FlareSolverr-compatible instance instead of a direct HTTP call or a
//  locally-launched headless Chromium. Byparr runs its own patched
//  browser to solve whatever Cloudflare/bot-challenge sits in front of
//  the request and hands back the solved page plus cookies, which is
//  useful precisely when a plain request gets blocked, or a vanilla
//  chromedp browser gets fingerprinted and stalls (as opposed to being
//  outright rejected) - the "context deadline exceeded" hang some
//  bot-detection produces for automated-looking browsers.
// ──────────────────────────────────────────────────────────────

type byparrFetcher struct {
	client    *http.Client
	baseURL   string
	timeoutS  int
	ua        string
	sessionID string
}

type byparrRequest struct {
	Cmd        string `json:"cmd"`
	URL        string `json:"url"`
	MaxTimeout int    `json:"maxTimeout"`
	Session    string `json:"session,omitempty"`
	Cookies    []struct {
		Name   string `json:"name"`
		Value  string `json:"value"`
		Domain string `json:"domain"`
	} `json:"cookies,omitempty"`
}

type byparrSolution struct {
	URL      string `json:"url"`
	Status   int    `json:"status"`
	Response string `json:"response"`
	Cookies  []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"cookies"`
}

type byparrResponse struct {
	Status   string         `json:"status"`
	Message  string         `json:"message"`
	Solution byparrSolution `json:"solution"`
}

func newByparrFetcher(cfg Config) *byparrFetcher {
	return &byparrFetcher{
		client:   &http.Client{Timeout: time.Duration(cfg.ByparrTimeoutS+15) * time.Second},
		baseURL:  cfg.ByparrURL,
		timeoutS: cfg.ByparrTimeoutS,
		ua:       cfg.UserAgent,
	}
}

// solve sends a single "request.get" through Byparr and returns the solved
// page body plus the HTTP status Byparr reports F95zone responded with.
func (f *byparrFetcher) solve(ctx context.Context, u string, cookie string) (string, int, error) {
	reqBody := byparrRequest{Cmd: "request.get", URL: u, MaxTimeout: f.timeoutS * 1000, Session: f.sessionID}
	if cookie != "" {
		for _, part := range strings.Split(cookie, ";") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			kv := strings.SplitN(part, "=", 2)
			if len(kv) != 2 || strings.TrimSpace(kv[0]) == "" {
				continue
			}
			reqBody.Cookies = append(reqBody.Cookies, struct {
				Name   string `json:"name"`
				Value  string `json:"value"`
				Domain string `json:"domain"`
			}{Name: strings.TrimSpace(kv[0]), Value: strings.TrimSpace(kv[1]), Domain: ".f95zone.to"})
		}
	}
	b, err := json.Marshal(reqBody)
	if err != nil {
		return "", 0, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, "POST", f.baseURL+"/v1", bytes.NewReader(b))
	if err != nil {
		return "", 0, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := f.client.Do(httpReq)
	if err != nil {
		return "", 0, fmt.Errorf("byparr request failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return "", 0, err
	}
	if resp.StatusCode != 200 {
		return "", 0, fmt.Errorf("byparr HTTP %d: %s", resp.StatusCode, trunc(string(body), 300))
	}
	var br byparrResponse
	if err := json.Unmarshal(body, &br); err != nil {
		return "", 0, fmt.Errorf("could not parse byparr response: %w", err)
	}
	if br.Status != "ok" {
		return "", 0, fmt.Errorf("byparr could not solve %s: %s", u, br.Message)
	}
	return br.Solution.Response, br.Solution.Status, nil
}

// Page fetches a full HTML page (a thread page, the listing page, etc.)
// through Byparr, satisfying the Fetcher interface so it's a drop-in
// replacement for either httpFetcher or browserFetcher.
func (f *byparrFetcher) Page(ctx context.Context, u string) (string, error) {
	body, status, err := f.solve(ctx, u, "")
	if err != nil {
		return "", err
	}
	if status != 0 && status >= 400 {
		return "", fmt.Errorf("byparr: HTTP %d fetching %s", status, u)
	}
	return body, nil
}

func (f *byparrFetcher) Close() {}

// extractByparrJSON pulls raw JSON out of a Byparr solution's response
// field for a request that hit F95zone's JSON listing API rather than an
// HTML page. Byparr's underlying browser wraps a non-HTML response body
// in a plain document (typically "<html><head></head><body><pre>...json
// here...</pre></body></html>"), so a solved JSON endpoint isn't valid
// JSON as-is and needs that wrapper stripped first.
func extractByparrJSON(raw string) string {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "{") || strings.HasPrefix(raw, "[") {
		return raw
	}
	if m := byparrPreRe.FindStringSubmatch(raw); len(m) == 2 {
		return html.UnescapeString(m[1])
	}
	return raw
}

var byparrPreRe = regexp.MustCompile(`(?is)<pre[^>]*>(.*?)</pre>`)

// newFetcher centralizes fetch-mode selection: a configured Byparr instance
// takes over every request unconditionally, overriding fetch_mode entirely,
// since a Byparr instance solves Cloudflare-style challenges regardless of
// whether the caller would otherwise have used a plain HTTP request or a
// local headless-Chromium browser.
func newFetcher(ctx context.Context, cfg Config) (Fetcher, error) {
	if cfg.ByparrURL != "" {
		return newByparrFetcher(cfg), nil
	}
	if cfg.FetchMode == "browser" {
		return newBrowserFetcher(ctx, cfg)
	}
	return newHTTPFetcher(cfg), nil
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

// ──────────────────────────────────────────────────────────────
//  Thread page enrichment
// ──────────────────────────────────────────────────────────────

var (
	overviewRe = regexp.MustCompile(`(?i)<b[^>]*>\s*Overview\s*</b>\s*:?\s*`)
	nextSecRe  = regexp.MustCompile(`(?i)<b[^>]*>\s*(?:Installation|Thread\s*Updated|Developer|Publisher|Censorship|Censored|Version|OS|Language|Store|Genre|Length|Notes?|Changelog|Downloads?|Links?|Screenshots?)\s*[:</]`)
	ovTextRe   = regexp.MustCompile(`(?i)^\s*Overview\s*:?\s*`)
	skipImg    = []string{"avatar", "smilie", "emoji", "icon", "logo", "spinner", "loading", "styles", "thumbs", "ui-"}

	// fieldBoundaryRe finds every "<b>Label</b>:" heading used for the thread's
	// metadata block (Thread Updated, Version, Genre, ...). The value of each
	// field runs until the next heading.
	// Includes a few labels we don't keep (Installation, Changelog, ...) purely
	// so they terminate the previous field's value instead of being swallowed
	// into it - Genre in particular runs right up to Installation on most threads.
	fieldBoundaryRe = regexp.MustCompile(`(?i)<b[^>]*>\s*(Thread\s*Updated|Release\s*Date|Developer|Publisher|Censored|Censorship|Version|OS|Language|Store|Genre|Length|Installation|Changelog|Downloads?|Links?|Notes?)\s*</b>\s*:?\s*`)
	tagStripRe      = regexp.MustCompile(`<[^>]+>`)
	wsRe            = regexp.MustCompile(`\s+`)
)

// parseThreadPrefixes reads the thread title's own prefix badges
// (<h1 class="p-title-value"><a class="labelLink"><span class="label ...">TEXT</span></a> ...)
// which is F95zone's own authoritative engine/status classification for the
// thread - far more reliable than guessing the engine from title brackets.
// Of the (usually two) prefixes, whichever text matches engineNames (case
// insensitive) is the engine; the other, if any, is returned as a status label
// (Completed, Abandoned, Onhold, VN, ...).
func parseThreadPrefixes(doc *goquery.Document) (engine, label string) {
	h1 := doc.Find("h1.p-title-value").First()
	if h1.Length() == 0 {
		return "", ""
	}
	h1.Find("a.labelLink span.label").Each(func(_ int, s *goquery.Selection) {
		t := strings.TrimSpace(s.Text())
		if t == "" {
			return
		}
		if canon, ok := engineNames[strings.ToLower(t)]; ok {
			if engine == "" {
				engine = canon
			}
			return
		}
		if label == "" {
			label = t
		}
	})
	return
}

// mergeLabel adds label to labels if it isn't already present (case-insensitive).
func mergeLabel(labels []string, label string) []string {
	if label == "" {
		return labels
	}
	for _, l := range labels {
		if strings.EqualFold(l, label) {
			return labels
		}
	}
	return append(labels, label)
}

// mergeTags merges two tag slices, deduped case-insensitively, trimmed.
func mergeTags(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, list := range [][]string{a, b} {
		for _, t := range list {
			t = strings.TrimSpace(t)
			if t == "" || seen[strings.ToLower(t)] {
				continue
			}
			seen[strings.ToLower(t)] = true
			out = append(out, t)
		}
	}
	return out
}

// parseHeaderTags reads the thread header's own tag list
// (<dl class="tagList"><dd><span class="js-tagList"><a class="tagItem">tag</a>...)
// which F95zone renders for every thread regardless of login state, unlike the
// Genre line in the first post which may be gated or simply not present.
func parseHeaderTags(doc *goquery.Document) []string {
	var out []string
	doc.Find(".js-tagList .tagItem, dl.tagList a.tagItem").Each(func(_ int, s *goquery.Selection) {
		t := strings.TrimSpace(s.Text())
		if t != "" {
			out = append(out, t)
		}
	})
	return out
}

// extractOverview pulls just the "Overview:" paragraph out of the cleaned post
// HTML as its own field, separate from ExtraDescription (which may be a wider
// fallback slice of the post when no explicit Overview label exists). The
// value runs until the next "<b>Label</b>" metadata heading or a double <br>,
// whichever comes first.
// changelogFieldRe/downloadsFieldRe find the "<b>Changelog</b>:"/"<b>Downloads</b>:"
// (or "Links") heading. Kept separate from fieldBoundaryRe because Downloads
// needs the raw HTML (to pull href attributes out), not the plain-text value
// every other field extraction collapses down to.
var (
	changelogFieldRe = regexp.MustCompile(`(?i)<b[^>]*>\s*Changelog\s*</b>\s*:?\s*`)
	// Threads phrase this heading in more ways than a single word ("Download",
	// "Download Link", "Download Links", "Links", ...), so match any of those
	// combinations rather than one word alone.
	downloadsFieldRe = regexp.MustCompile(`(?i)<b[^>]*>\s*(?:Download\s*Links?|Downloads?|Links?)\s*</b>\s*:?\s*`)
	anchorRe         = regexp.MustCompile(`(?is)<a\b[^>]*\bhref="([^"]+)"[^>]*>(.*?)</a>`)
	// downloadHostRe is the fallback used when a thread has no recognizable
	// "Download(s)/Links" heading at all: a handful of link texts that are
	// almost always an actual file host rather than prose, used to recover
	// download links from a post whose heading extraction otherwise misses.
	downloadHostRe = regexp.MustCompile(`(?i)^(mega|pixeldrain|gofile|mixdrop|workupload|katfile|datanodes|buzzheavier|vikingfile|anonfiles|mediafire|gdrive|google\s*drive|dropbox|uploadhaven|rapidgator|filecrypt|streamtape|1fichier|racaty)$`)
	// downloadSkipHrefRe excludes screenshot/attachment links that end up
	// inside (or, absent a following heading, past the end of) a Downloads
	// section - those are images, never an actual release download.
	downloadSkipHrefRe = regexp.MustCompile(`(?i)attachments\.f95zone\.to|\.(?:jpe?g|png|gif|webp|bmp|svg)(?:\?.*)?$`)
)

// extractChangelog pulls the "Changelog:" section as its own field, same
// boundary logic as Overview but without stopping at the first double-<br> -
// a changelog is usually itself a list of version entries separated by
// double-<br>/<li>, all of which belong in the one field.
func extractChangelog(inner string) string {
	m := changelogFieldRe.FindStringIndex(inner)
	if m == nil {
		return ""
	}
	rest := inner[m[1]:]
	end := len(rest)
	if n := nextSecRe.FindStringIndex(rest); n != nil {
		end = n[0]
	}
	txt := stripGated(stripHTMLParagraphs(rest[:end]))
	if len([]rune(txt)) < 3 {
		return ""
	}
	return txt
}

// extractDownloadLinks pulls every <a href="..."> out of the "Downloads:"/
// "Links:" section. Only http(s) links are kept (the odd relative/anchor
// link that slips into that section, e.g. a "how to install" jump link, is
// not a download).
func extractDownloadLinks(inner string) []DownloadLink {
	m := downloadsFieldRe.FindStringIndex(inner)
	var seg string
	if m != nil {
		rest := inner[m[1]:]
		end := len(rest)
		if n := nextSecRe.FindStringIndex(rest); n != nil {
			end = n[0]
		}
		seg = rest[:end]
	} else {
		// No "Download(s)/Links" heading found at all - fall back to
		// scanning the whole post for anchors whose own text names a known
		// file host, rather than showing no downloads for that release.
		seg = inner
	}
	seen := map[string]bool{}
	var out []DownloadLink
	for _, mm := range anchorRe.FindAllStringSubmatch(seg, -1) {
		href := strings.TrimSpace(html.UnescapeString(mm[1]))
		if !strings.HasPrefix(href, "http") || seen[href] || downloadSkipHrefRe.MatchString(href) {
			continue
		}
		text := strings.TrimSpace(stripGated(stripHTML(mm[2])))
		if m == nil && !downloadHostRe.MatchString(text) {
			continue
		}
		if text == "" {
			text = href
		}
		seen[href] = true
		out = append(out, DownloadLink{Host: text, URL: href})
	}
	return out
}

func extractOverview(inner string) string {
	m := overviewRe.FindStringIndex(inner)
	if m == nil {
		return ""
	}
	rest := inner[m[1]:]
	end := len(rest)
	if n := nextSecRe.FindStringIndex(rest); n != nil {
		end = n[0]
	}
	if i := strings.Index(rest[:end], "<br><br>"); i != -1 {
		end = i
	}
	if i := strings.Index(rest[:end], "<br/><br/>"); i != -1 && i < end {
		end = i
	}
	txt := stripGated(stripHTMLParagraphs(rest[:end]))
	if len([]rune(txt)) < 5 {
		return ""
	}
	return txt
}

func findPost(doc *goquery.Document) *goquery.Selection {
	for _, sel := range []string{".bbWrapper", ".message-cell--main", ".message-main", ".threadmark-content"} {
		if s := doc.Find(sel).First(); s.Length() > 0 {
			return s
		}
	}
	return nil
}

// unwrapSpoilers keeps a spoiler's content in place (F95zone renders it into
// the page HTML for guests too, just visually collapsed) and drops only the
// "SPOILER" toggle button/title around it. Earlier versions of this scraper
// removed spoilers outright, which silently ate the Genre line on most threads.
//
// .bbCodeBlock--spoiler is unwrapped the same way as .bbCodeSpoiler: for a
// logged-in session (site_cookie set to a valid cookie) F95zone renders the
// real field content inside it, same as any other spoiler. Only for a guest
// (or an expired cookie) does it contain the fixed "you don't have permission
// to view the spoiler content" notice instead - stripGated() is the safety
// net that scrubs that sentence back out of extracted field/description text
// so it never ends up stored as a "genre" or as the description.
func unwrapSpoilers(post *goquery.Selection) {
	for i := 0; i < 6; i++ {
		sp := post.Find(".bbCodeSpoiler, .bbCodeBlock--spoiler")
		if sp.Length() == 0 {
			return
		}
		sp.Each(func(_ int, s *goquery.Selection) {
			inner := s.Find(".bbCodeSpoiler-content, .bbCodeBlock-content").First()
			var h string
			if inner.Length() > 0 {
				h, _ = inner.Html()
			} else {
				h, _ = s.Html()
			}
			s.ReplaceWithHtml(h)
		})
	}
}

func cleanPost(post *goquery.Selection) {
	unwrapSpoilers(post)
	// .messageHide is F95zone's separate "register to view" gate (not a spoiler
	// block) - it never carries field values we want, so it's always dropped.
	post.Find(".messageHide").Remove()
	post.Find(".bbCodeBlock-title, .bbCodeBlock-expandLink, .bbCodeBlock-shrinkLink").Remove()
}

var gatedPlaceholderRe = regexp.MustCompile(`(?i)you (don't|do not) have permission to view the spoiler content\.?\s*(log in or register now\.?)?`)

// stripGated is a safety net for the "you don't have permission..." notice
// slipping through in some other wrapper shape; cleanPost's class-based
// removal is the primary defense.
func stripGated(s string) string {
	return strings.TrimSpace(gatedPlaceholderRe.ReplaceAllString(s, ""))
}

func stripHTML(s string) string {
	s = tagStripRe.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	s = strings.ReplaceAll(s, "​", "")
	return strings.TrimSpace(wsRe.ReplaceAllString(s, " "))
}

var brOrBlockCloseRe = regexp.MustCompile(`(?i)<br\s*/?>|</p>|</div>|</li>`)

// stripHTMLParagraphs is stripHTML's multi-paragraph cousin, for fields like
// Overview where the source is several <p>/<br><br>-separated paragraphs.
// Plain stripHTML replaces every tag (including the paragraph boundaries
// themselves) with a single space, flattening everything into one run-on
// line - fine for a single-line field like Genre, but it's what made the
// Overview box render as a wall of text with no breaks at all. This turns
// <br>/</p>/</div>/</li> into a paragraph break FIRST, strips whatever tags
// are left, then re-collapses only the whitespace within each paragraph
// (never across a break), and joins paragraphs back with a blank line.
func stripHTMLParagraphs(s string) string {
	s = brOrBlockCloseRe.ReplaceAllString(s, "\n\n")
	s = tagStripRe.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = strings.ReplaceAll(s, "​", "")
	var paras []string
	var cur []string
	flush := func() {
		if len(cur) == 0 {
			return
		}
		if p := strings.TrimSpace(wsRe.ReplaceAllString(strings.Join(cur, " "), " ")); p != "" {
			paras = append(paras, p)
		}
		cur = nil
	}
	for _, ln := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(ln); t == "" {
			flush()
		} else {
			cur = append(cur, t)
		}
	}
	flush()
	return strings.Join(paras, "\n\n")
}

func extractDescription(inner, fullText string) (string, bool) {
	desc := ""
	if m := overviewRe.FindStringIndex(inner); m != nil {
		rest := inner[m[1]:]
		if n := nextSecRe.FindStringIndex(rest); n != nil {
			desc = rest[:n[0]]
		} else {
			desc = rest
		}
	}
	if len(strings.TrimSpace(desc)) < 20 {
		desc = ""
		low := strings.ToLower(fullText)
		if ov := strings.Index(low, "overview"); ov != -1 {
			end := len(fullText)
			for _, p := range []string{"installation", "thread updated", "developer", "publisher", "genre", "censorship", "version", "os ", "language", "store"} {
				if i := strings.Index(low[ov+8:], p); i != -1 && ov+8+i < end {
					end = ov + 8 + i
				}
			}
			t := strings.TrimSpace(ovTextRe.ReplaceAllString(fullText[ov:end], ""))
			if len(t) > 20 {
				desc = strings.ReplaceAll(t, "\n", "<br>")
			}
		}
	}
	if len(strings.TrimSpace(desc)) < 20 {
		t := fullText
		if r := []rune(t); len(r) > 2000 {
			t = string(r[:2000])
		}
		desc = strings.ReplaceAll(t, "\n", "<br>")
	}
	desc = stripGated(desc)
	return strings.TrimSpace(desc), strings.TrimSpace(desc) != ""
}

// extractFields reads the "<b>Label</b>: value" metadata block (Thread Updated,
// Version, Genre, ...) out of the cleaned post HTML.
func extractFields(inner string) map[string]string {
	out := map[string]string{}
	matches := fieldBoundaryRe.FindAllStringSubmatchIndex(inner, -1)
	for i, m := range matches {
		label := strings.ToLower(wsRe.ReplaceAllString(inner[m[2]:m[3]], " "))
		valStart := m[1]
		valEnd := len(inner)
		if i+1 < len(matches) {
			valEnd = matches[i+1][0]
		}
		out[label] = stripGated(stripHTML(inner[valStart:valEnd]))
	}
	return out
}

// splitTags turns "2D Game, 2D CG, Corruption" into a trimmed, deduplicated slice.
func splitTags(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	seen := map[string]bool{}
	var out []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" || seen[strings.ToLower(p)] {
			continue
		}
		seen[strings.ToLower(p)] = true
		out = append(out, p)
	}
	return out
}

func absURL(src string) string {
	switch {
	case strings.HasPrefix(src, "//"):
		return "https:" + src
	case strings.HasPrefix(src, "/"):
		return f95BaseURL + src
	}
	return src
}

func extractImages(post *goquery.Selection, max int) (urls []string, header string) {
	seen := map[string]bool{}
	add := func(u string) {
		if !seen[u] {
			seen[u] = true
			urls = append(urls, u)
		}
	}
	post.Find("img").Each(func(_ int, s *goquery.Selection) {
		src, _ := s.Attr("src")
		if src == "" || strings.HasPrefix(src, "data:") {
			for _, at := range []string{"data-src", "data-url"} {
				if v, ok := s.Attr(at); ok && v != "" {
					src = v
					break
				}
			}
		}
		if src == "" {
			return
		}
		low := strings.ToLower(src)
		for _, p := range skipImg {
			if strings.Contains(low, p) {
				return
			}
		}
		src = absURL(hqURL(src))
		if strings.HasPrefix(src, "http") {
			add(src)
			if header == "" {
				header = src
			}
		}
	})
	post.Find("video source, video").Each(func(_ int, s *goquery.Selection) {
		if src, _ := s.Attr("src"); src != "" {
			add(absURL(src))
		}
	})
	if max >= 0 && len(urls) > max {
		urls = urls[:max]
	}
	return
}

func (a *App) enrichRelease(ctx context.Context, f Fetcher, r *Release, cfg Config) error {
	a.log.Info("Enriching: %s...", trunc(r.Title, 50))
	rawHTML, err := f.Page(ctx, r.Link)
	if err != nil {
		return fmt.Errorf("could not load thread page: %w", err)
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(rawHTML))
	if err != nil {
		return fmt.Errorf("could not parse thread page: %w", err)
	}
	post := findPost(doc)
	if post == nil {
		return errors.New("first post container not found")
	}
	cleanPost(post)
	inner, _ := post.Html()
	fullText := post.Text()

	desc, ok := extractDescription(inner, fullText)
	if !ok {
		return errors.New("could not extract description")
	}
	r.ExtraDescription = desc
	r.Overview = extractOverview(inner)
	r.Changelog = extractChangelog(inner)
	r.Downloads = extractDownloadLinks(inner)
	a.log.Info("  Description extracted (%d chars)", len([]rune(desc)))

	// Engine/label from the thread's own prefix badges (authoritative) fall back
	// to the title-bracket guess (already set on r.Engine/r.Labels) only when the
	// page has no prefix that matches a known engine.
	if pfxEngine, pfxLabel := parseThreadPrefixes(doc); pfxEngine != "" || pfxLabel != "" {
		if pfxEngine != "" {
			r.Engine = pfxEngine
		}
		r.Labels = mergeLabel(r.Labels, pfxLabel)
	}

	fields := extractFields(inner)
	r.ThreadUpdated = fields["thread updated"]
	// Don't clobber ThreadUpdatedISO if the discovery step already set it from
	// F95zone's own listing "ts" field (see fetchListingDataPage) - that's the
	// exact value the live site sorts by, refreshed on every listing scan with
	// no enrichment needed, whereas this OP-text field is only ever as fresh
	// as the release's last enrichment and only has day granularity. Parsing
	// it here is purely a fallback for whenever ts isn't available.
	if r.ThreadUpdatedISO == "" {
		r.ThreadUpdatedISO = parseFieldDate(r.ThreadUpdated)
	}
	r.ReleaseDate = fields["release date"]
	// A release discovered straight off the listing pages (fetchListingItems /
	// backfill) has no PubDate - that used to come from the F95zone source RSS
	// item, which regular runs no longer fetch. Fall back to the thread's own
	// "Thread updated" (or "Release date") field so Published/sorting/the
	// generated feed still have something to show; an already-known release
	// keeps its original PubDate (pipeline.go carries it forward before this
	// runs), so this only ever fires for a release seen for the first time.
	if r.PubDateISO == "" {
		for _, raw := range []string{r.ThreadUpdated, r.ReleaseDate} {
			raw = strings.TrimSpace(raw)
			if raw == "" {
				continue
			}
			if t, err := time.Parse("2006-01-02", raw); err == nil {
				r.PubDateRaw = raw
				r.PubDateISO = t.UTC().Format("2006-01-02T15:04:05-07:00")
				break
			}
		}
	}
	if r.Developer = fields["developer"]; r.Developer == "" {
		r.Developer = fields["publisher"]
	}
	if r.Censored = fields["censored"]; r.Censored == "" {
		r.Censored = fields["censorship"]
	}
	r.OS = fields["os"]
	r.Language = fields["language"]
	r.Store = fields["store"]
	if v := strings.TrimSpace(fields["version"]); v != "" {
		r.Version = v // the thread's own Version: field is authoritative over the title-bracket guess
	}
	// Tags: merge the thread header's own tag list (reliable, always rendered)
	// with whatever the Genre spoiler block in the first post yields (may be
	// gated or absent on some threads).
	r.Tags = mergeTags(parseHeaderTags(doc), splitTags(fields["genre"]))
	if len(r.Tags) > 0 {
		a.log.Info("  Tags: %s", strings.Join(r.Tags, ", "))
	}

	urls, header := extractImages(post, cfg.MaxImages)
	r.ImageURLs = urls
	r.HeaderImage = header
	if urls == nil {
		r.ImageURLs = []string{}
	}
	if len(urls) > 0 {
		a.log.Info("  Images: %d found", len(urls))
	}
	r.EnrichedAt = time.Now().UTC().Format(time.RFC3339)
	r.EnrichError = ""
	r.ParserVer = parserVersion
	return nil
}

func trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// ──────────────────────────────────────────────────────────────
//  Image download
// ──────────────────────────────────────────────────────────────

// downloadImage returns (filename, downloadedNew, error).
func (a *App) downloadImage(ctx context.Context, cfg Config, u, thread string) (string, bool, error) {
	fname := imgFilename(u)
	fpath := filepath.Join(a.store.imagesDir, fname)
	if _, err := os.Stat(fpath); err == nil {
		a.store.AddManifest(thread, imgHash(u), fname)
		return fname, false, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	req.Header.Set("User-Agent", cfg.UserAgent)
	if cfg.SiteCookie != "" {
		req.Header.Set("Cookie", cfg.SiteCookie)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", false, fmt.Errorf("HTTP %d: %s", resp.StatusCode, trunc(u, 60))
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return "", false, err
	}
	if err := atomicWrite(fpath, b); err != nil {
		return "", false, err
	}
	a.store.AddManifest(thread, imgHash(u), fname)
	return fname, true, nil
}

// ──────────────────────────────────────────────────────────────
//  Backfill: harvest thread links from the Angular "latest alpha" listing
// ──────────────────────────────────────────────────────────────

// listDataURLTemplate is F95zone's own paginated JSON API behind the
// "Latest Updates" Angular app - the exact endpoint latest.min.js itself
// calls to render the page, discovered by inspecting what the Angular shell
// actually fetches. Unlike the Angular route "/sam/latest_alpha/#/..." this
// is a plain JSON GET, needs no headless browser, and - confirmed live -
// needs no login either, unlike the Angular shell page (which gates on
// login/permissions for reasons unrelated to the underlying data). "%d" is
// the page number; 30 items per page.
const listDataURLTemplate = f95BaseURL + "/sam/latest_alpha/latest_data.php?cmd=list&cat=games&page=%d"

type listDataItem struct {
	ThreadID int    `json:"thread_id"`
	Title    string `json:"title"`
	// Ts is F95zone's own "last bumped" unix timestamp for this thread - it's
	// exactly what the live latest_alpha page itself sorts by, refreshed on
	// every listing fetch with no need to load the thread page at all. Using
	// this instead of relying on parsing the OP's own "Thread updated" text
	// (only available once a release is actually enriched, and only as fresh
	// as its last enrichment) is what lets our default sort actually track
	// F95zone's ordering in near-real-time rather than lagging behind by
	// however long since the release was last re-scraped.
	Ts int64 `json:"ts"`
}

type listDataResp struct {
	Status string `json:"status"`
	Msg    struct {
		Data       []listDataItem `json:"data"`
		Pagination struct {
			Page  int `json:"page"`
			Total int `json:"total"` // total PAGES, not items
		} `json:"pagination"`
	} `json:"msg"`
}

// fetchListingDataPage calls listDataURLTemplate for one page and returns
// the releases found on it plus the total page count the API itself
// reports, so callers don't need to guess when to stop.
func (a *App) fetchListingDataPage(ctx context.Context, cfg Config, page int) ([]Release, int, error) {
	u := fmt.Sprintf(listDataURLTemplate, page)
	var b []byte
	if cfg.ByparrURL != "" {
		bf := newByparrFetcher(cfg)
		raw, status, err := bf.solve(ctx, u, cfg.SiteCookie)
		if err != nil {
			return nil, 0, err
		}
		if status != 0 && status >= 400 {
			return nil, 0, fmt.Errorf("byparr: HTTP %d fetching listing page", status)
		}
		b = []byte(extractByparrJSON(raw))
	} else {
		req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
		if err != nil {
			return nil, 0, err
		}
		req.Header.Set("User-Agent", cfg.UserAgent)
		req.Header.Set("Accept", "application/json")
		if cfg.SiteCookie != "" {
			req.Header.Set("Cookie", cfg.SiteCookie)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, 0, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, 0, fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		b, err = io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		if err != nil {
			return nil, 0, err
		}
	}
	var d listDataResp
	if err := json.Unmarshal(b, &d); err != nil {
		return nil, 0, fmt.Errorf("could not parse listing JSON: %w", err)
	}
	if d.Status != "ok" {
		return nil, 0, fmt.Errorf("listing API returned status %q", d.Status)
	}
	out := make([]Release, 0, len(d.Msg.Data))
	for _, it := range d.Msg.Data {
		if it.ThreadID == 0 || it.Title == "" {
			continue
		}
		r := Release{Link: fmt.Sprintf("%s/threads/%d/", f95BaseURL, it.ThreadID), Title: it.Title, ImageURLs: []string{}}
		r.Labels, r.Engine, r.Version = parseTitleBrackets(r.Title)
		if it.Ts > 0 {
			r.ThreadUpdatedISO = time.Unix(it.Ts, 0).UTC().Format("2006-01-02T15:04:05-07:00")
		}
		out = append(out, r)
	}
	return out, d.Msg.Pagination.Total, nil
}

// ──────────────────────────────────────────────────────────────
//  Login check: does the configured site_cookie actually get us a
//  logged-in session, or are we still scraping gated placeholder text?
// ──────────────────────────────────────────────────────────────

// LoginCheck is the result of probing F95zone with the configured cookie.
type LoginCheck struct {
	CookieSet    bool   `json:"cookie_set"`
	Checked      bool   `json:"checked"`
	CheckedLink  string `json:"checked_link,omitempty"`
	CheckedTitle string `json:"checked_title,omitempty"`
	LoggedIn     bool   `json:"logged_in"`
	SpoilersSeen int    `json:"spoilers_seen"`
	Gated        bool   `json:"gated"`
	Detail       string `json:"detail"`
	CheckedAt    string `json:"checked_at"`
}

// CheckF95Login re-fetches a real thread - the most recently enriched
// release, or the first item of the source RSS feed if the database is still
// empty - with the configured site_cookie and checks whether its
// spoiler-gated fields came back as real content or as the fixed "you don't
// have permission to view the spoiler content" placeholder. That's the exact
// signal enrichRelease itself depends on for full release info (Genre,
// Developer, etc. are usually inside a spoiler block), so a clean pass here
// means the cookie is a working, logged-in session.
func (a *App) CheckF95Login(ctx context.Context) (LoginCheck, error) {
	cfg := a.cfg.Get()
	res := LoginCheck{CookieSet: strings.TrimSpace(cfg.SiteCookie) != "", CheckedAt: time.Now().UTC().Format(time.RFC3339)}
	if !res.CookieSet {
		res.Detail = "No site_cookie configured in Settings - enrichment runs as a logged-out guest, so gated fields (Genre, and some threads' whole description) will be blank."
		return res, nil
	}

	link, title, err := a.pickProbeThread(ctx, cfg)
	if err != nil {
		res.Detail = fmt.Sprintf("Could not find a thread to test the cookie against: %v", err)
		return res, err
	}
	res.CheckedLink, res.CheckedTitle = link, title

	var f Fetcher
	if cfg.ByparrURL != "" {
		f = newByparrFetcher(cfg)
	} else {
		f = newHTTPFetcher(cfg)
	}
	defer f.Close()
	rawHTML, err := f.Page(ctx, link)
	if err != nil {
		res.Detail = fmt.Sprintf("Thread fetch failed: %v", err)
		return res, err
	}
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(rawHTML))
	if err != nil {
		res.Detail = fmt.Sprintf("Could not parse the thread page: %v", err)
		return res, err
	}
	// Primary signal: F95zone (XenForo) marks a logged-in page with
	// data-logged-in="true" on <html id="XF" ...>, and shows the real account
	// name in the visitor nav instead of "Sign up/Log in" links. This is the
	// site's own authoritative marker, so it's checked first and is
	// conclusive either way - unlike the old spoiler-gating heuristic below,
	// which only works on threads that actually have a gated field.
	loggedInAttr := strings.EqualFold(strings.TrimSpace(doc.Find("html").AttrOr("data-logged-in", "")), "true")
	username := strings.TrimSpace(doc.Find(".p-navgroup-link--user .p-navgroup-linkText").First().Text())
	if loggedInAttr || username != "" {
		res.Checked = true
		res.LoggedIn = true
		if username != "" {
			res.Detail = fmt.Sprintf("Logged in as %q (data-logged-in=\"true\" on the page) - the site cookie is working.", username)
		} else {
			res.Detail = `Page reports data-logged-in="true" - the site cookie is working.`
		}
		return res, nil
	}

	post := findPost(doc)
	if post == nil {
		res.Detail = "Could not find the thread's first post on the page - its layout may not match what this scraper expects."
		return res, errors.New("first post container not found")
	}
	res.SpoilersSeen = post.Find(".bbCodeSpoiler, .bbCodeBlock--spoiler").Length()
	cleanPost(post)
	res.Gated = gatedPlaceholderRe.MatchString(post.Text())
	res.Checked = true

	switch {
	case res.SpoilersSeen == 0:
		res.Detail = fmt.Sprintf("%q shows no logged-in marker and has no spoiler-gated fields to test against either, so this isn't conclusive - try again once more threads are enriched.", title)
	case res.Gated:
		res.Detail = fmt.Sprintf("No logged-in marker on the page, and spoiler content on %q is still gated (\"log in or register now\") - the cookie is missing, expired, or belongs to a logged-out session.", title)
	default:
		res.LoggedIn = true
		res.Detail = fmt.Sprintf("Full content, including %d spoiler-gated field(s), came back from %q - the site cookie is working.", res.SpoilersSeen, title)
	}
	return res, nil
}

// pickProbeThread picks a real thread URL to test the cookie against: the
// most recently enriched release already in the database, or - on a brand
// new install with nothing scraped yet - the first item of the source RSS
// feed (one lightweight extra request, no enrichment).
func (a *App) pickProbeThread(ctx context.Context, cfg Config) (link, title string, err error) {
	if l, t, ok := a.store.db.MostRecentEnrichedLink(); ok {
		return l, t, nil
	}
	items, err := a.fetchListingItems(ctx, cfg, 1)
	if err != nil {
		return "", "", err
	}
	if len(items) == 0 {
		return "", "", errors.New("listing page returned no items")
	}
	return items[0].Link, items[0].Title, nil
}
