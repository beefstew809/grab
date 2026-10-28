package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeMeTube is an in-memory MeTubeClient: a settable history, a record of
// every Add, and error toggles for both. With simulate on, Add behaves like
// MeTube: it keys the entry by rewrite(url) (yt-dlp's webpage_url), refuses
// a key already queued with ErrAlreadyQueued, and queues it stamped now().
type fakeMeTube struct {
	mu      sync.Mutex
	h       History
	histErr error
	addErr  error
	adds    []AddRequest // successful Adds
	calls   int          // every Add, failed ones included
	deletes [][]string

	simulate bool
	rewrite  func(string) string
	now      func() time.Time
}

func (f *fakeMeTube) History(context.Context) (History, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.h, f.histErr
}

func (f *fakeMeTube) Add(_ context.Context, r AddRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.addErr != nil {
		return f.addErr
	}
	if f.simulate {
		key := r.URL
		if f.rewrite != nil {
			key = f.rewrite(key)
		}
		for _, e := range append(f.h.Queue, f.h.Pending...) {
			if e.URL == key {
				return ErrAlreadyQueued
			}
		}
		f.h.Queue = append(f.h.Queue, HistoryItem{Title: "A video", URL: key, Folder: r.Folder,
			Timestamp: f.now().UnixNano()})
	}
	f.adds = append(f.adds, r)
	return nil
}

func (f *fakeMeTube) Delete(_ context.Context, urls []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes = append(f.deletes, urls)
	// MeTube's done history is keyed by URL, even across users/folders.
	for _, u := range urls {
		for i := 0; i < len(f.h.Done); {
			if f.h.Done[i].URL == u {
				f.h.Done = append(f.h.Done[:i], f.h.Done[i+1:]...)
			} else {
				i++
			}
		}
	}
	return nil
}

// setDone replaces MeTube's done list. Entries without a timestamp get
// now(): MeTube always stamps its entries.
func (f *fakeMeTube) setDone(items ...HistoryItem) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range items {
		if items[i].Timestamp == 0 {
			items[i].Timestamp = f.now().UnixNano()
		}
	}
	f.h.Done = items
}

// finish moves MeTube's queued entry for key to done as a finished file;
// done is keyed by URL, so it replaces any earlier entry for that key.
func (f *fakeMeTube) finish(key, file string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var q []HistoryItem
	for _, e := range f.h.Queue {
		if e.URL != key {
			q = append(q, e)
			continue
		}
		e.Status, e.Filename, e.Size = "finished", file, 1234
		var d []HistoryItem
		for _, x := range f.h.Done {
			if x.URL != key {
				d = append(d, x)
			}
		}
		f.h.Done = append(d, e)
	}
	f.h.Queue = q
}

// youtubeCanon is what yt-dlp reports as webpage_url for youtu.be links.
func youtubeCanon(u string) string {
	if id, ok := strings.CutPrefix(u, "https://youtu.be/"); ok {
		id, _, _ = strings.Cut(id, "?")
		return "https://www.youtube.com/watch?v=" + id
	}
	return u
}

type grabEnv struct {
	st      *Store
	mt      *fakeMeTube
	conv    *fakeConv
	shares  string
	tr      *Tracker
	h       http.Handler
	now     time.Time
	nudges  int
	cookies *CookieStore
}

func newGrabEnv(t *testing.T) *grabEnv {
	t.Helper()
	st, _ := testStore(t)
	auth, err := NewNoAuth(st)
	if err != nil {
		t.Fatal(err)
	}
	e := &grabEnv{st: st, mt: &fakeMeTube{}, conv: &fakeConv{dir: t.TempDir(), jobs: map[string]Job{}},
		shares: t.TempDir(), now: time.Unix(1_800_000_000, 0)}
	e.mt.now = func() time.Time { return e.now }
	prober := NewFFprobe("ffprobe")
	e.tr = &Tracker{Store: st, MT: e.mt, SharesDir: e.shares, Conv: e.conv, Prober: prober,
		Now: func() time.Time { return e.now }}
	e.cookies = &CookieStore{Dir: t.TempDir()}
	e.tr.Cookies = e.cookies
	a := &App{Cookies: e.cookies, Static: staticFS(), Store: st, SharesDir: e.shares, Conv: e.conv, Prober: prober, Auth: auth,
		Nudge: func() { e.nudges++ }}
	e.h = a.Routes()
	return e
}

