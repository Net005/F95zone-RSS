package main

import (
	"bytes"
	"fmt"
	"html"
	"strings"
	"time"
)

var mimeMap = map[string]string{
	"jpg": "image/jpeg", "jpeg": "image/jpeg", "png": "image/png", "gif": "image/gif", "webp": "image/webp",
}

func xmlEsc(s string) string {
	var b bytes.Buffer
	xmlEscape(&b, s)
	return b.String()
}

func xmlEscape(b *bytes.Buffer, s string) {
	for _, r := range s {
		switch r {
		case '&':
			b.WriteString("&amp;")
		case '<':
			b.WriteString("&lt;")
		case '>':
			b.WriteString("&gt;")
		case '"':
			b.WriteString("&quot;")
		case 0x09, 0x0A, 0x0D:
			b.WriteRune(r)
		default:
			if r >= 0x20 && r != 0xFFFE && r != 0xFFFF && !(r >= 0xD800 && r <= 0xDFFF) {
				b.WriteRune(r)
			}
		}
	}
}

func (a *App) imgSrc(cfg Config, remote string) string {
	name := imgFilename(remote)
	if a.store.ImageExists(name) {
		return cfg.PublicBaseURL + "/f95zone/images/" + name
	}
	return remote
}

// BuildDescription renders the HTML body used for <description> and <content:encoded>.
func (a *App) BuildDescription(rel Release, cfg Config) string {
	var parts []string

	header := rel.HeaderImage
	if header == "" && len(rel.ImageURLs) > 0 {
		header = rel.ImageURLs[0]
	}
	if header != "" {
		header = hqURL(header)
		parts = append(parts,
			`<div style="width:100%;text-align:center;margin-bottom:16px;">`+
				`<img src="`+a.imgSrc(cfg, header)+`" alt="Cover Image" `+
				`style="max-width:100%;max-height:400px;width:auto;height:auto;border-radius:8px;box-shadow:0 2px 8px rgba(0,0,0,0.3);display:block;margin:0 auto;" />`+
				`</div>`)
	}

	desc := rel.ExtraDescription
	if desc == "" {
		desc = rel.SourceDescription
	}
	if desc != "" {
		parts = append(parts,
			`<div style="margin:16px 0;padding:16px;background:#1a1c20;border-radius:8px;line-height:1.6;border-left:4px solid #6d4aff;">`+
				desc+`</div>`)
	}

	if len(rel.ImageURLs) > 1 {
		var g strings.Builder
		g.WriteString(`<div style="margin:16px 0;"><div style="display:grid;grid-template-columns:repeat(auto-fill,minmax(200px,1fr));gap:8px;">`)
		for _, u := range rel.ImageURLs[1:] {
			u = hqURL(u)
			label := ""
			lu := strings.ToLower(u)
			if strings.HasSuffix(lu, ".gif") || strings.HasSuffix(lu, ".webp") {
				label = `<small style="position:absolute;top:2px;right:2px;background:rgba(0,0,0,0.7);color:#fff;padding:1px 4px;font-size:10px;border-radius:2px;">Animated</small>`
			}
			g.WriteString(`<div style="position:relative;overflow:hidden;border-radius:4px;background:#1a1c20;">` +
				`<img src="` + a.imgSrc(cfg, u) + `" alt="Screenshot" style="width:100%;height:120px;object-fit:cover;display:block;border-radius:4px;" />` +
				label + `</div>`)
		}
		g.WriteString(`</div></div>`)
		parts = append(parts, g.String())
	}

	var meta []string
	if rel.Engine != "" {
		meta = append(meta, "Engine: "+html.EscapeString(rel.Engine))
	}
	if rel.Version != "" {
		meta = append(meta, "Version: "+html.EscapeString(rel.Version))
	}
	if len(meta) > 0 {
		parts = append(parts, `<div style="margin:8px 0;color:#9a9a9a;font-size:12px;">`+strings.Join(meta, " &nbsp;&middot;&nbsp; ")+`</div>`)
	}

	badge := func(c, bg string) string {
		return `<span style="display:inline-block;background:` + bg + `;color:white;padding:3px 8px;border-radius:4px;font-size:11px;margin:4px 4px 0 0;">` + html.EscapeString(c) + `</span>`
	}
	if len(rel.Labels) > 0 {
		var b []string
		for _, c := range rel.Labels {
			b = append(b, badge(c, "#6d4aff"))
		}
		parts = append(parts, `<div style="margin:16px 0 4px 0;"><strong>Labels:</strong> `+strings.Join(b, " ")+`</div>`)
	}
	if len(rel.Tags) > 0 {
		var b []string
		for _, c := range rel.Tags {
			b = append(b, badge(c, "#3a3d46"))
		}
		parts = append(parts, `<div style="margin:8px 0;"><strong>Tags:</strong> `+strings.Join(b, " ")+`</div>`)
	}
	return strings.Join(parts, "\n")
}

