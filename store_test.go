package main

import (
	"bytes"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

func testStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "grab.sqlite3")
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func TestUpsertUserAssignsStableDir(t *testing.T) {
	s, _ := testStore(t)
	u, err := s.UpsertUser("sub-1", "Alex Q", false)
	if err != nil {
		t.Fatal(err)
	}
	if u.Dir != "alexq" || u.DefaultTarget != "original" {
		t.Fatalf("got %+v", u)
	}
	if err := s.SetDefaultTarget("sub-1", "telegram"); err != nil {
		t.Fatal(err)
	}
	u, err = s.UpsertUser("sub-1", "Someone Else", true)
	if err != nil {
		t.Fatal(err)
	}
	if u.Dir != "alexq" || u.Name != "Someone Else" || !u.Admin || u.DefaultTarget != "telegram" {
		t.Fatalf("got %+v", u)
	}
	for _, get := range []func() (User, error){
		func() (User, error) { return s.UserByID("sub-1") },
		func() (User, error) { return s.UserByDir("alexq") },
	} {
		g, err := get()
		if err != nil || g != u {
			t.Fatalf("lookup: %+v %v", g, err)
		}
	}
	if _, err := s.UserByID("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if _, err := s.UserByDir("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	all, err := s.Users()
	if err != nil || len(all) != 1 || all["sub-1"] != u {
		t.Fatalf("users: %+v %v", all, err)
	}
}

func TestUpsertUserDirCollision(t *testing.T) {
	s, _ := testStore(t)
	a, _ := s.UpsertUser("a", "alex", false)
	b, _ := s.UpsertUser("b", "alex", false)
	c, _ := s.UpsertUser("c", "alex", false)
	if a.Dir != "alex" || b.Dir != "alex-2" || c.Dir != "alex-3" {
		t.Fatalf("dirs %q %q %q", a.Dir, b.Dir, c.Dir)
	}
}

func TestUserDirSanitised(t *testing.T) {
	s, _ := testStore(t)
	u, err := s.UpsertUser("a", "../Évil Name!!", false)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[a-z0-9_-]+$`).MatchString(u.Dir) || strings.Contains(u.Dir, "..") {
		t.Fatalf("dir %q", u.Dir)
	}
	e, _ := s.UpsertUser("b", "", false)
	if e.Dir != "user" {
		t.Fatalf("dir %q", e.Dir)
	}
	long, _ := s.UpsertUser("c", strings.Repeat("x", 80), false)
	if len(long.Dir) != 32 {
		t.Fatalf("dir %q", long.Dir)
	}
}

func ids(ds []Download) []string {
	var out []string
	for _, d := range ds {
		out = append(out, d.ID)
	}
	sort.Strings(out)
	return out
}

func TestListDownloadsVisibility(t *testing.T) {
	s, _ := testStore(t)
	s.UpsertUser("A", "a", false)
	s.UpsertUser("B", "b", false)
	mk := func(id, user string, pub bool) {
		if _, err := s.CreateDownload(Download{ID: id, UserID: user, Public: pub, Status: "finished"}); err != nil {
			t.Fatal(err)
		}
	}
	mk("d1", "A", false)
	mk("d2", "A", true)
	mk("d3", "B", false)
	got, err := s.ListDownloads(ListFilter{ViewerID: "B"})
	if err != nil {
		t.Fatal(err)
	}
	if g := ids(got); strings.Join(g, ",") != "d2,d3" {
		t.Fatalf("viewer B: %v", g)
	}
	got, _ = s.ListDownloads(ListFilter{ViewerID: "B", Admin: true})
	if g := ids(got); strings.Join(g, ",") != "d1,d2,d3" {
		t.Fatalf("admin: %v", g)
	}
	got, _ = s.ListDownloads(ListFilter{Admin: true, Limit: 2})
	if len(got) != 2 {
		t.Fatalf("limit: %d", len(got))
	}
}

func TestDownloadRoundTripAndLifecycle(t *testing.T) {
	s, _ := testStore(t)
	s.UpsertUser("A", "a", false)
	d, err := s.CreateDownload(Download{UserID: "A", SharedURL: "https://example.com/v", Status: "waiting", Public: true, Percent: 1.5, Size: 9})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.ID) != 16 || d.CreatedAt.IsZero() {
		t.Fatalf("got %+v", d)
	}
	g, err := s.GetDownload(d.ID)
	if err != nil || !g.CreatedAt.Equal(d.CreatedAt) || g.FinishedAt != (time.Time{}) || g.QueuedAt != (time.Time{}) || g.NotifiedAt != (time.Time{}) {
		t.Fatalf("get %+v %v", g, err)
	}
	if g.SharedURL != d.SharedURL || !g.Public || g.Percent != 1.5 || g.Size != 9 {
		t.Fatalf("get %+v", g)
	}
	act, _ := s.Active()
	if len(act) != 1 {
		t.Fatalf("active %d", len(act))
	}
	now := time.Now()
	if _, err := s.UpdateDownload(d.ID, func(x *Download) error {
		x.Status = "finished"
		x.FinishedAt = now
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if act, _ = s.Active(); len(act) != 0 {
		t.Fatalf("active %d", len(act))
	}
	if un, _ := s.Unnotified(); len(un) != 1 {
		t.Fatalf("unnotified %d", len(un))
	}
	s.UpdateDownload(d.ID, func(x *Download) error { x.NotifiedAt = now; return nil })
	if un, _ := s.Unnotified(); len(un) != 0 {
		t.Fatalf("unnotified %d", len(un))
	}
	// Public rows never expire; private ones do once old enough.
	if ex, _ := s.ExpiredPrivate(now.Add(time.Hour)); len(ex) != 0 {
		t.Fatalf("public expired: %d", len(ex))
	}
	p, _ := s.CreateDownload(Download{UserID: "A", Status: "error"})
	if ex, _ := s.ExpiredPrivate(time.Now().Add(time.Hour)); len(ex) != 1 || ex[0].ID != p.ID {
		t.Fatalf("expired %+v", ex)
	}
	if ex, _ := s.ExpiredPrivate(p.CreatedAt.Add(-time.Hour)); len(ex) != 0 {
		t.Fatalf("expired early %+v", ex)
	}
	if err := s.DeleteDownload(d.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetDownload(d.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if _, err := s.UpdateDownload("zzz", func(*Download) error { return nil }); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestDownloadMeTubeTSAndLookups(t *testing.T) {
	s, _ := testStore(t)
	old, _ := s.CreateDownload(Download{UserID: "A", SharedURL: "https://example.com/v", Folder: "users/a/other",
		MeTubeTS: 1_790_808_248_041_641_193, CreatedAt: time.Unix(100, 0)})
	newer, _ := s.CreateDownload(Download{UserID: "A", SharedURL: "https://example.com/v", Folder: "users/a/other",
		CreatedAt: time.Unix(200, 0)})
	s.CreateDownload(Download{UserID: "B", SharedURL: "https://example.com/v", Folder: "users/b/other"})
	if g, err := s.GetDownload(old.ID); err != nil || g.MeTubeTS != 1_790_808_248_041_641_193 {
		t.Fatalf("round trip: %+v %v", g, err)
	}
	if g, err := s.UserShared("A", "https://example.com/v"); err != nil || g.ID != newer.ID {
		t.Fatalf("UserShared: %+v %v", g, err)
	}
	if _, err := s.UserShared("A", "https://example.com/other"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if ds, err := s.InFolder("users/a/other"); err != nil || len(ds) != 2 || ds[0].ID != old.ID {
		t.Fatalf("InFolder: %+v %v", ds, err)
	}
}

func TestUpdateDownloadAtomic(t *testing.T) {
	s, _ := testStore(t)
	s.UpsertUser("A", "a", false)
	d, _ := s.CreateDownload(Download{UserID: "A", Status: "waiting", Title: "keep"})
	boom := errors.New("boom")
	_, err := s.UpdateDownload(d.ID, func(x *Download) error {
		x.Title = "changed"
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
	g, _ := s.GetDownload(d.ID)
	if g.Title != "keep" {
		t.Fatalf("row changed: %+v", g)
	}
}

func TestTokenRoundTrip(t *testing.T) {
	s, path := testStore(t)
	u, _ := s.UpsertUser("A", "a", false)
	plain, tok, err := s.CreateToken("A", "phone")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(plain, "grab_") || tok.Label != "phone" || tok.UserID != "A" {
		t.Fatalf("%q %+v", plain, tok)
	}
	got, err := s.TokenUser(plain)
	if err != nil || got != u {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err := s.TokenUser("grab_wrong"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	list, _ := s.ListTokens("A")
	if len(list) != 1 || list[0].LastUsed.IsZero() {
		t.Fatalf("list %+v", list)
	}
	s.Close()
	for _, p := range []string{path, path + "-wal"} {
		if b, err := os.ReadFile(p); err == nil && bytes.Contains(b, []byte(plain)) {
			t.Fatalf("%s contains plaintext token", p)
		}
	}
	s2, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	// Another user cannot delete it.
	s2.UpsertUser("B", "b", false)
	s2.DeleteToken("B", tok.ID)
	if _, err := s2.TokenUser(plain); err != nil {
		t.Fatalf("foreign delete worked: %v", err)
	}
	if err := s2.DeleteToken("A", tok.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.TokenUser(plain); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestSessionExpiry(t *testing.T) {
	s, _ := testStore(t)
	u, _ := s.UpsertUser("A", "a", false)
	sid, err := s.CreateSession("A", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.SessionUser(sid); err != nil || got != u {
		t.Fatalf("%+v %v", got, err)
	}
	if err := s.DeleteSession(sid); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SessionUser(sid); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	old, _ := s.CreateSession("A", -time.Second)
	if _, err := s.SessionUser(old); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestNotifyURLsRoundTrip(t *testing.T) {
	s, _ := testStore(t)
	s.UpsertUser("A", "a", false)
	urls := []string{"tgram://x/y", "json://example.com"}
	if err := s.SetNotifyURLs("A", urls); err != nil {
		t.Fatal(err)
	}
	got, err := s.NotifyURLs("A")
	if err != nil || strings.Join(got, ",") != strings.Join(urls, ",") {
		t.Fatalf("%v %v", got, err)
	}
	if err := s.SetNotifyURLs("A", nil); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.NotifyURLs("A"); len(got) != 0 {
		t.Fatalf("not cleared: %v", got)
	}
}

func TestStoreFileMode(t *testing.T) {
	_, path := testStore(t)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
}

func TestCreateDownloadDuplicateIDFails(t *testing.T) {
	s, _ := testStore(t)
	s.UpsertUser("A", "a", false)
	id := "aaaaaaaaaaaaaaaa"
	if _, err := s.CreateDownload(Download{ID: id, UserID: "A", Title: "orig", Status: "finished"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateDownload(Download{ID: id, UserID: "A", Title: "other", Status: "error"}); err == nil {
		t.Fatal("duplicate id accepted")
	}
	g, _ := s.GetDownload(id)
	if g.Title != "orig" || g.Status != "finished" {
		t.Fatalf("row overwritten: %+v", g)
	}
}

func TestDownloadsByFile(t *testing.T) {
	s, _ := testStore(t)
	a, _ := s.CreateDownload(Download{UserID: "A", Folder: "users/a/youtube", Filename: "x.mp4"})
	b, _ := s.CreateDownload(Download{UserID: "A", Folder: "users/a/youtube", Filename: "x.mp4"})
	s.CreateDownload(Download{UserID: "A", Folder: "users/a/youtube", Filename: "y.mp4"})
	s.CreateDownload(Download{UserID: "A", Folder: "public/youtube", Filename: "x.mp4"})
	s.CreateDownload(Download{UserID: "A"}) // no file yet
	got, err := s.DownloadsByFile("users/a/youtube", "x.mp4")
	if err != nil || len(got) != 2 {
		t.Fatalf("got %+v, %v", got, err)
	}
	if ids := map[string]bool{got[0].ID: true, got[1].ID: true}; !ids[a.ID] || !ids[b.ID] {
		t.Fatalf("wrong records %+v", got)
	}
	if got, _ := s.DownloadsByFile("", ""); len(got) != 0 {
		t.Fatalf("empty filename matched %+v", got)
	}
}

// A database made by the first release (schema v1, no metube_ts) must
// upgrade in place with its rows intact.
func TestStoreUpgradesV1Schema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "grab.sqlite3")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	created := time.Unix(1_800_000_000, 0)
	for _, q := range []string{
		migrations[0],
		"PRAGMA user_version = 1",
		"INSERT INTO users(id, dir, name, admin) VALUES('sub-1', 'alex', 'Alex', 1)",
		`INSERT INTO downloads(id, user_id, shared_url, metube_url, site, folder, filename, title, status, created_at)
			VALUES('0123456789abcdef', 'sub-1', 'https://youtu.be/a', 'https://youtu.be/a', 'youtube',
			'users/alex/youtube', 'a.mp4', 'A video', 'finished', ` + strconv.FormatInt(created.UnixNano(), 10) + `)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	db.Close()

	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var v int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil || v != len(migrations) {
		t.Fatalf("user_version %d (%v), want %d", v, err, len(migrations))
	}
	d, err := s.GetDownload("0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if d.UserID != "sub-1" || d.Filename != "a.mp4" || d.Title != "A video" || d.Status != "finished" ||
		!d.CreatedAt.Equal(created) || d.MeTubeTS != 0 {
		t.Fatalf("row changed: %+v", d)
	}
	if u, err := s.UserByID("sub-1"); err != nil || u.Dir != "alex" || !u.Admin {
		t.Fatalf("user: %+v %v", u, err)
	}
	// The new column is writable.
	if _, err := s.UpdateDownload(d.ID, func(x *Download) error { x.MeTubeTS = 42; return nil }); err != nil {
		t.Fatal(err)
	}
	if d, _ := s.GetDownload(d.ID); d.MeTubeTS != 42 {
		t.Fatalf("metube_ts = %d", d.MeTubeTS)
	}
}