// user creates a user and an API token for acting as them.
func (e *grabEnv) user(t *testing.T, name string, admin bool) (User, string) {
	t.Helper()
	u, err := e.st.UpsertUser("sub-"+name, name, admin)
	if err != nil {
		t.Fatal(err)
	}
	tok, _, err := e.st.CreateToken(u.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	return u, tok
}

func (e *grabEnv) req(t *testing.T, tok, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+tok)
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

// submit posts a share and returns the new download's id.
func (e *grabEnv) submit(t *testing.T, tok, body string) string {
	t.Helper()
	w := e.req(t, tok, "POST", "/api/downloads", body)
	if w.Code != http.StatusCreated {
		t.Fatalf("submit %s: %d %s", body, w.Code, w.Body)
	}
	var out struct{ ID string }
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.ID == "" {
		t.Fatalf("submit: %s", w.Body)
	}
	return out.ID
}

func (e *grabEnv) tick(t *testing.T) {
	t.Helper()
	if err := e.tr.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
}

func (e *grabEnv) get(t *testing.T, id string) Download {
	t.Helper()
	d, err := e.st.GetDownload(id)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func done(url, folder, file string) HistoryItem {
	return HistoryItem{Title: "A video", URL: url, Folder: folder, Filename: file, Status: "finished", Size: 1234}
}

func TestSubmitCreatesWaitingDownload(t *testing.T) {
	e := newGrabEnv(t)
	alex, tok := e.user(t, "alex", false)
	id := e.submit(t, tok, `{"text":"see https://youtu.be/abc"}`)
	d := e.get(t, id)
	if d.UserID != alex.ID || d.SharedURL != "https://youtu.be/abc" || d.MeTubeURL != "https://youtu.be/abc" ||
		d.Site != "youtube" || d.Folder != "users/alex/youtube" || d.Status != "waiting" || d.Target != alex.DefaultTarget {
		t.Fatalf("got %+v", d)
	}
	if len(id) != 16 || strings.Trim(id, "0123456789abcdef") != "" {
		t.Fatalf("id %q is not 16 hex chars", id)
	}
	if e.nudges != 1 {
		t.Fatalf("tracker nudged %d times, want 1", e.nudges)
	}
}

func TestSubmitUnknownTarget(t *testing.T) {
	e := newGrabEnv(t)
	_, tok := e.user(t, "alex", false)
	w := e.req(t, tok, "POST", "/api/downloads", `{"url":"https://youtu.be/a","target":"myspace"}`)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "unknown target") {
		t.Fatalf("got %d %s", w.Code, w.Body)
	}
	if w := e.req(t, tok, "POST", "/api/downloads", `{"text":"no link here"}`); w.Code != 400 {
		t.Fatalf("no link: got %d", w.Code)
	}
	if ds, _ := e.st.ListDownloads(ListFilter{Admin: true}); len(ds) != 0 {
		t.Fatalf("rejected submissions were stored: %+v", ds)
	}
}

func TestTrackerSubmitsAndFinishes(t *testing.T) {
	e := newGrabEnv(t)
	_, tok := e.user(t, "alex", false)
	id := e.submit(t, tok, `{"url":"https://youtu.be/abc"}`)
	e.tick(t)
	if len(e.mt.adds) != 1 || e.mt.adds[0] != (AddRequest{URL: "https://youtu.be/abc", Folder: "users/alex/youtube"}) {
		t.Fatalf("adds %+v", e.mt.adds)
	}
	if d := e.get(t, id); d.Status != "queued" || !d.QueuedAt.Equal(e.now) {
		t.Fatalf("after add: %+v", d)
	}
	writeFile(t, filepath.Join(e.shares, "users", "alex", "youtube", "a [abc].mp4"))
	e.mt.setDone(done("https://youtu.be/abc", "users/alex/youtube", "a [abc].mp4"))
	e.tick(t)
	d := e.get(t, id)
	if d.Status != "finished" || d.Filename != "a [abc].mp4" || d.Size != 1234 || d.Title != "A video" || d.FinishedAt.IsZero() {
		t.Fatalf("after finish: %+v", d)
	}
	if len(e.mt.adds) != 1 {
		t.Fatalf("re-submitted: %+v", e.mt.adds)
	}
}

func TestTrackerProgress(t *testing.T) {
	e := newGrabEnv(t)
	_, tok := e.user(t, "alex", false)
	id := e.submit(t, tok, `{"url":"https://youtu.be/abc"}`)
	e.tick(t)
	e.mt.h.Queue = []HistoryItem{{Title: "Live", URL: "https://youtu.be/abc", Folder: "users/alex/youtube", Percent: 42,
		Timestamp: e.now.UnixNano()}}
	e.tick(t)
	if d := e.get(t, id); d.Status != "downloading" || d.Percent != 42 || d.Title != "Live" {
		t.Fatalf("got %+v", d)
	}
}

// Review focus 2: MeTube keys history by URL, so a second user's share of the
// same link waits until the first is finished, then lands in its own folder.
func TestTrackerSingleFlightPerURL(t *testing.T) {
	e := newGrabEnv(t)
	_, ta := e.user(t, "a", false)
	_, tb := e.user(t, "b", false)
	ida := e.submit(t, ta, `{"url":"https://youtu.be/same"}`)
	idb := e.submit(t, tb, `{"url":"https://youtu.be/same"}`)
	e.tick(t)
	if len(e.mt.adds) != 1 || e.mt.adds[0].Folder != "users/a/youtube" {
		t.Fatalf("adds %+v", e.mt.adds)
	}
	if d := e.get(t, idb); d.Status != "waiting" {
		t.Fatalf("b: %+v", d)
	}
	// A finishes in MeTube. Submit runs before sync in a tick, so this tick
	// only records A as finished; B must still not be sent while A was in flight.
	e.mt.setDone(done("https://youtu.be/same", "users/a/youtube", "s.mp4"))
	e.tick(t)
	if d := e.get(t, ida); d.Status != "finished" {
		t.Fatalf("a: %+v", d)
	}
	if len(e.mt.adds) != 1 {
		t.Fatalf("b sent while a was in flight: %+v", e.mt.adds)
	}
	e.tick(t)
	if len(e.mt.adds) != 2 || e.mt.adds[1] != (AddRequest{URL: "https://youtu.be/same", Folder: "users/b/youtube"}) {
		t.Fatalf("adds %+v", e.mt.adds)
	}
	if d := e.get(t, idb); d.Status != "queued" {
		t.Fatalf("b: %+v", d)
	}
	// A's done entry is in another folder: it must not finish B.
	e.tick(t)
	if d := e.get(t, idb); d.Status != "queued" {
		t.Fatalf("b matched a's history entry: %+v", d)
	}
}

// Review focus 3: shares made while MeTube is down are kept and sent later.
func TestSubmitWhileMeTubeDownIsQueued(t *testing.T) {
	e := newGrabEnv(t)
	_, tok := e.user(t, "alex", false)
	e.mt.addErr = errors.New("dial tcp: connection refused")
	id := e.submit(t, tok, `{"url":"https://youtu.be/abc"}`)
	e.tr.Tick(context.Background())
	if d := e.get(t, id); d.Status != "waiting" || d.Error != "" {
		t.Fatalf("while down: %+v", d)
	}
	e.mt.addErr = nil
	e.tick(t)
	if d := e.get(t, id); d.Status != "queued" || len(e.mt.adds) != 1 {
		t.Fatalf("after recovery: %+v %+v", d, e.mt.adds)
	}
}

func TestTrackerAddRejectedMarksError(t *testing.T) {
	e := newGrabEnv(t)
	_, tok := e.user(t, "alex", false)
	e.mt.addErr = &MeTubeError{Msg: "Unsupported URL"}
	id := e.submit(t, tok, `{"url":"https://example.com/x"}`)
	e.tick(t)
	if d := e.get(t, id); d.Status != "error" || d.Error != "Unsupported URL" {
		t.Fatalf("got %+v", d)
	}
}

func TestTrackerMeTubeErrorMarksError(t *testing.T) {
	e := newGrabEnv(t)
	_, tok := e.user(t, "alex", false)
	id := e.submit(t, tok, `{"url":"https://youtu.be/abc"}`)
	e.tick(t)
	e.mt.setDone(HistoryItem{URL: "https://youtu.be/abc", Folder: "users/alex/youtube", Status: "error",
		Msg: "ERROR: Sign in to confirm you're not a bot"})
	e.tick(t)
	if d := e.get(t, id); d.Status != "error" || d.Error != "ERROR: Sign in to confirm you're not a bot" {
		t.Fatalf("got %+v", d)
	}
}

func TestTrackerLostDownload(t *testing.T) {
	e := newGrabEnv(t)
	_, tok := e.user(t, "alex", false)
	id := e.submit(t, tok, `{"url":"https://youtu.be/abc"}`)
	e.tick(t)
	e.now = e.now.Add(9 * time.Minute)
	e.tick(t)
	if d := e.get(t, id); d.Status != "queued" {
		t.Fatalf("lost too early: %+v", d)
	}
	e.now = e.now.Add(2 * time.Minute)
	e.tick(t)
	if d := e.get(t, id); d.Status != "error" || d.Error != "the downloader lost this download" {
		t.Fatalf("got %+v", d)
	}
}

// A History failure says nothing about the downloads: none may be declared lost.
func TestTrackerHistoryDownKeepsState(t *testing.T) {
	e := newGrabEnv(t)
	_, tok := e.user(t, "alex", false)
	id := e.submit(t, tok, `{"url":"https://youtu.be/abc"}`)
	e.tick(t)
	e.mt.histErr = errors.New("connection refused")
	e.now = e.now.Add(time.Hour)
	if err := e.tr.Tick(context.Background()); err == nil {
		t.Fatal("want the history error reported")
	}
	if d := e.get(t, id); d.Status != "queued" {
		t.Fatalf("got %+v", d)
	}
}

func TestTrackerAutoConvertsTarget(t *testing.T) {
	src := fixture(t, "h264.mp4")
	e := newGrabEnv(t)
	_, tok := e.user(t, "alex", false)
	id := e.submit(t, tok, `{"url":"https://youtu.be/abc","target":"discord"}`)
	e.tick(t)
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(e.shares, "users", "alex", "youtube", "v.mp4")
	os.MkdirAll(filepath.Dir(dst), 0o755)
	if err := os.WriteFile(dst, b, 0o644); err != nil {
		t.Fatal(err)
	}
	e.mt.setDone(done("https://youtu.be/abc", "users/alex/youtube", "v.mp4"))
	e.tick(t)
	if _, ok := e.conv.Status(id, "discord"); !ok || e.conv.enqueued != 1 {
		t.Fatalf("no discord conversion: %+v", e.conv.jobs)
	}
	if d := e.get(t, id); !d.NotifiedAt.IsZero() {
		t.Fatal("notified before the target conversion finished")
	}
	e.tick(t)
	if e.conv.enqueued != 1 {
		t.Fatalf("enqueued %d times", e.conv.enqueued)
	}
	e.conv.jobs[id+"-discord"] = Job{State: JobFailed, Error: "boom"}
	e.tick(t)
	if d := e.get(t, id); d.NotifiedAt.IsZero() {
		t.Fatal("a failed conversion must still notify")
	}
}

func TestTrackerNotify(t *testing.T) {
	e := newGrabEnv(t)
	_, tok := e.user(t, "alex", false)
	var sent []string
	fail := true
	e.tr.Notify = func(_ context.Context, d Download, _ string) error {
		if fail {
			return errors.New("apprise down")
		}
		sent = append(sent, d.ID)
		return nil
	}
	// Three sites, so three folders: one download per folder is in flight.
	ok := e.submit(t, tok, `{"url":"https://youtu.be/ok"}`)
	late := e.submit(t, tok, `{"url":"https://www.reddit.com/r/x/comments/late"}`)
	bad := e.submit(t, tok, `{"url":"https://www.tiktok.com/@x/video/1"}`)
	e.tick(t)
	writeFile(t, filepath.Join(e.shares, "users", "alex", "youtube", "ok.mp4"))
	e.mt.setDone(done("https://youtu.be/ok", "users/alex/youtube", "ok.mp4"),
		done("https://www.reddit.com/r/x/comments/late", "users/alex/reddit", "late.mp4"), // file not visible yet
		HistoryItem{URL: "https://www.tiktok.com/@x/video/1", Folder: "users/alex/tiktok", Status: "error", Msg: "nope"})
	if err := e.tr.Tick(context.Background()); err == nil {
		t.Fatal("want the Notify failures reported")
	}
	if d := e.get(t, ok); !d.NotifiedAt.IsZero() {
		t.Fatal("marked notified although Notify failed")
	}
	fail = false
	e.now = e.now.Add(time.Minute) // past the first retry backoff
	e.tick(t)
	e.tick(t)
	if len(sent) != 2 || sent[0] != ok || sent[1] != bad {
		t.Fatalf("sent %v, want [ok bad] once each", sent)
	}
	if d := e.get(t, late); !d.NotifiedAt.IsZero() {
		t.Fatal("notified before the file exists")
	}
	writeFile(t, filepath.Join(e.shares, "users", "alex", "reddit", "late.mp4"))
	e.tick(t)
	if len(sent) != 3 || sent[2] != late {
		t.Fatalf("sent %v", sent)
	}
}

// finishedDue makes one finished, due download for alex and returns its id.
func finishedDue(t *testing.T, e *grabEnv) string {
	t.Helper()
	_, tok := e.user(t, "alex", false)
	id := e.submit(t, tok, `{"url":"https://youtu.be/abc"}`)
	e.tick(t)
	writeFile(t, filepath.Join(e.shares, "users", "alex", "youtube", "a.mp4"))
	e.mt.setDone(done("https://youtu.be/abc", "users/alex/youtube", "a.mp4"))
	return id
}

// A failing Notify (say, a typo'd Apprise URL) must not run apprise every
// tick: each attempt can take 30 s inside the one tracker goroutine.
func TestTrackerNotifyBacksOff(t *testing.T) {
	e := newGrabEnv(t)
	calls := 0
	e.tr.Notify = func(context.Context, Download, string) error { calls++; return errors.New("apprise exited 1") }
	finishedDue(t, e)
	for _, step := range []struct {
		advance time.Duration
		want    int
	}{
		{0, 1},                // first attempt
		{0, 1},                // same tick time: held back
		{59 * time.Second, 1}, // still inside the 1 min wait
		{time.Second, 2},      // 1 min after the first failure
		{time.Minute, 2},      // the wait doubled to 2 min
		{time.Minute, 3},
		{3 * time.Minute, 3}, // then 4 min
		{time.Minute, 4},
	} {
		e.now = e.now.Add(step.advance)
		e.tr.Tick(context.Background())
		if calls != step.want {
			t.Fatalf("after +%v: %d attempts, want %d", step.advance, calls, step.want)
		}
	}
	// The wait stops growing at an hour.
	for range 10 {
		e.now = e.now.Add(time.Hour)
		e.tr.Tick(context.Background())
	}
	if calls != 14 {
		t.Fatalf("hourly ticks: %d attempts, want 14", calls)
	}
}

func TestTrackerNotifyGivesUpAfterADay(t *testing.T) {
	var logs strings.Builder
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	e := newGrabEnv(t)
	calls := 0
	e.tr.Notify = func(context.Context, Download, string) error { calls++; return errors.New("apprise exited 1") }
	id := finishedDue(t, e)
	e.tr.Tick(context.Background())
	for range 23 {
		e.now = e.now.Add(time.Hour)
		e.tr.Tick(context.Background())
	}
	if d := e.get(t, id); !d.NotifiedAt.IsZero() {
		t.Fatal("gave up before 24 h")
	}
	e.now = e.now.Add(time.Hour)
	e.tr.Tick(context.Background())
	if d := e.get(t, id); d.NotifiedAt.IsZero() {
		t.Fatal("still not marked notified after 24 h of failures")
	}
	n := calls
	e.now = e.now.Add(2 * time.Hour)
	e.tick(t) // nothing left to notify: no error
	if calls != n {
		t.Fatal("tried again after giving up")
	}
	if c := strings.Count(logs.String(), "giving up"); c != 1 {
		t.Fatalf("logged giving up %d times: %q", c, logs.String())
	}
}

func TestTrackerNotifyOffMarksNotified(t *testing.T) {
	e := newGrabEnv(t)
	_, tok := e.user(t, "alex", false)
	id := e.submit(t, tok, `{"url":"https://youtu.be/abc"}`)
	e.tick(t)
	writeFile(t, filepath.Join(e.shares, "users", "alex", "youtube", "a.mp4"))
	e.mt.setDone(done("https://youtu.be/abc", "users/alex/youtube", "a.mp4"))
	e.tick(t)
	if d := e.get(t, id); d.NotifiedAt.IsZero() {
		t.Fatal("nil Notify must mark due downloads notified")
	}
}

func TestTrackerRunTicksOnNudge(t *testing.T) {
	e := newGrabEnv(t)
	alex, _ := e.user(t, "alex", false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go e.tr.Run(ctx, time.Hour)
	time.Sleep(50 * time.Millisecond) // let the start-up tick pass
	d, err := e.st.CreateDownload(Download{UserID: alex.ID, SharedURL: "https://youtu.be/abc",
		MeTubeURL: "https://youtu.be/abc", Site: "youtube", Folder: "users/alex/youtube", Status: "waiting"})
	if err != nil {
		t.Fatal(err)
	}
	id := d.ID
	e.tr.Nudge()
	e.tr.Nudge() // never blocks, even with a nudge pending
	deadline := time.Now().Add(5 * time.Second)
	for e.get(t, id).Status != "queued" {
		if time.Now().After(deadline) {
			t.Fatal("Run never submitted the download")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Review focus 3: the list comes from grab's store, not MeTube.
func TestListWorksWhileMeTubeDown(t *testing.T) {
	e := newGrabEnv(t)
	_, tok := e.user(t, "alex", false)
	id := e.submit(t, tok, `{"url":"https://youtu.be/abc"}`)
	e.mt.histErr = errors.New("connection refused")
	e.mt.addErr = errors.New("connection refused")
	e.tr.Tick(context.Background())
	w := e.req(t, tok, "GET", "/api/items", "")
	var items []Item
	json.Unmarshal(w.Body.Bytes(), &items)
	if w.Code != 200 || len(items) != 1 || items[0].ID != id || items[0].Status != "waiting" ||
		items[0].Source != "youtube" || !items[0].Mine || items[0].Target != "original" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

func TestItemVisibility(t *testing.T) {
	e := newGrabEnv(t)
	a, ta := e.user(t, "a", false)
	_, tb := e.user(t, "b", false)
	_, tadmin := e.user(t, "root", true)
	priv, _ := e.st.CreateDownload(Download{UserID: a.ID, SharedURL: "https://youtu.be/p", Site: "youtube",
		Folder: "users/a/youtube", Status: "finished", Filename: "p.mp4", Target: "original"})
	pub, _ := e.st.CreateDownload(Download{UserID: a.ID, SharedURL: "https://youtu.be/q", Site: "youtube",
		Folder: "public/youtube", Status: "finished", Filename: "q.mp4", Public: true, Target: "original"})

	for _, path := range []string{"/api/items/" + priv.ID, "/api/items/" + priv.ID + "/file?preset=original"} {
		if w := e.req(t, tb, "GET", path, ""); w.Code != 404 {
			t.Errorf("b GET %s: %d", path, w.Code)
		}
	}
	if w := e.req(t, tb, "POST", "/api/items/"+priv.ID+"/prepare?preset=original", ""); w.Code != 404 {
		t.Errorf("b prepare: %d", w.Code)
	}
	item := func(tok, id string) ItemDetail {
		t.Helper()
		w := e.req(t, tok, "GET", "/api/items/"+id, "")
		var d ItemDetail
		json.Unmarshal(w.Body.Bytes(), &d)
		if w.Code != 200 {
			t.Fatalf("GET %s: %d %s", id, w.Code, w.Body)
		}
		return d
	}
	if d := item(tb, pub.ID); !d.Public || d.Mine || d.Owner != "" {
		t.Errorf("b sees public: %+v", d.Item)
	}
	if d := item(ta, priv.ID); !d.Mine || d.Owner != "" {
		t.Errorf("a sees own: %+v", d.Item)
	}
	if d := item(tadmin, priv.ID); d.Mine || d.Owner != "a" {
		t.Errorf("admin sees private: %+v", d.Item)
	}
	list := func(tok string) map[string]Item {
		t.Helper()
		w := e.req(t, tok, "GET", "/api/items", "")
		var items []Item
		json.Unmarshal(w.Body.Bytes(), &items)
		out := map[string]Item{}
		for _, it := range items {
			out[it.ID] = it
		}
		return out
	}
	if l := list(tb); len(l) != 1 || l[pub.ID].ID == "" {
		t.Errorf("b list: %+v", l)
	}
	if l := list(tadmin); len(l) != 2 || l[priv.ID].Owner != "a" || l[pub.ID].Owner != "a" {
		t.Errorf("admin list: %+v", l)
	}
}

// MeTube keys its history by yt-dlp's webpage_url, not the link it was sent:
// youtu.be/abc?si=x comes back as youtube.com/watch?v=abc.
func TestTrackerPairsWhenMeTubeRewritesURL(t *testing.T) {
	e := newGrabEnv(t)
	e.mt.simulate, e.mt.rewrite = true, youtubeCanon
	_, tok := e.user(t, "alex", false)
	id := e.submit(t, tok, `{"url":"https://youtu.be/abc?si=x"}`)
	e.tick(t)
	d := e.get(t, id)
	if d.Status != "downloading" || d.MeTubeURL != "https://www.youtube.com/watch?v=abc" || d.SharedURL != "https://youtu.be/abc?si=x" {
		t.Fatalf("not paired: %+v", d)
	}
	if len(e.mt.adds) != 1 || e.mt.adds[0].URL != "https://youtu.be/abc?si=x" {
		t.Fatalf("adds %+v", e.mt.adds)
	}
	writeFile(t, filepath.Join(e.shares, "users", "alex", "youtube", "a.mp4"))
	e.mt.finish("https://www.youtube.com/watch?v=abc", "a.mp4")
	e.now = e.now.Add(time.Hour) // well past the lost-download timer
	e.tick(t)
	if d := e.get(t, id); d.Status != "finished" || d.Filename != "a.mp4" {
		t.Fatalf("got %+v", d)
	}
}

// Pairing by folder is only unambiguous with one unpaired download per folder.
func TestTrackerSerializesSameFolder(t *testing.T) {
	e := newGrabEnv(t)
	_, tok := e.user(t, "alex", false)
	first := e.submit(t, tok, `{"url":"https://youtu.be/one"}`)
	second := e.submit(t, tok, `{"url":"https://youtu.be/two"}`)
	e.tick(t)
	e.tick(t)
	if len(e.mt.adds) != 1 || e.mt.adds[0].URL != "https://youtu.be/one" {
		t.Fatalf("adds %+v", e.mt.adds)
	}
	if d := e.get(t, second); d.Status != "waiting" {
		t.Fatalf("second: %+v", d)
	}
	// MeTube shows the first under its own key: it pairs, which frees the folder.
	e.mt.h.Queue = []HistoryItem{{URL: "https://www.youtube.com/watch?v=one", Folder: "users/alex/youtube",
		Timestamp: e.now.UnixNano()}}
	e.tick(t)
	if d := e.get(t, first); d.Status != "downloading" || d.MeTubeURL != "https://www.youtube.com/watch?v=one" {
		t.Fatalf("first: %+v", d)
	}
	e.tick(t)
	if len(e.mt.adds) != 2 || e.mt.adds[1].URL != "https://youtu.be/two" {
		t.Fatalf("adds %+v", e.mt.adds)
	}
	// Another user's folder is not held up by alex's unpaired download.
	_, tb := e.user(t, "bob", false)
	e.submit(t, tb, `{"url":"https://youtu.be/three"}`)
	e.tick(t)
	if len(e.mt.adds) != 3 || e.mt.adds[2].Folder != "users/bob/youtube" {
		t.Fatalf("adds %+v", e.mt.adds)
	}
}

// Two spellings of one video: MeTube says "Already in queue" for the second
// and drops it, so grab keeps it waiting and retries after the first is done.
func TestTrackerAlreadyInQueueRetries(t *testing.T) {
	e := newGrabEnv(t)
	e.mt.simulate, e.mt.rewrite = true, youtubeCanon
	_, ta := e.user(t, "a", false)
	_, tb := e.user(t, "b", false)
	ida := e.submit(t, ta, `{"url":"https://youtu.be/abc"}`)
	idb := e.submit(t, tb, `{"url":"https://youtu.be/abc?si=z"}`)
	e.tick(t)
	if d := e.get(t, idb); d.Status != "waiting" || !d.QueuedAt.IsZero() || d.Error != "" {
		t.Fatalf("b after 'already in queue': %+v", d)
	}
	if d := e.get(t, ida); d.Status != "downloading" {
		t.Fatalf("a: %+v", d)
	}
	e.mt.finish("https://www.youtube.com/watch?v=abc", "a.mp4")
	e.now = e.now.Add(2 * time.Minute)
	e.tick(t)
	e.tick(t)
	d := e.get(t, idb)
	if d.Status != "downloading" || d.MeTubeURL != "https://www.youtube.com/watch?v=abc" || d.Folder != "users/b/youtube" {
		t.Fatalf("b after retry: %+v", d)
	}
	if d := e.get(t, ida); d.Status != "finished" {
		t.Fatalf("a: %+v", d)
	}
}

func TestTrackerPairingIgnoresOtherFoldersAndClaimedEntries(t *testing.T) {
	e := newGrabEnv(t)
	alex, tok := e.user(t, "alex", false)
	folder := "users/alex/youtube"
	fresh := e.now.UnixNano()
	// An earlier download of alex's owns this exact MeTube entry (URL and
	// timestamp), e.g. one paired a moment before this one was sent.
	if _, err := e.st.CreateDownload(Download{UserID: alex.ID, SharedURL: "https://youtu.be/old",
		MeTubeURL: "https://www.youtube.com/watch?v=old", MeTubeTS: fresh, Site: "youtube", Folder: folder,
		Status: "finished"}); err != nil {
		t.Fatal(err)
	}
	id := e.submit(t, tok, `{"url":"https://youtu.be/new"}`)
	e.tick(t) // queued at e.now
	e.mt.h.Queue = []HistoryItem{{URL: "https://www.youtube.com/watch?v=bob", Folder: "users/bob/youtube", Timestamp: fresh}}
	e.mt.h.Done = []HistoryItem{
		{URL: "https://www.youtube.com/watch?v=old", Folder: folder, Status: "finished", Timestamp: fresh},
		{URL: "https://www.youtube.com/watch?v=stale", Folder: folder, Status: "finished",
			Timestamp: e.now.Add(-time.Minute).UnixNano()},
	}
	e.tick(t)
	if d := e.get(t, id); d.Status != "queued" || d.MeTubeURL != "https://youtu.be/new" {
		t.Fatalf("paired with someone else's entry: %+v", d)
	}
	e.mt.h.Pending = []HistoryItem{{URL: "https://www.youtube.com/watch?v=new", Folder: folder,
		Timestamp: e.now.Add(-2 * time.Second).UnixNano()}}
	e.tick(t)
	if d := e.get(t, id); d.Status != "downloading" || d.MeTubeURL != "https://www.youtube.com/watch?v=new" ||
		d.MeTubeTS != e.now.Add(-2*time.Second).UnixNano() {
		t.Fatalf("got %+v", d)
	}
}

// MeTube reuses one key for every download of a video, so an earlier
// download owns only the entry with its own timestamp. Re-sharing a video
// that was grabbed (or failed) before must pair with MeTube's new entry.
func TestTrackerReshareAfterEarlierDownloadPairs(t *testing.T) {
	for _, status := range []string{"finished", "error"} {
		t.Run(status, func(t *testing.T) {
			e := newGrabEnv(t)
			e.mt.simulate, e.mt.rewrite = true, youtubeCanon
			alex, tok := e.user(t, "alex", false)
			folder := "users/alex/youtube"
			old := e.now.Add(-time.Hour).UnixNano()
			a, err := e.st.CreateDownload(Download{UserID: alex.ID, SharedURL: "https://youtu.be/abc?si=1",
				MeTubeURL: "https://www.youtube.com/watch?v=abc", MeTubeTS: old, Site: "youtube", Folder: folder,
				Status: status, Filename: "a.mp4"})
			if err != nil {
				t.Fatal(err)
			}
			e.mt.setDone(HistoryItem{URL: "https://www.youtube.com/watch?v=abc", Folder: folder, Status: status,
				Filename: "a.mp4", Timestamp: old})
			id := e.submit(t, tok, `{"url":"https://youtu.be/abc?si=2"}`)
			e.tick(t)
			if d := e.get(t, id); d.Status != "downloading" || d.MeTubeTS != e.now.UnixNano() {
				t.Fatalf("b not paired: %+v", d)
			}
			e.mt.finish("https://www.youtube.com/watch?v=abc", "b.mp4")
			e.tick(t)
			if d := e.get(t, id); d.Status != "finished" || d.Filename != "b.mp4" {
				t.Fatalf("b: %+v", d)
			}
			if d := e.get(t, a.ID); d.Status != status || d.Filename != "a.mp4" {
				t.Fatalf("a changed: %+v", d)
			}
		})
	}
}

func TestSubmitDuplicateReturnsExisting(t *testing.T) {
	e := newGrabEnv(t)
	_, ta := e.user(t, "a", false)
	_, tb := e.user(t, "b", false)
	id := e.submit(t, ta, `{"url":"https://youtu.be/abc"}`)
	for _, body := range []string{`{"url":"https://youtu.be/abc"}`, `{"text":"again https://youtu.be/abc"}`} {
		w := e.req(t, ta, "POST", "/api/downloads", body)
		var out struct {
			ID       string
			Existing bool
		}
		json.Unmarshal(w.Body.Bytes(), &out)
		if w.Code != 200 || out.ID != id || !out.Existing {
			t.Fatalf("%s: %d %s", body, w.Code, w.Body)
		}
	}
	// Still deduplicated once it's finished.
	e.st.UpdateDownload(id, func(d *Download) error { d.Status = "finished"; return nil })
	if w := e.req(t, ta, "POST", "/api/downloads", `{"url":"https://youtu.be/abc"}`); w.Code != 200 {
		t.Fatalf("finished: %d", w.Code)
	}
	// Another user gets a record of their own.
	if other := e.submit(t, tb, `{"url":"https://youtu.be/abc"}`); other == id {
		t.Fatal("b got a's record")
	}
	if ds, _ := e.st.ListDownloads(ListFilter{Admin: true}); len(ds) != 2 {
		t.Fatalf("%d records, want 2", len(ds))
	}
}

// Sharing a failed link again (say, after uploading cookies) retries the
// same record, and the old failed entry in MeTube's history is not taken
// for the new attempt.
func TestSubmitAfterErrorRetriesSameRecord(t *testing.T) {
	e := newGrabEnv(t)
	_, tok := e.user(t, "alex", false)
	folder := "users/alex/youtube"
	id := e.submit(t, tok, `{"url":"https://youtu.be/abc"}`)
	e.tick(t)
	e.mt.setDone(HistoryItem{URL: "https://youtu.be/abc", Folder: folder, Status: "error", Msg: "Sign in"})
	e.tick(t)
	if d := e.get(t, id); d.Status != "error" || d.NotifiedAt.IsZero() || d.MeTubeTS == 0 {
		t.Fatalf("first attempt: %+v", d)
	}
	e.now = e.now.Add(time.Minute)
	w := e.req(t, tok, "POST", "/api/downloads", `{"url":"https://youtu.be/abc"}`)
	var out struct{ ID string }
	json.Unmarshal(w.Body.Bytes(), &out)
	if w.Code != 200 || out.ID != id {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if d := e.get(t, id); d.Status != "waiting" || d.Error != "" || !d.QueuedAt.IsZero() || d.MeTubeTS != 0 || !d.NotifiedAt.IsZero() {
		t.Fatalf("not reset: %+v", d)
	}
	e.tick(t)
	if d := e.get(t, id); d.Status != "queued" || len(e.mt.adds) != 2 {
		t.Fatalf("retry not sent, or matched the stale failure: %+v", d)
	}
	e.mt.h.Queue = []HistoryItem{{URL: "https://youtu.be/abc", Folder: folder, Timestamp: e.now.UnixNano()}}
	e.tick(t)
	if d := e.get(t, id); d.Status != "downloading" {
		t.Fatalf("got %+v", d)
	}
}

// MeTube runs yt-dlp's extraction before answering /add and queues the
// download even if grab stopped waiting, so a timeout means "maybe queued".
// Retention (DELETE_AFTER_DAYS) counts from the retry, not from the first
// share: a link retried after weeks must not be swept the next hour.
func TestRetryRestartsRetentionClock(t *testing.T) {
	e := newGrabEnv(t)
	_, tok := e.user(t, "alex", false)
	id := e.submit(t, tok, `{"url":"https://youtu.be/abc"}`)
	old := time.Now().Add(-40 * 24 * time.Hour)
	if _, err := e.st.UpdateDownload(id, func(x *Download) error {
		x.Status, x.Error, x.CreatedAt = "error", "Sign in", old
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	if w := e.req(t, tok, "POST", "/api/downloads", `{"url":"https://youtu.be/abc"}`); w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if d := e.get(t, id); d.Status != "waiting" || d.CreatedAt.Before(before) {
		t.Fatalf("created_at not reset: %v (retried at %v)", d.CreatedAt, before)
	}
}

func TestAddTimeoutCountsAsMaybeQueued(t *testing.T) {
	e := newGrabEnv(t)
	_, tok := e.user(t, "alex", false)
	folder := "users/alex/youtube"
	e.mt.addErr = &url.Error{Op: "Post", URL: "http://metube/add", Err: context.DeadlineExceeded}
	first := e.submit(t, tok, `{"url":"https://youtu.be/one"}`)
	second := e.submit(t, tok, `{"url":"https://youtu.be/two"}`)
	e.tick(t)
	if d := e.get(t, first); d.Status != "queued" || !d.QueuedAt.Equal(e.now) {
		t.Fatalf("first: %+v", d)
	}
	if d := e.get(t, second); d.Status != "waiting" || e.mt.calls != 1 {
		t.Fatalf("second sent into an unpaired folder: %+v calls=%d", d, e.mt.calls)
	}
	e.mt.addErr = nil
	e.mt.h.Queue = []HistoryItem{{URL: "https://www.youtube.com/watch?v=one", Folder: folder, Timestamp: e.now.UnixNano()}}
	e.tick(t)
	if d := e.get(t, first); d.Status != "downloading" {
		t.Fatalf("first not paired: %+v", d)
	}
	e.tick(t)
	if d := e.get(t, second); d.Status != "queued" {
		t.Fatalf("second: %+v", d)
	}
}

// A connection that times out never reached MeTube: that is "down", not "maybe queued".
func TestAddDialTimeoutStaysWaiting(t *testing.T) {
	e := newGrabEnv(t)
	_, tok := e.user(t, "alex", false)
	e.mt.addErr = &url.Error{Op: "Post", URL: "http://metube/add",
		Err: &net.OpError{Op: "dial", Net: "tcp", Err: os.ErrDeadlineExceeded}}
	id := e.submit(t, tok, `{"url":"https://youtu.be/one"}`)
	e.tr.Tick(context.Background())
	if d := e.get(t, id); d.Status != "waiting" {
		t.Fatalf("got %+v", d)
	}
}
