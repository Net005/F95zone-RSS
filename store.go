package main

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type RunRecord struct {
	ID        int       `json:"id"`
	Trigger   string    `json:"trigger"` // startup, schedule, manual, cache, backfill
	Status    string    `json:"status"`  // ok, error, cancelled, cache
	Started   time.Time `json:"started"`
	Finished  time.Time `json:"finished"`
	Duration  float64   `json:"duration_s"`
	Items     int       `json:"items"`
	Enriched  int       `json:"enriched"`
	Reused    int       `json:"reused"`
	Failed    int       `json:"failed"`
	ImagesNew int       `json:"images_new"`
	New       int       `json:"new_releases"`
	Updated   int       `json:"updated_releases"`
	Error     string    `json:"error,omitempty"`
}

// Store holds everything that isn't in the SQLite database: the image cache on
// disk, its manifest, and run history. Release data itself lives in DB.
type Store struct {
	db *DB

	dir         string
	imagesDir   string
	rssFile     string
	manifest    string
	historyFile string

	mmu     sync.Mutex
	man     map[string]map[string]string
	hmu     sync.Mutex
	history []RunRecord
}

func NewStore(db *DB, dataDir string) *Store {
	s := &Store{
		db:          db,
		dir:         dataDir,
		imagesDir:   filepath.Join(dataDir, "images"),
		rssFile:     filepath.Join(dataDir, "feed.xml"),
		manifest:    filepath.Join(dataDir, "images_manifest.json"),
		historyFile: filepath.Join(dataDir, "runs.json"),
	}
	os.MkdirAll(s.imagesDir, 0o755)
	s.loadManifest()
	if b, err := os.ReadFile(s.historyFile); err == nil {
		json.Unmarshal(b, &s.history)
	}
	return s
}

// ImportLegacyCache imports a Python-era releases_cache.json (or an earlier Go
// version's) into the database once, on startup, and renames it aside.
func (s *Store) ImportLegacyCache(path string, log *LogHub) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var legacy []map[string]any
	if json.Unmarshal(b, &legacy) != nil || len(legacy) == 0 {
		return
	}
	n := 0
	for _, m := range legacy {
		r := Release{
			Link:              str(m["link"]),
			Title:             str(m["title"]),
			PubDateRaw:        str(m["pub_date_raw"]),
			PubDateISO:        str(m["pub_date_iso"]),
			SourceDescription: str(m["source_description"]),
			ExtraDescription:  str(m["extra_description"]),
			ImageURLs:         strArr(m["image_urls"]),
			HeaderImage:       str(m["header_image"]),
			EnrichedAt:        str(m["enriched_at"]),
			EnrichError:       str(m["enrich_error"]),
			ParserVer:         0, // force one re-scrape under the new parser
		}
		if r.Link == "" {
			continue
		}
		// The old "categories" field mixed status labels, engine names and the
		// raw version bracket together (that's the bug this release fixes), so
		// it's discarded rather than merged in. Labels/engine/version are
		// re-derived from the title, and Tags (Genre) arrive on the next
		// enrichment pass since parser_ver=0 forces a re-scrape.
		r.Labels, r.Engine, r.Version = parseTitleBrackets(r.Title)
		if err := s.db.UpsertRelease(r, "legacy-import"); err == nil {
			n++
		}
	}
	dst := path + ".imported"
	if _, err := os.Stat(dst); err == nil {
		dst = path + ".imported." + time.Now().UTC().Format("20060102150405")
	}
	if err := os.Rename(path, dst); err != nil {
		log.Warn("Legacy release cache imported (%d items) but %s could not be renamed: %v", n, path, err)
	} else {
		log.Info("Imported %d releases from legacy cache %s (renamed to %s). They'll be re-scraped once to pick up correct tags/engine/version.", n, path, filepath.Base(dst))
	}
}

