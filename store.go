package main

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver: the build stays CGO_ENABLED=0
)

var ErrNotFound = errors.New("not found")

// tokenPrefix starts every API token, so grab can tell its own bearer tokens
// from ones a reverse proxy forwards.
const tokenPrefix = "grab_"

type User struct {
	ID, Dir, Name string
	Admin         bool
	DefaultTarget string
}

type Download struct {
	ID, UserID, SharedURL, MeTubeURL, Site, Target string
	Folder, Filename, Title                        string // Folder relative to SharesDir
	Public                                         bool
	Status                                         string // waiting|queued|downloading|finished|error
	Percent                                        float64
	Error                                          string
	Size                                           int64
	CreatedAt, FinishedAt, NotifiedAt, QueuedAt    time.Time // zero = unset
	// MeTubeTS is the timestamp (Unix ns) of the MeTube history entry this
	// download was paired with; 0 = not paired yet. MeTube reuses one URL
	// key for every download of a video, so (MeTubeURL, MeTubeTS) is what
	// identifies "our" entry.
	MeTubeTS int64
}

// ListFilter: the viewer's own downloads plus public ones; Admin sees all.
type ListFilter struct {
	ViewerID string
	Admin    bool
	Limit    int
}

type Token struct {
	ID, UserID, Label   string
	CreatedAt, LastUsed time.Time
}

type Store struct{ db *sql.DB }

// Times are stored as Unix nanoseconds with 0 meaning "unset", so the zero
// time.Time round-trips exactly (time.Time{}.UnixNano() is not 0).
func tn(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixNano()
}

func fn(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

var migrations = []string{`
CREATE TABLE users (
	id TEXT PRIMARY KEY,
	dir TEXT NOT NULL UNIQUE,
	name TEXT NOT NULL,
	admin INTEGER NOT NULL DEFAULT 0,
	default_target TEXT NOT NULL DEFAULT 'original',
	notify_urls TEXT NOT NULL DEFAULT ''
);
CREATE TABLE downloads (
	id TEXT PRIMARY KEY,
	user_id TEXT NOT NULL,
	shared_url TEXT NOT NULL DEFAULT '',
	metube_url TEXT NOT NULL DEFAULT '',
	site TEXT NOT NULL DEFAULT '',
	target TEXT NOT NULL DEFAULT '',
	folder TEXT NOT NULL DEFAULT '',
	filename TEXT NOT NULL DEFAULT '',
	title TEXT NOT NULL DEFAULT '',
	public INTEGER NOT NULL DEFAULT 0,
	status TEXT NOT NULL DEFAULT 'waiting',
	percent REAL NOT NULL DEFAULT 0,
	error TEXT NOT NULL DEFAULT '',
	size INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL DEFAULT 0,
	finished_at INTEGER NOT NULL DEFAULT 0,
	notified_at INTEGER NOT NULL DEFAULT 0,
	queued_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX downloads_user ON downloads(user_id);
CREATE INDEX downloads_status ON downloads(status);
CREATE TABLE tokens (
	id TEXT PRIMARY KEY,
	user_id TEXT NOT NULL,
	label TEXT NOT NULL DEFAULT '',
	hash TEXT NOT NULL UNIQUE,
	created_at INTEGER NOT NULL,
	last_used INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE sessions (
	hash TEXT PRIMARY KEY,
	user_id TEXT NOT NULL,
	expires_at INTEGER NOT NULL
);`, `
ALTER TABLE downloads ADD COLUMN metube_ts INTEGER NOT NULL DEFAULT 0;`}

func OpenStore(path string) (*Store, error) {
	// Create the file ourselves so it is 0600 from the start.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	f.Close()
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate() error {
	var v int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		return err
	}
	for ; v < len(migrations); v++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[v]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", v+1, err)
		}
		// PRAGMA takes no bound parameters; v is an int we control.
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", v+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) Close() error { return s.db.Close() }

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failing is unrecoverable
	}
	return hex.EncodeToString(b)
}

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// ---- users ----

func userDirBase(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			b.WriteRune(r)
		}
	}
	d := b.String()
	if len(d) > 32 {
		d = d[:32]
	}
	if d == "" {
		d = "user"
	}
	return d
}

const userCols = "id, dir, name, admin, default_target"

type scanner interface{ Scan(...any) error }

func scanUser(r scanner) (User, error) {
	var u User
	err := r.Scan(&u.ID, &u.Dir, &u.Name, &u.Admin, &u.DefaultTarget)
	if errors.Is(err, sql.ErrNoRows) {
		return u, ErrNotFound
	}
	return u, err
}

