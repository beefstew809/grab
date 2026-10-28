package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func staticFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html": {Data: []byte("<!doctype html><title>Share to Download</title>")},
		"sw.js":      {Data: []byte("self.addEventListener('fetch', () => {});")},
	}
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
	return w
}

func TestStaticIndexServedWithQuery(t *testing.T) {
	a := &App{Static: staticFS()}
	w := get(t, a.Routes(), "/?url=https%3A%2F%2Fyoutu.be%2Fx")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Share to Download") {
		t.Fatalf("got %d %q", w.Code, w.Body.String())
	}
}

func TestHealthzOK(t *testing.T) {
	a := &App{Static: staticFS(), Health: func() map[string]error { return map[string]error{"nfs": nil} }}
	if w := get(t, a.Routes(), "/healthz"); w.Code != 200 {
		t.Fatalf("got %d", w.Code)
	}
}

// /healthz needs no login, so it says only ok/fail per check; the detail
// (which can name paths on the host) goes to the server log.
func TestHealthzFails(t *testing.T) {
	var logs strings.Builder
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	a := &App{Static: staticFS(), Health: func() map[string]error {
		return map[string]error{"nfs": errors.New("stat /shares/.sentinel: no such file"), "vaapi": nil}
	}}
	w := get(t, a.Routes(), "/healthz")
	body, _ := io.ReadAll(w.Body)
	var out map[string]string
	if err := json.Unmarshal(body, &out); err != nil || w.Code != 503 || out["nfs"] != "fail" || out["vaapi"] != "ok" {
		t.Fatalf("got %d %s", w.Code, body)
	}
	if strings.Contains(string(body), "sentinel") {
		t.Fatalf("detail leaked: %s", body)
	}
	if !strings.Contains(logs.String(), "nfs") || !strings.Contains(logs.String(), "/shares/.sentinel") {
		t.Fatalf("detail not logged: %q", logs.String())
	}
}

type fakeProber map[string]Probe

func (f fakeProber) Probe(_ context.Context, path string) (Probe, error) {
	p, ok := f[path]
	if !ok {
		return Probe{}, errors.New("no probe")
	}
	return p, nil
}

type fakeConv struct {
	dir      string
	jobs     map[string]Job
	enqueued int
}

func (f *fakeConv) Enqueue(id string, pr Preset, src string, p Probe, d Decision) Job {
	k := id + "-" + pr.Name
	if j, ok := f.jobs[k]; ok && j.State != JobFailed {
		if _, err := os.Stat(f.CachePath(id, pr.Name)); j.State != JobDone || err == nil {
			return j
		}
	}
	f.enqueued++
	f.jobs[k] = Job{State: JobQueued}
	return f.jobs[k]
}
func (f *fakeConv) Status(id, preset string) (Job, bool) {
	j, ok := f.jobs[id+"-"+preset]
	return j, ok
}
func (f *fakeConv) CachePath(id, preset string) string {
	return filepath.Join(f.dir, id+"-"+preset+".mp4")
}
func (f *fakeConv) Touch(string) {}

