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
			labels_json TEXT NOT NULL DEFAULT '[]',
			engine TEXT NOT NULL DEFAULT '',
			version TEXT NOT NULL DEFAULT '',
			image_urls_json TEXT NOT NULL DEFAULT '[]',
			header_image TEXT NOT NULL DEFAULT '',
			thread_updated TEXT NOT NULL DEFAULT '',
			release_date TEXT NOT NULL DEFAULT '',
			developer TEXT NOT NULL DEFAULT '',
			censored TEXT NOT NULL DEFAULT '',
			os TEXT NOT NULL DEFAULT '',
			language TEXT NOT NULL DEFAULT '',
			store TEXT NOT NULL DEFAULT '',
			enriched_at TEXT NOT NULL DEFAULT '',
			enrich_error TEXT NOT NULL DEFAULT '',
			parser_ver INTEGER NOT NULL DEFAULT 0,
			first_seen TEXT NOT NULL,
			last_seen TEXT NOT NULL,
			discovered_via TEXT NOT NULL DEFAULT 'feed')`,
		`CREATE INDEX IF NOT EXISTS idx_releases_pub ON releases(pub_date_iso DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_releases_engine ON releases(engine)`,
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
	return &DB{d}, nil
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