func (s *Store) UpsertUser(id, preferredName string, admin bool) (User, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback()
	res, err := tx.Exec("UPDATE users SET name=?, admin=? WHERE id=?", preferredName, admin, id)
	if err != nil {
		return User{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		base := userDirBase(preferredName)
		dir := base
		for i := 2; ; i++ {
			var one int
			err := tx.QueryRow("SELECT 1 FROM users WHERE dir=?", dir).Scan(&one)
			if errors.Is(err, sql.ErrNoRows) {
				break
			}
			if err != nil {
				return User{}, err
			}
			dir = fmt.Sprintf("%s-%d", base, i)
		}
		if _, err := tx.Exec("INSERT INTO users(id, dir, name, admin, default_target) VALUES(?,?,?,?,'original')",
			id, dir, preferredName, admin); err != nil {
			return User{}, err
		}
	}
	u, err := scanUser(tx.QueryRow("SELECT "+userCols+" FROM users WHERE id=?", id))
	if err != nil {
		return User{}, err
	}
	return u, tx.Commit()
}

func (s *Store) UserByID(id string) (User, error) {
	return scanUser(s.db.QueryRow("SELECT "+userCols+" FROM users WHERE id=?", id))
}

func (s *Store) UserByDir(dir string) (User, error) {
	return scanUser(s.db.QueryRow("SELECT "+userCols+" FROM users WHERE dir=?", dir))
}

func (s *Store) Users() (map[string]User, error) {
	rows, err := s.db.Query("SELECT " + userCols + " FROM users")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]User{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out[u.ID] = u
	}
	return out, rows.Err()
}

func (s *Store) SetDefaultTarget(userID, target string) error {
	_, err := s.db.Exec("UPDATE users SET default_target=? WHERE id=?", target, userID)
	return err
}

func (s *Store) SetNotifyURLs(userID string, urls []string) error {
	enc := ""
	if len(urls) > 0 {
		b, err := json.Marshal(urls)
		if err != nil {
			return err
		}
		enc = string(b)
	}
	_, err := s.db.Exec("UPDATE users SET notify_urls=? WHERE id=?", enc, userID)
	return err
}

func (s *Store) NotifyURLs(userID string) ([]string, error) {
	var enc string
	if err := s.db.QueryRow("SELECT notify_urls FROM users WHERE id=?", userID).Scan(&enc); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if enc == "" {
		return nil, nil
	}
	var urls []string
	return urls, json.Unmarshal([]byte(enc), &urls)
}

// ---- downloads ----

const dlCols = "id, user_id, shared_url, metube_url, site, target, folder, filename, title, public, status, percent, error, size, created_at, finished_at, notified_at, queued_at, metube_ts"

func scanDownload(r scanner) (Download, error) {
	var d Download
	var c, f, n, q int64
	err := r.Scan(&d.ID, &d.UserID, &d.SharedURL, &d.MeTubeURL, &d.Site, &d.Target, &d.Folder, &d.Filename,
		&d.Title, &d.Public, &d.Status, &d.Percent, &d.Error, &d.Size, &c, &f, &n, &q, &d.MeTubeTS)
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrNotFound
	}
	d.CreatedAt, d.FinishedAt, d.NotifiedAt, d.QueuedAt = fn(c), fn(f), fn(n), fn(q)
	return d, err
}

