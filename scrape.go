package main

import (
	"context"
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

func parseRSSDate(s string) string {
	t, err := mail.ParseDate(s)
	if err != nil {
		t = time.Now().UTC()
	}
	return t.Format("2006-01-02T15:04:05-07:00")
}

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

var engineNames = map[string]string{
	"rpgm": "RPGM", "rpg maker": "RPGM", "rpg maker mv": "RPGM", "rpg maker vx": "RPGM",
	"unity": "Unity", "ren'py": "Ren'Py", "renpy": "Ren'Py", "qsp": "QSP", "html": "HTML",
	"rags": "RAGS", "java": "Java", "flash": "Flash", "adrift": "ADRIFT", "wolf rpg": "Wolf RPG",
	"unreal engine": "Unreal Engine", "unreal": "Unreal Engine", "webgl": "WebGL", "godot": "Godot",
	"tads": "TADS", "gamemaker": "GameMaker studio", "ocean": "Ocean",
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
	nextSecRe  = regexp.MustCompile(`(?i)<b[^>]*>\s*(?:Installation|Thread\s*Updated|Developer|Publisher|Censorship|Censored|Version|OS|Language|Store|Genre|Length|Notes?|Changelog|Downloads?|Links?)\s*[:</]`)
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
	a.log.Info("  Description extracted (%d chars)", len([]rune(desc)))

	fields := extractFields(inner)
	r.ThreadUpdated = fields["thread updated"]
	r.ReleaseDate = fields["release date"]
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
	r.Tags = splitTags(fields["genre"])
	if len(r.Tags) > 0 {
		a.log.Info("  Genre: %s", strings.Join(r.Tags, ", "))
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

var threadHrefRe = regexp.MustCompile(`/threads/[^/?#]+\.\d+/?$`)

const harvestJS = `(() => {
  const out = []; const seen = new Set();
  document.querySelectorAll('a[href*="/threads/"]').forEach(a => {
    let href = a.getAttribute('href') || '';
    if (!href) return;
    if (href.startsWith('//')) href = location.protocol + href;
    else if (href.startsWith('/')) href = location.origin + href;
    href = href.split('#')[0].split('?')[0];
    const title = (a.textContent || '').trim();
    if (!title || seen.has(href)) return;
    seen.add(href);
    out.push({link: href, title: title});
  });
  return out;
})()`

type harvestedLink struct {
	Link  string `json:"link"`
	Title string `json:"title"`
}

// harvestListingPage loads one page of the client-rendered "latest alpha"
// listing and pulls out thread links + titles. It's best-effort: F95zone
// doesn't document this endpoint, and full results likely need a logged-in
// session cookie (set in Settings).
func (a *App) harvestListingPage(ctx context.Context, bf *browserFetcher, pageURL string) ([]Release, error) {
	tctx, cancel := context.WithTimeout(bf.ctx, 60*time.Second)
	defer cancel()
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	var found []harvestedLink
	err := chromedp.Run(tctx,
		chromedp.Navigate(pageURL),
		chromedp.Sleep(4*time.Second),
		chromedp.Evaluate(harvestJS, &found),
	)
	if err != nil {
		return nil, err
	}
	var out []Release
	for _, h := range found {
		if !threadHrefRe.MatchString(h.Link) {
			continue
		}
		r := Release{Link: h.Link, Title: h.Title, ImageURLs: []string{}}
		r.Labels, r.Engine, r.Version = parseTitleBrackets(r.Title)
		out = append(out, r)
	}
	return out, nil
}
