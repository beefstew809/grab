package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type countRefresher struct{ n atomic.Int32 }

func (c *countRefresher) Refresh(context.Context) error { c.n.Add(1); return nil }

type libEnv struct {
	lib    *Library
	st     *Store
	mt     *fakeMeTube
	conv   *fakeConv
	shares string
	alex   User
	bob    User
}

func newLibEnv(t *testing.T) *libEnv {
	t.Helper()
	st, _ := testStore(t)
	alex, err := st.UpsertUser("u-alex", "alex", false)
	if err != nil {
		t.Fatal(err)
	}
	bob, err := st.UpsertUser("u-bob", "bob", false)
	if err != nil {
		t.Fatal(err)
	}
	e := &libEnv{st: st, mt: &fakeMeTube{}, conv: &fakeConv{dir: t.TempDir(), jobs: map[string]Job{}},
		shares: t.TempDir(), alex: alex, bob: bob}
	e.lib = &Library{Store: st, SharesDir: e.shares, Conv: e.conv, RefreshDelay: 10 * time.Millisecond}
	return e
}

func (e *libEnv) write(t *testing.T, rel, body string) string {
	t.Helper()
	p := filepath.Join(e.shares, filepath.FromSlash(rel))
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func (e *libEnv) add(t *testing.T, u User, shared, folder, file string, public bool) Download {
	t.Helper()
	d, err := e.st.CreateDownload(Download{UserID: u.ID, SharedURL: shared, MeTubeURL: shared, Site: "youtube",
		Target: "original", Folder: folder, Filename: file, Status: "finished", Public: public})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func TestSetPublicMovesFile(t *testing.T) {
	e := newLibEnv(t)
	old := e.write(t, "users/alex/youtube/a.mp4", "A")
	d := e.add(t, e.alex, "https://youtu.be/a", "users/alex/youtube", "a.mp4", false)
	got, err := e.lib.SetPublic(context.Background(), d, true)
	if err != nil {
		t.Fatal(err)
	}
	pub := filepath.Join(e.shares, "public/youtube/a.mp4")
	if !exists(pub) || exists(old) || !got.Public || got.Folder != "public/youtube" || got.Filename != "a.mp4" {
		t.Fatalf("public move wrong: %+v", got)
	}
	if s, _ := e.st.GetDownload(d.ID); s.Folder != "public/youtube" || !s.Public {
		t.Fatalf("row not updated: %+v", s)
	}
	if _, err := e.lib.SetPublic(context.Background(), got, false); err != nil {
		t.Fatal(err)
	}
	if !exists(old) || exists(pub) {
		t.Fatal("did not move back")
	}
	if s, _ := e.st.GetDownload(d.ID); s.Folder != "users/alex/youtube" || s.Public {
		t.Fatalf("row not restored: %+v", s)
	}
}

func TestSetPublicNameCollision(t *testing.T) {
	e := newLibEnv(t)
	e.write(t, "users/alex/youtube/a.mp4", "ALEX")
	bobs := e.write(t, "public/youtube/a.mp4", "BOB")
	e.write(t, "public/youtube/a (alex).mp4", "TAKEN")
	d := e.add(t, e.alex, "https://youtu.be/a", "users/alex/youtube", "a.mp4", false)
	got, err := e.lib.SetPublic(context.Background(), d, true)
	if err != nil {
		t.Fatal(err)
	}
	if got.Filename != "a (alex 2).mp4" {
		t.Fatalf("filename %q", got.Filename)
	}
	b, _ := os.ReadFile(bobs)
	mine, _ := os.ReadFile(filepath.Join(e.shares, "public/youtube/a (alex 2).mp4"))
	if !bytes.Equal(b, []byte("BOB")) || !bytes.Equal(mine, []byte("ALEX")) {
		t.Fatalf("bob %q mine %q", b, mine)
	}
	// First free suffix is plain "(alex)".
	e.write(t, "users/alex/youtube/c.mp4", "C")
	e.write(t, "public/youtube/c.mp4", "BOBC")
	d2 := e.add(t, e.alex, "https://youtu.be/c", "users/alex/youtube", "c.mp4", false)
	if g, _ := e.lib.SetPublic(context.Background(), d2, true); g.Filename != "c (alex).mp4" {
		t.Fatalf("filename %q", g.Filename)
	}
}

func TestSetPublicNotFinished(t *testing.T) {
	e := newLibEnv(t)
	e.write(t, "users/alex/youtube/a.mp4", "A")
	d := e.add(t, e.alex, "https://youtu.be/a", "users/alex/youtube", "a.mp4", false)
	d, _ = e.st.UpdateDownload(d.ID, func(x *Download) error { x.Status = "downloading"; return nil })
	if _, err := e.lib.SetPublic(context.Background(), d, true); err != ErrNotFinished {
		t.Fatalf("want ErrNotFinished, got %v", err)
	}
	// finished but file missing
	d2 := e.add(t, e.alex, "https://youtu.be/m", "users/alex/youtube", "missing.mp4", false)
	if _, err := e.lib.SetPublic(context.Background(), d2, true); err != ErrNotFinished {
		t.Fatalf("want ErrNotFinished, got %v", err)
	}
	h := e.handler(t, e.alex)
	if w := doJSON(h, "POST", "/api/items/"+d.ID+"/public", `{"public":true}`); w.Code != 409 {
		t.Fatalf("api %d %s", w.Code, w.Body)
	}
}

func TestSetPublicRefreshesJellyfin(t *testing.T) {
	e := newLibEnv(t)
	r := &countRefresher{}
	e.lib.Jellyfin = r
	e.write(t, "users/alex/youtube/a.mp4", "A")
	e.write(t, "users/alex/youtube/b.mp4", "B")
	a := e.add(t, e.alex, "https://youtu.be/a", "users/alex/youtube", "a.mp4", false)
	b := e.add(t, e.alex, "https://youtu.be/b", "users/alex/youtube", "b.mp4", false)
	e.lib.SetPublic(context.Background(), a, true)
	e.lib.SetPublic(context.Background(), b, true) // inside the window: coalesced
	deadline := time.Now().Add(2 * time.Second)
	for r.n.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(60 * time.Millisecond)
	if n := r.n.Load(); n != 1 {
		t.Fatalf("refresh calls %d, want 1", n)
	}
}

func TestJellyfinRequest(t *testing.T) {
	var path, auth, method string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, auth, method = r.URL.Path, r.Header.Get("Authorization"), r.Method
		w.WriteHeader(204)
	}))
	defer srv.Close()
	j := &Jellyfin{URL: srv.URL + "/", APIKey: "k3y", LibraryID: "lib1"}
	if err := j.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if method != "POST" || path != "/Items/lib1/Refresh" || auth != `MediaBrowser Token="k3y"` {
		t.Fatalf("%s %s %s", method, path, auth)
	}
	j.LibraryID = ""
	if err := j.Refresh(context.Background()); err != nil || path != "/Library/Refresh" {
		t.Fatalf("path %s err %v", path, err)
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) }))
	defer bad.Close()
	err := (&Jellyfin{URL: bad.URL, APIKey: "k3y"}).Refresh(context.Background())
	if err == nil || bytes.Contains([]byte(err.Error()), []byte("k3y")) {
		t.Fatalf("err %v", err)
	}
}

