package main

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
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
	"github.com/chromedp/chromedp"
)

// ──────────────────────────────────────────────────────────────
//  Source RSS
// ──────────────────────────────────────────────────────────────

var bracketRe = regexp.MustCompile(`\[(.*?)\]`)

var prefixToTag = map[string]string{
	"RPGM": "engine-rpgmaker", "VN": "genre-visualnovel", "Unity": "engine-unity",
	"Ren'Py": "engine-renpy", "QSP": "engine-qsp", "HTML": "engine-html",
	"RAGS": "engine-rags", "Java": "engine-java", "Flash": "engine-flash",
	"ADRIFT": "engine-adrift", "Wolf RPG": "engine-wolfrpg", "Unreal Engine": "engine-unreal",
	"WebGL": "engine-webgl", "Godot": "engine-godot", "Completed": "status-completed",
	"Onhold": "status-onhold", "Abandoned": "status-abandoned", "SiteRip": "source-siterip",
	"Collection": "type-collection",
}

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
			Genres:            []string{},
			ImageURLs:         []string{},
		}
		r.PubDateISO = parseRSSDate(r.PubDateRaw)
		cats := []string{}
		has := func(v string) bool {
			for _, c := range cats {
				if c == v {
					return true
				}
			}
			return false
		}
		for _, c := range it.Categories {
			if c = strings.TrimSpace(c); c != "" {
				cats = append(cats, c)
			}
		}
		for _, m := range bracketRe.FindAllStringSubmatch(r.Title, -1) {
			bt := strings.TrimSpace(m[1])
			tag := bt
			if t, ok := prefixToTag[bt]; ok {
				tag = t
			}
			if !has(tag) {
				cats = append(cats, tag)
			}
			if !has(bt) {
				cats = append(cats, bt)
			}
		}
		r.Categories = cats
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
//  Page fetchers (plain HTTP or headless Chromium)
// ──────────────────────────────────────────────────────────────

type Fetcher interface {
	Page(ctx context.Context, url string) (string, error)
	Close()
}

type httpFetcher struct {
	client *http.Client
	ua     string
}

func newHTTPFetcher(cfg Config) *httpFetcher {
	jar, _ := cookiejar.New(nil)
	return &httpFetcher{client: &http.Client{Jar: jar, Timeout: 60 * time.Second}, ua: cfg.UserAgent}
}

func (f *httpFetcher) Page(ctx context.Context, u string) (string, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	req.Header.Set("User-Agent", f.ua)
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
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
//  Thread page enrichment (port of ENRICH_JS + image scan)
// ──────────────────────────────────────────────────────────────

var (
	overviewRe = regexp.MustCompile(`(?i)<b[^>]*>\s*Overview\s*</b>\s*:?\s*`)
	nextSecRe  = regexp.MustCompile(`(?i)<b[^>]*>\s*(?:Installation|Thread\s*Updated|Developer|Publisher|Censorship|Version|OS|Language|Store|Genre|Notes?|Changelog|Downloads?|Links?)\s*[:</]`)
	ovTextRe   = regexp.MustCompile(`(?i)^\s*Overview\s*:?\s*`)
	skipImg    = []string{"avatar", "smilie", "emoji", "icon", "logo", "spinner", "loading", "styles", "thumbs", "ui-"}
)

const postSelectors = ".bbWrapper, .message-cell--main, .message-main, .threadmark-content"

func findPost(doc *goquery.Document) *goquery.Selection {
	for _, sel := range []string{".bbWrapper", ".message-cell--main", ".message-main", ".threadmark-content"} {
		if s := doc.Find(sel).First(); s.Length() > 0 {
			return s
		}
	}
	return nil
}

func extractDescription(html string) (string, bool) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		return "", false
	}
	post := findPost(doc)
	if post == nil {
		return "", false
	}
	// guests cannot see spoiler content, hidden blocks, or block titles
	post.Find(".bbCodeSpoiler").Remove()
	post.Find(".messageHide").Remove()
	post.Find(".bbCodeBlock-title, .bbCodeBlock-expandLink, .bbCodeBlock-shrinkLink").Remove()
	inner, _ := post.Html()
	fullText := post.Text()

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
	desc = strings.TrimSpace(desc)
	return desc, desc != ""
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

func extractImages(html string, max int) (urls []string, header string) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		return
	}
	post := findPost(doc)
	if post == nil {
		return
	}
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
	html, err := f.Page(ctx, r.Link)
	if err != nil {
		return fmt.Errorf("could not load thread page: %w", err)
	}
	if desc, ok := extractDescription(html); ok {
		r.ExtraDescription = desc
		a.log.Info("  Description extracted (%d chars)", len([]rune(desc)))
	} else {
		return errors.New("first post container not found")
	}
	urls, header := extractImages(html, cfg.MaxImages)
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
