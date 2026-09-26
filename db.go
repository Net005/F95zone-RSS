package main

import (
	"database/sql"
	"time"

	_ "modernc.org/sqlite"
)

// DB holds settings, the admin account and login sessions (SQLite, pure Go).
type DB struct{ sql *sql.DB }

func OpenDB(path string) (*DB, error) {
	d, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	d.SetMaxOpenConns(1)
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS settings(key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS users(
			id INTEGER PRIMARY KEY, username TEXT NOT NULL UNIQUE COLLATE NOCASE,
			pass_hash TEXT NOT NULL, created_at INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS sessions(
			token_hash TEXT PRIMARY KEY, user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL, ip TEXT, ua TEXT)`,
		`CREATE TABLE IF NOT EXISTS releases(
			link TEXT PRIMARY KEY,
			title TEXT NOT NULL,
			pub_date_raw TEXT NOT NULL DEFAULT '',
			pub_date_iso TEXT NOT NULL DEFAULT '',
			source_description TEXT NOT NULL DEFAULT '',
			extra_description TEXT NOT NULL DEFAULT '',
			overview TEXT NOT NULL DEFAULT '',
			labels_json TEXT NOT NULL DEFAULT '[]',
			engine TEXT NOT NULL DEFAULT '',
			version TEXT NOT NULL DEFAULT '',
			image_urls_json TEXT NOT NULL DEFAULT '[]',
			header_image TEXT NOT NULL DEFAULT '',
			thread_updated TEXT NOT NULL DEFAULT '',
			thread_updated_iso TEXT NOT NULL DEFAULT '',
			release_date TEXT NOT NULL DEFAULT '',
			developer TEXT NOT NULL DEFAULT '',
			censored TEXT NOT NULL DEFAULT '',
			os TEXT NOT NULL DEFAULT '',
			language TEXT NOT NULL DEFAULT '',
			store TEXT NOT NULL DEFAULT '',
			changelog TEXT NOT NULL DEFAULT '',
			downloads_json TEXT NOT NULL DEFAULT '[]',
			enriched_at TEXT NOT NULL DEFAULT '',
			enrich_error TEXT NOT NULL DEFAULT '',
			parser_ver INTEGER NOT NULL DEFAULT 0,
			first_seen TEXT NOT NULL,
			last_seen TEXT NOT NULL,
			discovered_via TEXT NOT NULL DEFAULT 'feed')`,
		`CREATE INDEX IF NOT EXISTS idx_releases_pub ON releases(pub_date_iso DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_releases_engine ON releases(engine)`,
		// idx_releases_thread_updated is NOT created here: on an existing database
		// (the common case - this only runs once per new install) the CREATE TABLE
		// above is a no-op because the table already exists, so the column this
		// index references doesn't exist until migrateThreadUpdatedISOColumn adds
		// it further down. Creating the index here unconditionally broke every
		// upgrade from a pre-6.5.6 database: this whole statement list runs before
		// any migration, so "no such column: thread_updated_iso" failed here and
		// OpenDB returned an error before ever reaching the migration that would
		// have added it - the app couldn't even start. The index is created inside
		// the migration itself instead, after the ALTER TABLE that adds the column.
		`CREATE TABLE IF NOT EXISTS release_tags(
			link TEXT NOT NULL REFERENCES releases(link) ON DELETE CASCADE,
			tag TEXT NOT NULL,
			PRIMARY KEY(link, tag))`,
		`CREATE INDEX IF NOT EXISTS idx_release_tags_tag ON release_tags(tag)`,
		`CREATE TABLE IF NOT EXISTS notifications(
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			link TEXT NOT NULL,
			title TEXT NOT NULL,
			kind TEXT NOT NULL,
			old_version TEXT NOT NULL DEFAULT '',
			new_version TEXT NOT NULL DEFAULT '',
			cover TEXT NOT NULL DEFAULT '',
			labels_json TEXT NOT NULL DEFAULT '[]',
			tags_json TEXT NOT NULL DEFAULT '[]',
			created_at TEXT NOT NULL,
			read INTEGER NOT NULL DEFAULT 0,
			pushed INTEGER NOT NULL DEFAULT 0,
			push_error TEXT NOT NULL DEFAULT '')`,
		`CREATE INDEX IF NOT EXISTS idx_notif_created ON notifications(created_at DESC)`,
	} {
		if _, err := d.Exec(q); err != nil {
			return nil, err
		}
	}
	db := &DB{d}
	if err := db.migrateWatchedColumn(); err != nil {
		return nil, err
	}
	if err := db.migrateOverviewColumn(); err != nil {
		return nil, err
	}
	if err := db.migrateChangelogDownloadsColumns(); err != nil {
		return nil, err
	}
	if err := db.migrateThreadUpdatedISOColumn(); err != nil {
		return nil, err
	}
	if err := db.migrateValidateThreadUpdatedISO(); err != nil {
		return nil, err
	}
	return db, nil
}

// migrateOverviewColumn adds releases.overview to databases created before the
// dedicated Overview field existed. Same pattern as migrateWatchedColumn.
func (d *DB) migrateOverviewColumn() error {
	rows, err := d.sql.Query(`PRAGMA table_info(releases)`)
	if err != nil {
		return err
	}
	has := false
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		if name == "overview" {
			has = true
		}
	}
	rows.Close()
	if !has {
		if _, err := d.sql.Exec(`ALTER TABLE releases ADD COLUMN overview TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	return nil
}

// migrateChangelogDownloadsColumns adds releases.changelog/downloads_json to
// databases created before those fields existed. Same pattern as
// migrateOverviewColumn.
func (d *DB) migrateChangelogDownloadsColumns() error {
	rows, err := d.sql.Query(`PRAGMA table_info(releases)`)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		have[name] = true
	}
	rows.Close()
	if !have["changelog"] {
		if _, err := d.sql.Exec(`ALTER TABLE releases ADD COLUMN changelog TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	if !have["downloads_json"] {
		if _, err := d.sql.Exec(`ALTER TABLE releases ADD COLUMN downloads_json TEXT NOT NULL DEFAULT '[]'`); err != nil {
			return err
		}
	}
	return nil
}

// migrateWatchedColumn adds releases.watched to databases created before the
// "monitor this release" feature existed. SQLite has no ADD COLUMN IF NOT
// EXISTS, so the existing schema is checked first.
func (d *DB) migrateWatchedColumn() error {
	rows, err := d.sql.Query(`PRAGMA table_info(releases)`)
	if err != nil {
		return err
	}
	has := false
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		if name == "watched" {
			has = true
		}
	}
	rows.Close()
	if !has {
		if _, err := d.sql.Exec(`ALTER TABLE releases ADD COLUMN watched INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
	}
	_, err = d.sql.Exec(`CREATE INDEX IF NOT EXISTS idx_releases_watched ON releases(watched)`)
	return err
}

func (d *DB) GetSetting(key string) (string, bool) {
	var v string
	if err := d.sql.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v); err != nil {
		return "", false
	}
	return v, true
}

func (d *DB) SetSetting(key, val string) error {
	_, err := d.sql.Exec(`INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, val)
	return err
}

func (d *DB) UserCount() int {
	var n int
	d.sql.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n
}

// CreateFirstUser inserts the admin only while no user exists (setup race safe).
func (d *DB) CreateFirstUser(name, hash string) (int64, bool, error) {
	res, err := d.sql.Exec(`INSERT INTO users(username,pass_hash,created_at) SELECT ?,?,? WHERE NOT EXISTS (SELECT 1 FROM users)`,
		name, hash, time.Now().Unix())
	if err != nil {
		return 0, false, err
	}
	n, _ := res.RowsAffected()
	id, _ := res.LastInsertId()
	return id, n == 1, nil
}

func (d *DB) GetUser(name string) (id int64, hash string, ok bool) {
	err := d.sql.QueryRow(`SELECT id, pass_hash FROM users WHERE username=?`, name).Scan(&id, &hash)
	return id, hash, err == nil
}

func (d *DB) UserHash(id int64) (string, bool) {
	var h string
	err := d.sql.QueryRow(`SELECT pass_hash FROM users WHERE id=?`, id).Scan(&h)
	return h, err == nil
}

func (d *DB) SetPassword(id int64, hash string) error {
	_, err := d.sql.Exec(`UPDATE users SET pass_hash=? WHERE id=?`, hash, id)
	return err
}

func (d *DB) CreateSession(uid int64, tokenHash string, ttl time.Duration, ip, ua string) error {
	now := time.Now()
	d.sql.Exec(`DELETE FROM sessions WHERE expires_at < ?`, now.Unix())
	_, err := d.sql.Exec(`INSERT INTO sessions(token_hash,user_id,created_at,expires_at,ip,ua) VALUES(?,?,?,?,?,?)`,
		tokenHash, uid, now.Unix(), now.Add(ttl).Unix(), ip, ua)
	return err
}

func (d *DB) SessionUser(tokenHash string) (int64, string, bool) {
	var id int64
	var name string
	err := d.sql.QueryRow(`SELECT u.id, u.username FROM sessions s JOIN users u ON u.id=s.user_id WHERE s.token_hash=? AND s.expires_at>?`,
		tokenHash, time.Now().Unix()).Scan(&id, &name)
	return id, name, err == nil
}

func (d *DB) DeleteSession(tokenHash string) {
	d.sql.Exec(`DELETE FROM sessions WHERE token_hash=?`, tokenHash)
}

func (d *DB) DeleteOtherSessions(uid int64, keep string) {
	d.sql.Exec(`DELETE FROM sessions WHERE user_id=? AND token_hash<>?`, uid, keep)
}

// migrateThreadUpdatedISOColumn adds releases.thread_updated_iso to databases
// created before the default sort was changed to match F95zone's own
// latest-alpha listing (most recent thread update, not our first-seen /
// publish date). For rows that already have a plain "Thread updated" date in
// the existing well-known "2006-01-02" shape, backfill the ISO column
// directly in SQL so the corrected sort applies immediately without waiting
// for every release to be re-scraped.
func (d *DB) migrateThreadUpdatedISOColumn() error {
	rows, err := d.sql.Query(`PRAGMA table_info(releases)`)
	if err != nil {
		return err
	}
	has := false
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		if name == "thread_updated_iso" {
			has = true
		}
	}
	rows.Close()
	if !has {
		if _, err := d.sql.Exec(`ALTER TABLE releases ADD COLUMN thread_updated_iso TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	if _, err := d.sql.Exec(`CREATE INDEX IF NOT EXISTS idx_releases_thread_updated ON releases(thread_updated_iso DESC)`); err != nil {
		return err
	}
	// Backfill in Go, not a blind SQL UPDATE - the original version of this
	// used `thread_updated GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]'`
	// to pick rows shaped like a date, but GLOB only checks digit shape, not
	// whether it's a real calendar date. A garbled OP field like "2026-60-27"
	// (month 60) matches that shape fine and got written through as-is,
	// which then sorted ahead of every legitimately-dated release under a
	// plain text ORDER BY and rendered as "NaN days ago" client-side. Parsing
	// each candidate for real before writing it (same check applied live in
	// parseFieldDate) is the fix; migrateValidateThreadUpdatedISO below
	// cleans up rows an earlier, buggy run of this migration already wrote.
	rows2, err := d.sql.Query(`SELECT link, thread_updated FROM releases WHERE (thread_updated_iso IS NULL OR thread_updated_iso = '') AND thread_updated <> ''`)
	if err != nil {
		return err
	}
	type upd struct{ link, iso string }
	var updates []upd
	for rows2.Next() {
		var link, raw string
		if err := rows2.Scan(&link, &raw); err != nil {
			rows2.Close()
			return err
		}
		if t, err := time.Parse("2006-01-02", raw); err == nil {
			updates = append(updates, upd{link, t.UTC().Format("2006-01-02T15:04:05-07:00")})
		}
	}
	rows2.Close()
	for _, u := range updates {
		if _, err := d.sql.Exec(`UPDATE releases SET thread_updated_iso = ? WHERE link = ?`, u.iso, u.link); err != nil {
			return err
		}
	}
	return nil
}

// migrateValidateThreadUpdatedISO clears any thread_updated_iso value that
// isn't an actually-valid calendar date - specifically to clean up rows an
// earlier, buggier version of migrateThreadUpdatedISOColumn already wrote
// (see its comment). Cheap and idempotent: once a database is clean this
// finds nothing to do on every subsequent startup.
func (d *DB) migrateValidateThreadUpdatedISO() error {
	rows, err := d.sql.Query(`SELECT link, thread_updated_iso FROM releases WHERE thread_updated_iso <> ''`)
	if err != nil {
		return err
	}
	var bad []string
	for rows.Next() {
		var link, iso string
		if err := rows.Scan(&link, &iso); err != nil {
			rows.Close()
			return err
		}
		if _, err := time.Parse(time.RFC3339, iso); err != nil {
			bad = append(bad, link)
		}
	}
	rows.Close()
	for _, link := range bad {
		if _, err := d.sql.Exec(`UPDATE releases SET thread_updated_iso = '' WHERE link = ?`, link); err != nil {
			return err
		}
	}
	return nil
}