func TestDeleteRemovesFileCacheAndRow(t *testing.T) {
	e := newLibEnv(t)
	f := e.write(t, "users/alex/youtube/a.mp4", "A")
	d := e.add(t, e.alex, "https://youtu.be/a", "users/alex/youtube", "a.mp4", false)
	var caches []string
	for _, pr := range Presets {
		p := e.conv.CachePath(d.ID, pr.Name)
		os.WriteFile(p, []byte("c"), 0o644)
		caches = append(caches, p)
	}
	if err := e.lib.Delete(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if exists(f) {
		t.Fatal("file kept")
	}
	for _, p := range caches {
		if exists(p) {
			t.Fatalf("cache kept %s", p)
		}
	}
	if _, err := e.st.GetDownload(d.ID); err != ErrNotFound {
		t.Fatalf("row kept: %v", err)
	}
}

func TestDeleteKeepsFileSharedByAnotherRecord(t *testing.T) {
	e := newLibEnv(t)
	f := e.write(t, "users/alex/youtube/a.mp4", "A")
	d1 := e.add(t, e.alex, "https://youtu.be/a", "users/alex/youtube", "a.mp4", false)
	d2 := e.add(t, e.alex, "https://www.youtube.com/watch?v=a", "users/alex/youtube", "a.mp4", false)
	cache := e.conv.CachePath(d1.ID, "telegram")
	os.WriteFile(cache, []byte("c"), 0o644)
	if err := e.lib.Delete(context.Background(), d1); err != nil {
		t.Fatal(err)
	}
	if !exists(f) || exists(cache) || len(e.mt.deletes) != 0 {
		t.Fatalf("file %v cache %v deletes %v", exists(f), exists(cache), e.mt.deletes)
	}
	if _, err := e.st.GetDownload(d1.ID); err != ErrNotFound {
		t.Fatal("row kept")
	}
	if err := e.lib.Delete(context.Background(), d2); err != nil {
		t.Fatal(err)
	}
	if exists(f) || len(e.mt.deletes) != 0 {
		t.Fatalf("last record should remove the file; file %v deletes %v", exists(f), e.mt.deletes)
	}
}

func TestSetPublicMovesAllRecordsSharingTheFile(t *testing.T) {
	e := newLibEnv(t)
	e.write(t, "users/alex/youtube/a.mp4", "A")
	d1 := e.add(t, e.alex, "https://youtu.be/a", "users/alex/youtube", "a.mp4", false)
	d2 := e.add(t, e.alex, "https://www.youtube.com/watch?v=a", "users/alex/youtube", "a.mp4", false)
	if _, err := e.lib.SetPublic(context.Background(), d1, true); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{d1.ID, d2.ID} {
		s, _ := e.st.GetDownload(id)
		if s.Folder != "public/youtube" || s.Filename != "a.mp4" || !s.Public {
			t.Fatalf("%s not followed: %+v", id, s)
		}
		if _, ok := resolvePath(e.shares, s.Folder, s.Filename); !ok || !exists(filepath.Join(e.shares, s.Folder, s.Filename)) {
			t.Fatalf("%s points at nothing", id)
		}
	}
}

// fixedAuth makes every request come from one user.
type fixedAuth struct{ u User }

func (f fixedAuth) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey{}, f.u)))
	})
}
func (fixedAuth) Register(*http.ServeMux) {}

