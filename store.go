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

// Release keeps the same JSON field names as the Python cache, so an existing
// releases_cache.json is picked up as-is.
type Release struct {
	Title             string   `json:"title"`
	Link              string   `json:"link"`
	PubDateRaw        string   `json:"pub_date_raw"`
	PubDateISO        string   `json:"pub_date_iso"`
	SourceDescription string   `json:"source_description"`
	Categories        []string `json:"categories"`
	ExtraDescription  string   `json:"extra_description"`
	Genres            []string `json:"genres"`
	ImageURLs         []string `json:"image_urls"`
	HeaderImage       string   `json:"header_image"`
	EnrichedAt        string   `json:"enriched_at,omitempty"`
	EnrichError       string   `json:"enrich_error,omitempty"`
}

type RunRecord struct {
	ID        int       `json:"id"`
	Trigger   string    `json:"trigger"` // startup, schedule, manual, cache
	Status    string    `json:"status"`  // ok, error, cancelled, cache
	Started   time.Time `json:"started"`
	Finished  time.Time `json:"finished"`
	Duration  float64   `json:"duration_s"`
	Items     int       `json:"items"`
	Enriched  int       `json:"enriched"`
	Reused    int       `json:"reused"`
	Failed    int       `json:"failed"`
	ImagesNew int       `json:"images_new"`
	Error     string    `json:"error,omitempty"`
}

type Store struct {
	dir         string
	imagesDir   string
	cacheFile   string
	rssFile     string
	manifest    string
	historyFile string

	mu       sync.RWMutex
	releases []Release
	mmu      sync.Mutex
	man      map[string]map[string]string
	hmu      sync.Mutex
	history  []RunRecord
}

func NewStore(dataDir string) *Store {
	s := &Store{
		dir:         dataDir,
		imagesDir:   filepath.Join(dataDir, "images"),
		cacheFile:   filepath.Join(dataDir, "releases_cache.json"),
		rssFile:     filepath.Join(dataDir, "feed.xml"),
		manifest:    filepath.Join(dataDir, "images_manifest.json"),
		historyFile: filepath.Join(dataDir, "runs.json"),
	}
	os.MkdirAll(s.imagesDir, 0o755)
	s.loadCache()
	s.loadManifest()
	if b, err := os.ReadFile(s.historyFile); err == nil {
		json.Unmarshal(b, &s.history)
	}
	return s
}

func (s *Store) loadCache() {
	b, err := os.ReadFile(s.cacheFile)
	if err != nil {
		return
	}
	var r []Release
	if json.Unmarshal(b, &r) == nil {
		s.releases = r
	}
}

func (s *Store) Releases() []Release {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Release, len(s.releases))
	copy(out, s.releases)
	return out
}

func (s *Store) Find(link string) (Release, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, r := range s.releases {
		if r.Link == link {
			return r, true
		}
	}
	return Release{}, false
}

func (s *Store) SaveReleases(r []Release) error {
	s.mu.Lock()
	s.releases = r
	s.mu.Unlock()
	b, _ := json.MarshalIndent(r, "", "  ")
	return atomicWrite(s.cacheFile, b)
}

func (s *Store) ClearCache() {
	s.mu.Lock()
	s.releases = nil
	s.mu.Unlock()
	os.Remove(s.cacheFile)
}

func (s *Store) CacheInfo() (mtime time.Time, ok bool) {
	st, err := os.Stat(s.cacheFile)
	if err != nil {
		return time.Time{}, false
	}
	return st.ModTime(), true
}

func (s *Store) CacheFresh(ttlHours float64) bool {
	m, ok := s.CacheInfo()
	if !ok || ttlHours <= 0 {
		return false
	}
	return time.Since(m) < time.Duration(ttlHours*float64(time.Hour))
}

// ── image manifest ──

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

type ImageStats struct {
	Count    int   `json:"count"`
	Bytes    int64 `json:"bytes"`
	Orphans  int   `json:"orphans"`
	OrphanSz int64 `json:"orphan_bytes"`
}

// referenced returns every image filename the current cache points at.
func (s *Store) referenced() map[string]bool {
	ref := map[string]bool{}
	for _, r := range s.Releases() {
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
	// prune manifest entries for threads no longer cached
	s.mmu.Lock()
	links := map[string]bool{}
	for _, r := range s.Releases() {
		links[r.Link] = true
	}
	for k := range s.man {
		if !links[k] {
			delete(s.man, k)
		}
	}
	s.mmu.Unlock()
	s.SaveManifest()
	return n, sz
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
	if len(s.history) > 100 {
		s.history = s.history[len(s.history)-100:]
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

func (s *Store) imageSize(name string) int64 {
	if st, err := os.Stat(filepath.Join(s.imagesDir, name)); err == nil {
		return st.Size()
	}
	return 0
}