func (f *fakeConv) Delete(id string) error {
	for _, pr := range Presets {
		delete(f.jobs, id+"-"+pr.Name)
		for _, suffix := range []string{"", ".tmp"} {
			if err := os.Remove(f.CachePath(id, pr.Name) + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}

type apiEnv struct {
	app   *App
	h     http.Handler
	conv  *fakeConv
	short Item // 60 s 720p h264 mp4, 20 MB
	long  Item // 10 min vp9 webm 1080p
	gone  Item // finished, but the file is missing
}

// newAPIEnv stores three finished downloads for the AUTH=none local user, who
// every un-authenticated test request acts as.
func newAPIEnv(t *testing.T) *apiEnv {
	t.Helper()
	root := t.TempDir()
	folder := "users/local/youtube"
	shortPath := filepath.Join(root, folder, "Short [s].mp4")
	longPath := filepath.Join(root, folder, "Long [l].webm")
	os.MkdirAll(filepath.Dir(shortPath), 0o755)
	os.WriteFile(shortPath, []byte(strings.Repeat("v", 1000)), 0o644)
	os.WriteFile(longPath, []byte("w"), 0o644)
	st, _ := testStore(t)
	auth, err := NewNoAuth(st)
	if err != nil {
		t.Fatal(err)
	}
	base := time.Unix(1_800_000_000, 0)
	add := func(title, file string, size int64, age time.Duration) Item {
		d, err := st.CreateDownload(Download{UserID: localUserID, SharedURL: "https://youtu.be/" + title, Site: "youtube",
			Target: "original", Folder: folder, Filename: file, Title: title, Status: "finished", Size: size,
			CreatedAt: base.Add(-age)})
		if err != nil {
			t.Fatal(err)
		}
		return Item{ID: d.ID}
	}
	conv := &fakeConv{dir: t.TempDir(), jobs: map[string]Job{}}
	a := &App{Static: staticFS(), Store: st, SharesDir: root, Conv: conv, Auth: auth, Prober: fakeProber{
		shortPath: {VCodec: "h264", ACodec: "aac", Container: "mov,mp4,m4a,3gp,3g2,mj2", Height: 720, Size: 20e6, Duration: 60},
		longPath:  {VCodec: "vp9", ACodec: "opus", Container: "matroska,webm", Height: 1080, Size: 300e6, Duration: 600},
	}}
	e := &apiEnv{app: a, h: a.Routes(), conv: conv}
	e.short = add("Short", "Short [s].mp4", 20e6, 0)
	e.long = add("Long", "Long [l].webm", 300e6, time.Minute)
	e.gone = add("Gone", "Gone.mp4", 0, 2*time.Minute)
	return e
}

func do(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(method, path, nil))
	return w
}

func detail(t *testing.T, e *apiEnv, id string) map[string]PresetView {
	t.Helper()
	w := do(t, e.h, "GET", "/api/items/"+id)
	if w.Code != 200 {
		t.Fatalf("detail %d %s", w.Code, w.Body)
	}
	var d struct {
		Duration float64      `json:"duration"`
		Presets  []PresetView `json:"presets"`
	}
	json.Unmarshal(w.Body.Bytes(), &d)
	out := map[string]PresetView{}
	for _, p := range d.Presets {
		out[p.Name] = p
	}
	return out
}

func TestListItems(t *testing.T) {
	e := newAPIEnv(t)
	w := do(t, e.h, "GET", "/api/items?limit=2")
	var items []Item
	json.Unmarshal(w.Body.Bytes(), &items)
	if w.Code != 200 || len(items) != 2 || items[0].Title != "Short" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

func TestDetailPresetStates(t *testing.T) {
	e := newAPIEnv(t)
	s := detail(t, e, e.short.ID)
	for _, name := range []string{"original", "telegram", "signal", "messages"} {
		if s[name].State != "ready" {
			t.Errorf("short %s: %+v", name, s[name])
		}
	}
	if s["discord"].State != "convert" || s["discord"].EstBytes == 0 {
		t.Errorf("short discord: %+v", s["discord"])
	}
	l := detail(t, e, e.long.ID)
	if l["discord"].State != "too_long" || l["discord"].MaxDuration != 181 {
		t.Errorf("long discord: %+v", l["discord"])
	}
	if l["original"].State != "convert" {
		t.Errorf("long original: %+v", l["original"])
	}
	g := detail(t, e, e.gone.ID)
	if g["original"].State != "missing" {
		t.Errorf("gone: %+v", g["original"])
	}
}

func TestDetailUnknown(t *testing.T) {
	if w := do(t, newAPIEnv(t).h, "GET", "/api/items/0000000000000000"); w.Code != 404 {
		t.Fatalf("got %d", w.Code)
	}
}

func TestPrepareQueuesOnce(t *testing.T) {
	e := newAPIEnv(t)
	for i := 0; i < 2; i++ {
		w := do(t, e.h, "POST", "/api/items/"+e.short.ID+"/prepare?preset=discord")
		if w.Code != 202 {
			t.Fatalf("got %d %s", w.Code, w.Body)
		}
	}
	if e.conv.enqueued != 1 {
		t.Fatalf("enqueued %d", e.conv.enqueued)
	}
	if s := detail(t, e, e.short.ID)["discord"]; s.State != "queued" {
		t.Fatalf("got %+v", s)
	}
}

func TestPrepareRejects(t *testing.T) {
	e := newAPIEnv(t)
	if w := do(t, e.h, "POST", "/api/items/"+e.short.ID+"/prepare?preset=myspace"); w.Code != 400 {
		t.Errorf("unknown preset: %d", w.Code)
	}
	if w := do(t, e.h, "POST", "/api/items/"+e.long.ID+"/prepare?preset=discord"); w.Code != 409 {
		t.Errorf("too long: %d", w.Code)
	}
	if w := do(t, e.h, "POST", "/api/items/"+e.gone.ID+"/prepare?preset=original"); w.Code != 409 {
		t.Errorf("missing: %d", w.Code)
	}
}

func TestFileServesReadySourceWithRange(t *testing.T) {
	e := newAPIEnv(t)
	w := do(t, e.h, "GET", "/api/items/"+e.short.ID+"/file?preset=original")
	if w.Code != 200 || w.Header().Get("Content-Type") != "video/mp4" || w.Body.Len() != 1000 {
		t.Fatalf("%d %q %d", w.Code, w.Header().Get("Content-Type"), w.Body.Len())
	}
	req := httptest.NewRequest("GET", "/api/items/"+e.short.ID+"/file?preset=original", nil)
	req.Header.Set("Range", "bytes=0-99")
	rw := httptest.NewRecorder()
	e.h.ServeHTTP(rw, req)
	if rw.Code != 206 || rw.Body.Len() != 100 {
		t.Fatalf("range: %d %d", rw.Code, rw.Body.Len())
	}
}

func TestFileNotReady(t *testing.T) {
	e := newAPIEnv(t)
	if w := do(t, e.h, "GET", "/api/items/"+e.short.ID+"/file?preset=discord"); w.Code != 409 {
		t.Fatalf("got %d", w.Code)
	}
}

func TestFileServesCache(t *testing.T) {
	e := newAPIEnv(t)
	os.WriteFile(e.conv.CachePath(e.long.ID, "discord"), []byte("converted"), 0o644)
	w := do(t, e.h, "GET", "/api/items/"+e.long.ID+"/file?preset=discord")
	body, _ := io.ReadAll(w.Body)
	if w.Code != 200 || string(body) != "converted" {
		t.Fatalf("%d %q", w.Code, body)
	}
}

func TestHealthzIgnoresMeTube(t *testing.T) {
	e := newAPIEnv(t)
	e.app.Health = func() map[string]error { return map[string]error{"nfs": nil, "vaapi": nil} }
	if w := do(t, e.app.Routes(), "GET", "/healthz"); w.Code != 200 {
		t.Fatalf("got %d", w.Code)
	}
}

// Review #2: {id} is URL-decoded, so an encoded slash must not reach the
// cache path and escape it.
func TestRejectsMalformedID(t *testing.T) {
	e := newAPIEnv(t)
	secret := filepath.Join(filepath.Dir(e.conv.dir), "secret-original.mp4")
	os.WriteFile(secret, []byte("SECRET"), 0o644)
	rel := "..%2F" + filepath.Base(filepath.Dir(e.conv.dir)) + "%2Fsecret"
	for _, path := range []string{
		"/api/items/" + rel + "/file?preset=original",
		"/api/items/..%2Fsecret/file?preset=original",
		"/api/items/" + rel,
		"/api/items/" + rel + "/prepare?preset=original",
	} {
		method := "GET"
		if strings.Contains(path, "prepare") {
			method = "POST"
		}
		w := do(t, e.h, method, path)
		if w.Code != 404 || strings.Contains(w.Body.String(), "SECRET") {
			t.Errorf("%s %s: %d %q", method, path, w.Code, w.Body.String())
		}
	}
}

// download=1 asks the browser to save the file (Firefox can't share files,
// and Chrome refuses shares over its size limit), so it needs a real filename.
func TestFileDownloadHasFilename(t *testing.T) {
	e := newAPIEnv(t)
	w := do(t, e.h, "GET", "/api/items/"+e.short.ID+"/file?preset=original&download=1")
	cd := w.Header().Get("Content-Disposition")
	if w.Code != 200 || cd != `attachment; filename="Short.mp4"; filename*=UTF-8''Short.mp4` {
		t.Fatalf("%d %q", w.Code, cd)
	}
	if w := do(t, e.h, "GET", "/api/items/"+e.short.ID+"/file?preset=original"); w.Header().Get("Content-Disposition") != "" {
		t.Fatal("plain fetch (for sharing) must not force a download")
	}
}

func TestDownloadFilename(t *testing.T) {
	for in, want := range map[string]string{
		"He Impressed Frieza...": `attachment; filename="He Impressed Frieza.mp4"; filename*=UTF-8''He%20Impressed%20Frieza.mp4`,
		`a/b\c:"d"`:              `attachment; filename="a b c d.mp4"; filename*=UTF-8''a%20b%20c%20d.mp4`,
		"Insane… #nfl":           `attachment; filename="Insane #nfl.mp4"; filename*=UTF-8''Insane%E2%80%A6%20%23nfl.mp4`,
		"":                       `attachment; filename="video.mp4"; filename*=UTF-8''video.mp4`,
		"a..b":                   `attachment; filename="a.b.mp4"; filename*=UTF-8''a.b.mp4`,
	} {
		if got := contentDisposition(in); got != want {
			t.Errorf("%q:\n got %s\nwant %s", in, got, want)
		}
	}
}

// A CDN caches .js/.css at the edge when the origin says nothing: phones
// kept running a pre-fix app.js for an hour after 1.1.3 shipped (and the edge
// copy skipped the login proxy). Static files must be revalidated every time.
func TestStaticFilesNotCacheable(t *testing.T) {
	a := &App{Static: staticFS()}
	for _, path := range []string{"/", "/sw.js"} {
		if cc := get(t, a.Routes(), path).Header().Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("%s: Cache-Control %q", path, cc)
		}
	}
}

func TestItemNeedsCookies(t *testing.T) {
	u := User{ID: "u1"}
	cases := []struct {
		status, err string
		want        bool
	}{
		{"error", "Sign in to confirm you're not a bot", true},
		{"error", "disk full", false},
		{"finished", "login required", false},
	}
	for _, c := range cases {
		it := ItemFrom(Download{ID: "x", UserID: "u1", Status: c.status, Error: c.err}, t.TempDir(), "", u)
		if it.NeedsCookies != c.want {
			t.Errorf("%s/%q: NeedsCookies=%v, want %v", c.status, c.err, it.NeedsCookies, c.want)
		}
	}
}