func (e *libEnv) handler(t *testing.T, u User) http.Handler {
	t.Helper()
	return (&App{Static: staticFS(), Store: e.st, SharesDir: e.shares, Conv: e.conv, Library: e.lib, Auth: fixedAuth{u}}).Routes()
}

func doJSON(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	h.ServeHTTP(w, r)
	return w
}

func TestDeletePermissions(t *testing.T) {
	e := newLibEnv(t)
	e.write(t, "public/youtube/pub.mp4", "P")
	e.write(t, "users/alex/youtube/priv.mp4", "V")
	pub := e.add(t, e.alex, "https://youtu.be/p", "public/youtube", "pub.mp4", true)
	priv := e.add(t, e.alex, "https://youtu.be/v", "users/alex/youtube", "priv.mp4", false)
	hb := e.handler(t, e.bob)
	if w := doJSON(hb, "DELETE", "/api/items/"+pub.ID, ""); w.Code != 403 {
		t.Fatalf("public: %d", w.Code)
	}
	if w := doJSON(hb, "POST", "/api/items/"+pub.ID+"/public", `{"public":false}`); w.Code != 403 {
		t.Fatalf("public toggle: %d", w.Code)
	}
	if w := doJSON(hb, "DELETE", "/api/items/"+priv.ID, ""); w.Code != 404 {
		t.Fatalf("private: %d", w.Code)
	}
	if !exists(filepath.Join(e.shares, "public/youtube/pub.mp4")) {
		t.Fatal("forbidden delete removed the file")
	}
	admin := User{ID: "adm", Dir: "adm", Name: "adm", Admin: true}
	if w := doJSON(e.handler(t, admin), "DELETE", "/api/items/"+priv.ID, ""); w.Code != 204 {
		t.Fatalf("admin: %d", w.Code)
	}
	if w := doJSON(e.handler(t, e.alex), "DELETE", "/api/items/"+pub.ID, ""); w.Code != 204 {
		t.Fatalf("owner: %d", w.Code)
	}
}