const rfc822 = "Mon, 02 Jan 2006 15:04:05 -0700"

// GenerateFeed writes feed.xml from the given releases.
func (a *App) GenerateFeed(releases []Release) error {
	cfg := a.cfg.Get()
	a.log.Info("Building RSS feed...")
	var b bytes.Buffer
	w := b.WriteString
	w(`<?xml version="1.0" ?>` + "\n")
	w(`<rss version="2.0" xmlns:atom="http://www.w3.org/2005/Atom" xmlns:content="http://purl.org/rss/1.0/modules/content/">` + "\n")
	w("  <channel>\n")
	w("    <title>F95Zone Latest Games - Enriched</title>\n")
	w("    <link>" + f95BaseURL + "</link>\n")
	w("    <description>F95Zone latest game releases with descriptions and images. Cover image first, then overview text, then screenshot gallery.</description>\n")
	w("    <language>en-us</language>\n")
	w("    <lastBuildDate>" + time.Now().UTC().Format(rfc822) + "</lastBuildDate>\n")
	w("    <generator>F95Zone RSS Enricher (Go) v" + appVersion + "</generator>\n")
	w(fmt.Sprintf(`    <atom:link href="%s/feed.xml" rel="self" type="application/rss+xml"/>`+"\n", xmlEsc(cfg.PublicBaseURL)))

	for _, rel := range releases {
		w("    <item>\n")
		w("      <title>" + xmlEsc(rel.Title) + "</title>\n")
		w("      <link>" + xmlEsc(rel.Link) + "</link>\n")
		pub := rel.PubDateRaw
		if t, err := time.Parse(time.RFC3339, rel.PubDateISO); err == nil {
			pub = t.Format(rfc822)
		} else if pub == "" {
			pub = time.Now().UTC().Format(rfc822)
		}
		w("      <pubDate>" + xmlEsc(pub) + "</pubDate>\n")
		w(`      <guid isPermaLink="true">` + xmlEsc(rel.Link) + "</guid>\n")
		for _, c := range rel.Labels {
			w("      <category>" + xmlEsc(c) + "</category>\n")
		}
		for _, c := range rel.Tags {
			w("      <category>" + xmlEsc(c) + "</category>\n")
		}
		if rel.Engine != "" {
			w("      <category>" + xmlEsc(rel.Engine) + "</category>\n")
		}
		body := xmlEsc(a.BuildDescription(rel, cfg))
		w("      <description>" + body + "</description>\n")
		w("      <content:encoded>" + body + "</content:encoded>\n")

		header := rel.HeaderImage
		if header == "" && len(rel.ImageURLs) > 0 {
			header = rel.ImageURLs[0]
		}
		if header != "" {
			header = hqURL(header)
			name := imgFilename(header)
			encURL, mime, size := header, "image/jpeg", int64(0)
			if a.store.ImageExists(name) {
				encURL = cfg.PublicBaseURL + "/f95zone/images/" + name
				ext := strings.ToLower(strings.TrimPrefix(name[strings.LastIndex(name, "."):], "."))
				if m, ok := mimeMap[ext]; ok {
					mime = m
				}
				size = a.store.imageSize(name)
			}
			w(fmt.Sprintf(`      <enclosure url="%s" type="%s" length="%d"/>`+"\n", xmlEsc(encURL), mime, size))
		}
		w("    </item>\n")
	}
	w("  </channel>\n</rss>\n")
	if err := atomicWrite(a.store.rssFile, b.Bytes()); err != nil {
		return err
	}
	a.log.Info("RSS saved: %s (%d items)", a.store.rssFile, len(releases))
	return nil
}