func (s *Store) queryDownloads(q string, args ...any) ([]Download, error) {
	rows, err := s.db.Query("SELECT "+dlCols+" FROM downloads "+q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Download
	for rows.Next() {
		d, err := scanDownload(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

const (
	insertDownload  = "INSERT INTO downloads(" + dlCols + ") VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)"
	replaceDownload = "INSERT OR REPLACE INTO downloads(" + dlCols + ") VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)"
)

func dlArgs(d Download) []any {
	return []any{d.ID, d.UserID, d.SharedURL, d.MeTubeURL, d.Site, d.Target, d.Folder, d.Filename, d.Title,
		d.Public, d.Status, d.Percent, d.Error, d.Size, tn(d.CreatedAt), tn(d.FinishedAt), tn(d.NotifiedAt), tn(d.QueuedAt), d.MeTubeTS}
}

func (s *Store) CreateDownload(d Download) (Download, error) {
	if d.ID == "" {
		d.ID = randHex(8)
	}
	if d.CreatedAt.IsZero() {
		d.CreatedAt = time.Now()
	}
	_, err := s.db.Exec(insertDownload, dlArgs(d)...)
	return d, err
}

func (s *Store) GetDownload(id string) (Download, error) {
	return scanDownload(s.db.QueryRow("SELECT "+dlCols+" FROM downloads WHERE id=?", id))
}

func (s *Store) ListDownloads(f ListFilter) ([]Download, error) {
	q := "WHERE 1"
	var args []any
	if !f.Admin {
		q = "WHERE user_id=? OR public=1"
		args = append(args, f.ViewerID)
	}
	q += " ORDER BY created_at DESC, id"
	if f.Limit > 0 {
		q += " LIMIT ?"
		args = append(args, f.Limit)
	}
	return s.queryDownloads(q, args...)
}

// UpdateDownload runs fn on the row inside one transaction; an error from fn
// rolls the change back.
func (s *Store) UpdateDownload(id string, fnc func(*Download) error) (Download, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Download{}, err
	}
	defer tx.Rollback()
	d, err := scanDownload(tx.QueryRow("SELECT "+dlCols+" FROM downloads WHERE id=?", id))
	if err != nil {
		return Download{}, err
	}
	if err := fnc(&d); err != nil {
		return Download{}, err
	}
	d.ID = id // the key is not editable
	if _, err := tx.Exec(replaceDownload, dlArgs(d)...); err != nil {
		return Download{}, err
	}
	return d, tx.Commit()
}

func (s *Store) DeleteDownload(id string) error {
	_, err := s.db.Exec("DELETE FROM downloads WHERE id=?", id)
	return err
}

func (s *Store) Active() ([]Download, error) {
	return s.queryDownloads("WHERE status IN ('waiting','queued','downloading') ORDER BY created_at, id")
}

// UserShared returns the user's latest download of sharedURL (ErrNotFound if none).
func (s *Store) UserShared(userID, sharedURL string) (Download, error) {
	ds, err := s.queryDownloads("WHERE user_id=? AND shared_url=? ORDER BY created_at DESC, id LIMIT 1", userID, sharedURL)
	if err != nil {
		return Download{}, err
	}
	if len(ds) == 0 {
		return Download{}, ErrNotFound
	}
	return ds[0], nil
}

// InFolder lists every download filed under folder (relative to SharesDir).
func (s *Store) InFolder(folder string) ([]Download, error) {
	return s.queryDownloads("WHERE folder=? ORDER BY created_at, id", folder)
}

func (s *Store) Unnotified() ([]Download, error) {
	return s.queryDownloads("WHERE status IN ('finished','error') AND notified_at=0 ORDER BY created_at, id")
}

func (s *Store) ExpiredPrivate(before time.Time) ([]Download, error) {
	return s.queryDownloads("WHERE public=0 AND status IN ('finished','error') AND created_at<? ORDER BY created_at, id", tn(before))
}

// ---- tokens ----

func (s *Store) CreateToken(userID, label string) (string, Token, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", Token{}, err
	}
	plain := tokenPrefix + base64.RawURLEncoding.EncodeToString(b)
	t := Token{ID: randHex(8), UserID: userID, Label: label, CreatedAt: time.Now()}
	// Only the hash is stored: the plaintext cannot be recovered later.
	_, err := s.db.Exec("INSERT INTO tokens(id, user_id, label, hash, created_at) VALUES(?,?,?,?,?)",
		t.ID, userID, label, sha(plain), tn(t.CreatedAt))
	if err != nil {
		return "", Token{}, err
	}
	return plain, t, nil
}

func (s *Store) TokenUser(plaintext string) (User, error) {
	h := sha(plaintext)
	u, err := scanUser(s.db.QueryRow(`SELECT u.id, u.dir, u.name, u.admin, u.default_target
		FROM tokens t JOIN users u ON u.id=t.user_id WHERE t.hash=?`, h))
	if err != nil {
		return User{}, err
	}
	_, err = s.db.Exec("UPDATE tokens SET last_used=? WHERE hash=?", time.Now().UnixNano(), h)
	return u, err
}

func (s *Store) ListTokens(userID string) ([]Token, error) {
	rows, err := s.db.Query("SELECT id, user_id, label, created_at, last_used FROM tokens WHERE user_id=? ORDER BY created_at, id", userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Token
	for rows.Next() {
		var t Token
		var c, l int64
		if err := rows.Scan(&t.ID, &t.UserID, &t.Label, &c, &l); err != nil {
			return nil, err
		}
		t.CreatedAt, t.LastUsed = fn(c), fn(l)
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) DeleteToken(userID, tokenID string) error {
	_, err := s.db.Exec("DELETE FROM tokens WHERE id=? AND user_id=?", tokenID, userID)
	return err
}

// ---- sessions ----

func (s *Store) CreateSession(userID string, ttl time.Duration) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	sid := base64.RawURLEncoding.EncodeToString(b)
	_, err := s.db.Exec("INSERT INTO sessions(hash, user_id, expires_at) VALUES(?,?,?)",
		sha(sid), userID, time.Now().Add(ttl).UnixNano())
	if err != nil {
		return "", err
	}
	// Opportunistic cleanup keeps the table from growing without bound.
	s.db.Exec("DELETE FROM sessions WHERE expires_at<?", time.Now().UnixNano())
	return sid, nil
}

func (s *Store) SessionUser(sid string) (User, error) {
	return scanUser(s.db.QueryRow(`SELECT u.id, u.dir, u.name, u.admin, u.default_target
		FROM sessions x JOIN users u ON u.id=x.user_id WHERE x.hash=? AND x.expires_at>?`,
		sha(sid), time.Now().UnixNano()))
}

func (s *Store) DeleteSession(sid string) error {
	_, err := s.db.Exec("DELETE FROM sessions WHERE hash=?", sha(sid))
	return err
}

// DownloadsByFile lists every download whose file is folder/filename. One
// video shared under two link spellings gives two records for one file, so
// moving or deleting the file has to account for all of them.
func (s *Store) DownloadsByFile(folder, filename string) ([]Download, error) {
	if filename == "" {
		return nil, nil
	}
	return s.queryDownloads("WHERE folder=? AND filename=? ORDER BY created_at, id", folder, filename)
}