func str(v any) string {
	s, _ := v.(string)
	return s
}
func strArr(v any) []string {
	a, _ := v.([]any)
	out := make([]string, 0, len(a))
	for _, x := range a {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// ── image helpers (filenames match the Python implementation) ──

func imgHash(u string) string {
	sum := md5.Sum([]byte(u))
	return hex.EncodeToString(sum[:])[:10]
}

func imgFilename(u string) string {
	p := strings.SplitN(u, "?", 2)[0]
	if pu, err := url.Parse(p); err == nil && pu.Path != "" {
		p = pu.Path
	}
	ext := path.Ext(p)
	if ext == "" {
		ext = ".jpg"
	}
	return imgHash(u) + ext
}

func hqURL(u string) string {
	if strings.Contains(u, "/thumb/") {
		return strings.ReplaceAll(u, "/thumb/", "/")
	}
	return u
}

func (s *Store) ImageExists(fname string) bool {
	_, err := os.Stat(filepath.Join(s.imagesDir, fname))
	return err == nil
}

func (s *Store) imageSize(name string) int64 {
	if st, err := os.Stat(filepath.Join(s.imagesDir, name)); err == nil {
		return st.Size()
	}
	return 0
}

type ImageStats struct {
	Count    int   `json:"count"`
	Bytes    int64 `json:"bytes"`
	Orphans  int   `json:"orphans"`
	OrphanSz int64 `json:"orphan_bytes"`
}

// referenced returns every image filename any release (of any age) points at.
func (s *Store) referenced() map[string]bool {
	ref := map[string]bool{}
	page, err := s.db.QueryReleases(ReleaseFilter{PageSize: 1_000_000, Page: 1})
	if err != nil {
		return ref
	}
	for _, r := range page.Items {
		if r.HeaderImage != "" {
			ref[imgFilename(hqURL(r.HeaderImage))] = true
		}
		for _, u := range r.ImageURLs {
			ref[imgFilename(hqURL(u))] = true
		}
	}
	return ref
}

func (s *Store) ImageStats() ImageStats {
	ref := s.referenced()
	var st ImageStats
	ents, _ := os.ReadDir(s.imagesDir)
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		st.Count++
		st.Bytes += info.Size()
		if !ref[e.Name()] {
			st.Orphans++
			st.OrphanSz += info.Size()
		}
	}
	return st
}

func (s *Store) PurgeOrphans() (int, int64) {
	ref := s.referenced()
	ents, _ := os.ReadDir(s.imagesDir)
	n := 0
	var sz int64
	for _, e := range ents {
		if e.IsDir() || ref[e.Name()] {
			continue
		}
		if info, err := e.Info(); err == nil {
			if os.Remove(filepath.Join(s.imagesDir, e.Name())) == nil {
				n++
				sz += info.Size()
			}
		}
	}
	s.mmu.Lock()
	links := s.db.AllLinks()
	for k := range s.man {
		if !links[k] {
			delete(s.man, k)
		}
	}
	s.mmu.Unlock()
	s.SaveManifest()
	return n, sz
}

func (s *Store) loadManifest() {
	s.man = map[string]map[string]string{}
	if b, err := os.ReadFile(s.manifest); err == nil {
		json.Unmarshal(b, &s.man)
	}
}

func (s *Store) AddManifest(thread, key, fname string) {
	s.mmu.Lock()
	defer s.mmu.Unlock()
	if s.man[thread] == nil {
		s.man[thread] = map[string]string{}
	}
	s.man[thread][key] = fname
}

func (s *Store) SaveManifest() {
	s.mmu.Lock()
	b, _ := json.MarshalIndent(s.man, "", "  ")
	s.mmu.Unlock()
	atomicWrite(s.manifest, b)
}

// ── run history ──

func (s *Store) AddRun(r RunRecord) {
	s.hmu.Lock()
	defer s.hmu.Unlock()
	next := 1
	for _, h := range s.history {
		if h.ID >= next {
			next = h.ID + 1
		}
	}
	r.ID = next
	s.history = append(s.history, r)
	if len(s.history) > 200 {
		s.history = s.history[len(s.history)-200:]
	}
	b, _ := json.MarshalIndent(s.history, "", "  ")
	atomicWrite(s.historyFile, b)
}

func (s *Store) History() []RunRecord {
	s.hmu.Lock()
	defer s.hmu.Unlock()
	out := make([]RunRecord, len(s.history))
	copy(out, s.history)
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out
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
