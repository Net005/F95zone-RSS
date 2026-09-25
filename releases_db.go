package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Release is the in-memory shape used by the scraper, feed builder and API.
// Labels are the bracket badges from the title (UPDATE, NEW, COMPLETED, ...).
// Tags are the thread's own Genre values. Engine and Version are parsed out
// separately because they're filtered/displayed on their own.
type Release struct {
	Link              string   `json:"link"`
	Title             string   `json:"title"`
	PubDateRaw        string   `json:"pub_date_raw"`
	PubDateISO        string   `json:"pub_date_iso"`
	SourceDescription string   `json:"source_description"`
	ExtraDescription  string   `json:"extra_description"`
	Overview          string   `json:"overview,omitempty"`
	Labels            []string `json:"labels"`
	Tags              []string `json:"tags"`
	Engine            string   `json:"engine"`
	Version           string   `json:"version"`
	ImageURLs         []string `json:"image_urls"`
	HeaderImage       string   `json:"header_image"`
	ThreadUpdated     string   `json:"thread_updated,omitempty"`
	ReleaseDate       string   `json:"release_date,omitempty"`
	Developer         string   `json:"developer,omitempty"`
	Censored          string   `json:"censored,omitempty"`
	OS                string   `json:"os,omitempty"`
	Language          string   `json:"language,omitempty"`
	Store             string   `json:"store,omitempty"`
	EnrichedAt        string   `json:"enriched_at,omitempty"`
	EnrichError       string   `json:"enrich_error,omitempty"`
	ParserVer         int      `json:"-"`
	FirstSeen         string   `json:"first_seen,omitempty"`
	LastSeen          string   `json:"last_seen,omitempty"`
	DiscoveredVia     string   `json:"discovered_via,omitempty"`
	Watched           bool     `json:"watched"`
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func jsonArr(v []string) string {
	if v == nil {
		v = []string{}
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func parseArr(s string) []string {
	var v []string
	json.Unmarshal([]byte(s), &v)
	if v == nil {
		v = []string{}
	}
	return v
}

func scanRelease(row interface {
	Scan(...any) error
}) (Release, error) {
	var r Release
	var labels, imgs string
	var watched int
	err := row.Scan(&r.Link, &r.Title, &r.PubDateRaw, &r.PubDateISO, &r.SourceDescription, &r.ExtraDescription,
		&r.Overview, &labels, &r.Engine, &r.Version, &imgs, &r.HeaderImage, &r.ThreadUpdated, &r.ReleaseDate, &r.Developer,
		&r.Censored, &r.OS, &r.Language, &r.Store, &r.EnrichedAt, &r.EnrichError, &r.ParserVer,
		&r.FirstSeen, &r.LastSeen, &r.DiscoveredVia, &watched)
	if err != nil {
		return r, err
	}
	// Tags come from the release_tags join table, filled in by the caller.
	r.Labels = parseArr(labels)
	r.ImageURLs = parseArr(imgs)
	r.Watched = watched == 1
	return r, nil
}

const releaseCols = `link, title, pub_date_raw, pub_date_iso, source_description, extra_description, overview,
	labels_json, engine, version, image_urls_json, header_image, thread_updated, release_date, developer,
	censored, os, language, store, enriched_at, enrich_error, parser_ver, first_seen, last_seen, discovered_via, watched`

func (d *DB) tagsFor(link string) []string {
	rows, err := d.sql.Query(`SELECT tag FROM release_tags WHERE link=? ORDER BY tag`, link)
	if err != nil {
		return []string{}
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var t string
		rows.Scan(&t)
		out = append(out, t)
	}
	return out
}

func (d *DB) GetRelease(link string) (Release, bool) {
	row := d.sql.QueryRow(`SELECT `+releaseCols+` FROM releases WHERE link=?`, link)
	r, err := scanRelease(row)
	if err != nil {
		return Release{}, false
	}
	r.Tags = d.tagsFor(link)
	return r, true
}

// UpsertRelease writes or updates a release row and its tag set, preserving first_seen.
// discoveredVia is only set on insert.
func (d *DB) UpsertRelease(r Release, discoveredVia string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var firstSeen string
	err = tx.QueryRow(`SELECT first_seen FROM releases WHERE link=?`, r.Link).Scan(&firstSeen)
	isNew := err == sql.ErrNoRows
	if isNew {
		firstSeen = now
	}
	if discoveredVia == "" {
		discoveredVia = "feed"
	}

	// watched is intentionally left out of the UPDATE SET clause below: a
	// human's "monitor this release" toggle must survive every re-scrape.
	// The bound value here only ever applies to a genuinely new row.
	_, err = tx.Exec(`INSERT INTO releases(`+releaseCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(link) DO UPDATE SET
			title=excluded.title, pub_date_raw=excluded.pub_date_raw, pub_date_iso=excluded.pub_date_iso,
			source_description=excluded.source_description, extra_description=excluded.extra_description,
			overview=excluded.overview,
			labels_json=excluded.labels_json, engine=excluded.engine, version=excluded.version,
			image_urls_json=excluded.image_urls_json, header_image=excluded.header_image,
			thread_updated=excluded.thread_updated, release_date=excluded.release_date, developer=excluded.developer,
			censored=excluded.censored, os=excluded.os, language=excluded.language, store=excluded.store,
			enriched_at=excluded.enriched_at, enrich_error=excluded.enrich_error, parser_ver=excluded.parser_ver,
			last_seen=excluded.last_seen`,
		r.Link, r.Title, r.PubDateRaw, r.PubDateISO, r.SourceDescription, r.ExtraDescription, r.Overview,
		jsonArr(r.Labels), r.Engine, r.Version, jsonArr(r.ImageURLs), r.HeaderImage, r.ThreadUpdated, r.ReleaseDate,
		r.Developer, r.Censored, r.OS, r.Language, r.Store, r.EnrichedAt, r.EnrichError, r.ParserVer,
		firstSeen, now, discoveredVia, boolToInt(r.Watched))
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM release_tags WHERE link=?`, r.Link); err != nil {
		return err
	}
	for _, t := range dedupTags(r.Tags) {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO release_tags(link,tag) VALUES(?,?)`, r.Link, t); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func dedupTags(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range in {
		t = strings.TrimSpace(t)
		if t == "" || seen[strings.ToLower(t)] {
			continue
		}
		seen[strings.ToLower(t)] = true
		out = append(out, t)
	}
	return out
}

type ReleaseFilter struct {
	Query       string
	Tags        []string // AND match
	Engine      string
	Failed      bool
	WatchedOnly bool
	Sort        string // newest, oldest, title
	Page        int
	PageSize    int
}

// SetWatched flags (or unflags) a release for "monitor this release"
// notifications. Returns sql.ErrNoRows if the link isn't in the database.
func (d *DB) SetWatched(link string, watched bool) error {
	res, err := d.sql.Exec(`UPDATE releases SET watched=? WHERE link=?`, boolToInt(watched), link)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// MostRecentEnrichedLink returns the most recently enriched release, used as
// a live probe thread when checking whether the configured site cookie is
// still a valid logged-in session.
func (d *DB) MostRecentEnrichedLink() (link, title string, ok bool) {
	err := d.sql.QueryRow(`SELECT link, title FROM releases WHERE enriched_at<>'' ORDER BY enriched_at DESC LIMIT 1`).Scan(&link, &title)
	return link, title, err == nil
}

type ReleasePage struct {
	Total int       `json:"total"`
	Page  int       `json:"page"`
	Pages int       `json:"pages"`
	Items []Release `json:"items"`
}

func (d *DB) QueryReleases(f ReleaseFilter) (ReleasePage, error) {
	where := []string{"1=1"}
	args := []any{}
	if f.Query != "" {
		where = append(where, "title LIKE ? ESCAPE '\\'")
		args = append(args, "%"+escLike(f.Query)+"%")
	}
	if f.Engine != "" {
		where = append(where, "engine = ?")
		args = append(args, f.Engine)
	}
	if f.Failed {
		where = append(where, "enrich_error <> ''")
	}
	if f.WatchedOnly {
		where = append(where, "watched = 1")
	}
	tagFilter := ""
	if len(f.Tags) > 0 {
		ph := strings.TrimRight(strings.Repeat("?,", len(f.Tags)), ",")
		tagFilter = fmt.Sprintf(` AND link IN (SELECT link FROM release_tags WHERE tag IN (%s) GROUP BY link HAVING COUNT(DISTINCT tag)=%d)`, ph, len(f.Tags))
		for _, t := range f.Tags {
			args = append(args, t)
		}
	}
	order := "pub_date_iso DESC, last_seen DESC"
	switch f.Sort {
	case "oldest":
		order = "pub_date_iso ASC, last_seen ASC"
	case "title":
		order = "title COLLATE NOCASE ASC"
	case "recent":
		order = "last_seen DESC"
	}
	whereSQL := strings.Join(where, " AND ") + tagFilter

	var total int
	if err := d.sql.QueryRow(`SELECT COUNT(*) FROM releases WHERE `+whereSQL, args...).Scan(&total); err != nil {
		return ReleasePage{}, err
	}
	if f.PageSize <= 0 || f.PageSize > 200 {
		f.PageSize = 60
	}
	if f.Page < 1 {
		f.Page = 1
	}
	pages := (total + f.PageSize - 1) / f.PageSize
	rows, err := d.sql.Query(`SELECT `+releaseCols+` FROM releases WHERE `+whereSQL+` ORDER BY `+order+` LIMIT ? OFFSET ?`,
		append(args, f.PageSize, (f.Page-1)*f.PageSize)...)
	if err != nil {
		return ReleasePage{}, err
	}
	defer rows.Close()
	items := []Release{}
	var links []string
	for rows.Next() {
		r, err := scanRelease(rows)
		if err != nil {
			return ReleasePage{}, err
		}
		items = append(items, r)
		links = append(links, r.Link)
	}
	tagsByLink := d.tagsForMany(links)
	for i := range items {
		items[i].Tags = tagsByLink[items[i].Link]
	}
	return ReleasePage{Total: total, Page: f.Page, Pages: pages, Items: items}, nil
}

func (d *DB) tagsForMany(links []string) map[string][]string {
	out := map[string][]string{}
	if len(links) == 0 {
		return out
	}
	ph := strings.TrimRight(strings.Repeat("?,", len(links)), ",")
	args := make([]any, len(links))
	for i, l := range links {
		args[i] = l
	}
	rows, err := d.sql.Query(`SELECT link, tag FROM release_tags WHERE link IN (`+ph+`) ORDER BY tag`, args...)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var link, tag string
		rows.Scan(&link, &tag)
		out[link] = append(out[link], tag)
	}
	return out
}

func escLike(s string) string {
	r := strings.NewReplacer("\\", "\\\\", "%", "\\%", "_", "\\_")
	return r.Replace(s)
}

type Count struct {
	Value string `json:"value"`
	Count int    `json:"count"`
}

func (d *DB) TagCounts(limit int) []Count {
	rows, err := d.sql.Query(`SELECT tag, COUNT(*) c FROM release_tags GROUP BY tag ORDER BY c DESC, tag ASC LIMIT ?`, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []Count
	for rows.Next() {
		var c Count
		rows.Scan(&c.Value, &c.Count)
		out = append(out, c)
	}
	return out
}

func (d *DB) EngineCounts() []Count {
	rows, err := d.sql.Query(`SELECT engine, COUNT(*) c FROM releases WHERE engine<>'' GROUP BY engine ORDER BY c DESC, engine ASC`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []Count
	for rows.Next() {
		var c Count
		rows.Scan(&c.Value, &c.Count)
		out = append(out, c)
	}
	return out
}

type ReleaseStats struct {
	Total        int `json:"total"`
	WithDesc     int `json:"with_description"`
	Failed       int `json:"failed"`
	NeedsReparse int `json:"needs_reparse"`
}

func (d *DB) Stats() ReleaseStats {
	var s ReleaseStats
	err := d.sql.QueryRow(`SELECT COUNT(*), SUM(extra_description<>''), SUM(enrich_error<>''), SUM(parser_ver<>?) FROM releases`, parserVersion).
		Scan(&s.Total, &nullInt{&s.WithDesc}, &nullInt{&s.Failed}, &nullInt{&s.NeedsReparse})
	if err != nil {
		return ReleaseStats{}
	}
	return s
}

// nullInt scans a possibly-NULL aggregate (SUM over zero rows) into an int, defaulting to 0.
type nullInt struct{ p *int }

func (n *nullInt) Scan(v any) error {
	if v == nil {
		*n.p = 0
		return nil
	}
	switch t := v.(type) {
	case int64:
		*n.p = int(t)
	case float64:
		*n.p = int(t)
	}
	return nil
}

func (d *DB) AllLinks() map[string]bool {
	rows, err := d.sql.Query(`SELECT link FROM releases`)
	if err != nil {
		return map[string]bool{}
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var l string
		rows.Scan(&l)
		out[l] = true
	}
	return out
}

func (d *DB) DeleteAllReleases() error {
	_, err := d.sql.Exec(`DELETE FROM releases`)
	return err
}

// ── notifications ──

type Notification struct {
	ID         int64    `json:"id"`
	Link       string   `json:"link"`
	Title      string   `json:"title"`
	Kind       string   `json:"kind"` // new, update
	OldVersion string   `json:"old_version"`
	NewVersion string   `json:"new_version"`
	Cover      string   `json:"cover"`
	Labels     []string `json:"labels"`
	Tags       []string `json:"tags"`
	CreatedAt  string   `json:"created_at"`
	Read       bool     `json:"read"`
	Pushed     bool     `json:"pushed"`
	PushError  string   `json:"push_error,omitempty"`
}

func (d *DB) AddNotification(n Notification) (int64, error) {
	res, err := d.sql.Exec(`INSERT INTO notifications(link,title,kind,old_version,new_version,cover,labels_json,tags_json,created_at,read,pushed,push_error)
		VALUES(?,?,?,?,?,?,?,?,?,0,?,?)`,
		n.Link, n.Title, n.Kind, n.OldVersion, n.NewVersion, n.Cover, jsonArr(n.Labels), jsonArr(n.Tags), n.CreatedAt, n.Pushed, n.PushError)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (d *DB) MarkPushed(id int64, err string) {
	d.sql.Exec(`UPDATE notifications SET pushed=1, push_error=? WHERE id=?`, err, id)
}

func (d *DB) Notifications(unreadOnly bool, limit int) []Notification {
	q := `SELECT id,link,title,kind,old_version,new_version,cover,labels_json,tags_json,created_at,read,pushed,push_error FROM notifications`
	if unreadOnly {
		q += ` WHERE read=0`
	}
	q += ` ORDER BY id DESC LIMIT ?`
	if limit <= 0 {
		limit = 200
	}
	rows, err := d.sql.Query(q, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := []Notification{}
	for rows.Next() {
		var n Notification
		var labels, tags string
		var read, pushed int
		rows.Scan(&n.ID, &n.Link, &n.Title, &n.Kind, &n.OldVersion, &n.NewVersion, &n.Cover, &labels, &tags, &n.CreatedAt, &read, &pushed, &n.PushError)
		n.Labels, n.Tags = parseArr(labels), parseArr(tags)
		n.Read, n.Pushed = read == 1, pushed == 1
		out = append(out, n)
	}
	return out
}

func (d *DB) UnreadCount() int {
	var n int
	d.sql.QueryRow(`SELECT COUNT(*) FROM notifications WHERE read=0`).Scan(&n)
	return n
}

func (d *DB) MarkNotificationRead(id int64, read bool) {
	v := 0
	if read {
		v = 1
	}
	d.sql.Exec(`UPDATE notifications SET read=? WHERE id=?`, v, id)
}

func (d *DB) MarkAllNotificationsRead() {
	d.sql.Exec(`UPDATE notifications SET read=1`)
}

func (d *DB) DeleteNotification(id int64) {
	d.sql.Exec(`DELETE FROM notifications WHERE id=?`, id)
}

func (d *DB) DeleteAllNotifications() {
	d.sql.Exec(`DELETE FROM notifications`)
}