func TestSweepOnlyPrivateAndOld(t *testing.T) {
	e := newLibEnv(t)
	now := time.Now()
	mk := func(name string, public bool, age time.Duration) Download {
		folder := "users/alex/youtube"
		if public {
			folder = "public/youtube"
		}
		e.write(t, folder+"/"+name, name)
		d, err := e.st.CreateDownload(Download{UserID: e.alex.ID, SharedURL: "https://youtu.be/" + name, Site: "youtube",
			Folder: folder, Filename: name, Status: "finished", Public: public, CreatedAt: now.Add(-age)})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	oldPriv := mk("old-priv.mp4", false, 40*24*time.Hour)
	oldPub := mk("old-pub.mp4", true, 40*24*time.Hour)
	newPriv := mk("new-priv.mp4", false, time.Hour)
	if n, err := e.lib.Sweep(context.Background(), 0, now); n != 0 || err != nil {
		t.Fatalf("maxAge 0: %d %v", n, err)
	}
	if _, err := e.st.GetDownload(oldPriv.ID); err != nil {
		t.Fatal("maxAge 0 deleted something")
	}
	n, err := e.lib.Sweep(context.Background(), 30*24*time.Hour, now)
	if n != 1 || err != nil {
		t.Fatalf("sweep %d %v", n, err)
	}
	if _, err := e.st.GetDownload(oldPriv.ID); err != ErrNotFound {
		t.Fatal("old private kept")
	}
	for _, d := range []Download{oldPub, newPriv} {
		if _, err := e.st.GetDownload(d.ID); err != nil || !exists(filepath.Join(e.shares, d.Folder, d.Filename)) {
			t.Fatalf("%s wrongly swept", d.Filename)
		}
	}
}

func TestSweepSkipsItemMadePublicMeanwhile(t *testing.T) {
	e := newLibEnv(t)
	now := time.Now()
	e.write(t, "users/alex/youtube/a.mp4", "A")
	d, _ := e.st.CreateDownload(Download{UserID: e.alex.ID, SharedURL: "https://youtu.be/a", Site: "youtube",
		Folder: "users/alex/youtube", Filename: "a.mp4", Status: "finished", CreatedAt: now.Add(-40 * 24 * time.Hour)})
	cutoff := now.Add(-30 * 24 * time.Hour)
	stale, _ := e.st.ExpiredPrivate(cutoff)
	if len(stale) != 1 {
		t.Fatalf("setup: %d", len(stale))
	}
	if _, err := e.lib.SetPublic(context.Background(), d, true); err != nil {
		t.Fatal(err)
	}
	if removed, err := e.lib.remove(context.Background(), stale[0].ID, cutoff); removed || err != nil {
		t.Fatalf("removed=%v err=%v", removed, err)
	}
	if _, err := e.st.GetDownload(d.ID); err != nil || !exists(filepath.Join(e.shares, "public/youtube/a.mp4")) {
		t.Fatal("public item was swept")
	}
}

func TestDeleteWithStaleRecordRemovesMovedFile(t *testing.T) {
	e := newLibEnv(t)
	e.write(t, "users/alex/youtube/a.mp4", "A")
	d := e.add(t, e.alex, "https://youtu.be/a", "users/alex/youtube", "a.mp4", false)
	if _, err := e.lib.SetPublic(context.Background(), d, true); err != nil {
		t.Fatal(err)
	}
	if err := e.lib.Delete(context.Background(), d); err != nil { // d still says private
		t.Fatal(err)
	}
	if exists(filepath.Join(e.shares, "public/youtube/a.mp4")) {
		t.Fatal("file orphaned in public/")
	}
	if err := e.lib.Delete(context.Background(), d); err != nil { // already gone: no-op
		t.Fatal(err)
	}
}

// Deleting a download MeTube is still working on would leave its file to
// land later with no record (and the tracker would find nothing to update).
func TestDeleteInFlightRefused(t *testing.T) {
	e := newLibEnv(t)
	h := e.handler(t, e.alex)
	for _, status := range []string{"waiting", "queued", "downloading"} {
		d, err := e.st.CreateDownload(Download{UserID: e.alex.ID, SharedURL: "https://youtu.be/" + status, Site: "youtube",
			Target: "original", Folder: "users/alex/youtube", Status: status})
		if err != nil {
			t.Fatal(err)
		}
		w := doJSON(h, "DELETE", "/api/items/"+d.ID, "")
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), `"error":"still downloading"`) {
			t.Errorf("%s: %d %s", status, w.Code, w.Body)
		}
		if _, err := e.st.GetDownload(d.ID); err != nil {
			t.Errorf("%s: record gone: %v", status, err)
		}
	}
	failed, _ := e.st.CreateDownload(Download{UserID: e.alex.ID, SharedURL: "https://youtu.be/x", Site: "youtube",
		Target: "original", Folder: "users/alex/youtube", Status: "error"})
	if w := doJSON(h, "DELETE", "/api/items/"+failed.ID, ""); w.Code != http.StatusNoContent {
		t.Errorf("error record: %d %s", w.Code, w.Body)
	}
}
