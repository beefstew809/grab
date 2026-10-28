package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestItemIDMatchesKnownValue(t *testing.T) {
	// Same literal is asserted by the PWA e2e test: JS and Go must agree.
	if got := ItemID("  https://www.youtube.com/watch?v=AcvoSoUT7EE\n"); got != "10530c6fb5070182" {
		t.Fatalf("got %s", got)
	}
}

func TestResolvePathRefusesEscape(t *testing.T) {
	root := t.TempDir()
	for _, c := range []struct{ folder, file string }{
		{"youtube", "../../etc/passwd"},
		{"..", "x.mp4"},
		{"youtube", "/etc/passwd"},
		{"youtube", ""},
		{"youtube/../..", "x.mp4"},
	} {
		if p, ok := resolvePath(root, c.folder, c.file); ok {
			t.Errorf("%q/%q resolved to %s", c.folder, c.file, p)
		}
	}
	if p, ok := resolvePath(root, "youtube", "a [x].webm"); !ok || p != filepath.Join(root, "youtube", "a [x].webm") {
		t.Errorf("legit path refused: %s %v", p, ok)
	}
}

func writeFile(t *testing.T, path string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestMeTubeHistory(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/history" {
			http.NotFound(w, r)
			return
		}
		// Shape copied from a real MeTube 2026.08.28 response, nulls included.
		w.Write([]byte(`{"done":[{"id":"AcvoSoUT7EE","title":"T","url":"https://www.youtube.com/watch?v=AcvoSoUT7EE",
		 "folder":"youtube","msg":null,"percent":100.0,"speed":null,"status":"finished","size":204384621,
		 "timestamp":1790808248041641193,"error":null,"filename":"T [AcvoSoUT7EE].webm"}],"queue":[],"pending":[]}`))
	}))
	defer srv.Close()
	h, err := (&MeTube{Base: srv.URL, HC: srv.Client()}).History(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Done) != 1 || h.Done[0].Size != 204384621 || h.Done[0].Filename != "T [AcvoSoUT7EE].webm" {
		t.Fatalf("got %+v", h)
	}
}

func TestMeTubeHistoryErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer srv.Close()
	if _, err := (&MeTube{Base: srv.URL, HC: srv.Client()}).History(context.Background()); err == nil {
		t.Fatal("want error")
	}
}

// metubeRecorder is an httptest MeTube that records each request body and
// answers with a fixed status and body.
func metubeRecorder(t *testing.T, code int, reply string) (*MeTube, *[]map[string]any, *[]string) {
	t.Helper()
	var bodies []map[string]any
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
			t.Errorf("body: %v", err)
		}
		bodies = append(bodies, m)
		paths = append(paths, r.Method+" "+r.URL.Path+" "+r.Header.Get("Content-Type"))
		w.WriteHeader(code)
		w.Write([]byte(reply))
	}))
	t.Cleanup(srv.Close)
	return &MeTube{Base: srv.URL + "/", HC: srv.Client()}, &bodies, &paths
}

func TestMeTubeAddPayload(t *testing.T) {
	mt, bodies, paths := metubeRecorder(t, 200, `{"status":"ok"}`)
	ctx := context.Background()
	if err := mt.Add(ctx, AddRequest{URL: "https://youtu.be/abc", Folder: "users/alex/youtube"}); err != nil {
		t.Fatal(err)
	}
	if err := mt.Add(ctx, AddRequest{URL: "https://www.instagram.com/reel/x/", Folder: "users/alex/instagram",
		CookieFile: "/cookies/alex/instagram.txt"}); err != nil {
		t.Fatal(err)
	}
	want := []map[string]any{
		{"url": "https://youtu.be/abc", "download_type": "video", "codec": "h264", "format": "mp4",
			"quality": "best", "folder": "users/alex/youtube"},
		{"url": "https://www.instagram.com/reel/x/", "download_type": "video", "codec": "h264", "format": "mp4",
			"quality": "best", "folder": "users/alex/instagram",
			"ytdl_options_overrides": map[string]any{"cookiefile": "/cookies/alex/instagram.txt"}},
	}
	if !reflect.DeepEqual(*bodies, want) {
		t.Fatalf("got  %v\nwant %v", *bodies, want)
	}
	if (*paths)[0] != "POST /add application/json" {
		t.Fatalf("request %q", (*paths)[0])
	}
}

func TestMeTubeAddErrors(t *testing.T) {
	ctx := context.Background()
	req := AddRequest{URL: "https://example.com/x", Folder: "users/alex/other"}
	var me *MeTubeError

	mt, _, _ := metubeRecorder(t, 200, `{"status":"error","msg":"Unsupported URL"}`)
	if err := mt.Add(ctx, req); !errors.As(err, &me) || me.Msg != "Unsupported URL" {
		t.Errorf("status error: %v", err)
	}
	mt, _, _ = metubeRecorder(t, 400, `{"status":"error","msg":"bad folder"}`)
	if err := mt.Add(ctx, req); !errors.As(err, &me) || me.Msg != "bad folder" {
		t.Errorf("4xx with json: %v", err)
	}
	mt, _, _ = metubeRecorder(t, 400, `400: Bad Request`)
	if err := mt.Add(ctx, req); !errors.As(err, &me) || me.Msg != "400: Bad Request" {
		t.Errorf("4xx without json: %v", err)
	}
	// "Already in queue" is MeTube dropping the request: not queued, not refused.
	mt, _, _ = metubeRecorder(t, 200, `{"status":"ok","msg":"Already in queue: A video"}`)
	if err := mt.Add(ctx, req); !errors.Is(err, ErrAlreadyQueued) {
		t.Errorf("already in queue: %v", err)
	}
	// MeTube itself failing is not a verdict on the link: retry later.
	mt, _, _ = metubeRecorder(t, 502, `<html>Bad Gateway</html>`)
	if err := mt.Add(ctx, req); err == nil || errors.As(err, &me) {
		t.Errorf("5xx: %v", err)
	}
	dead := &MeTube{Base: "http://127.0.0.1:1", HC: http.DefaultClient}
	if err := dead.Add(ctx, req); err == nil || errors.As(err, &me) {
		t.Errorf("connection refused: %v", err)
	}
}

func TestMeTubeDelete(t *testing.T) {
	mt, bodies, paths := metubeRecorder(t, 200, `{"status":"ok"}`)
	if err := mt.Delete(context.Background(), []string{"https://youtu.be/a"}); err != nil {
		t.Fatal(err)
	}
	want := []map[string]any{{"ids": []any{"https://youtu.be/a"}, "where": "done"}}
	if !reflect.DeepEqual(*bodies, want) || (*paths)[0] != "POST /delete application/json" {
		t.Fatalf("got %v %v", *bodies, *paths)
	}
}

// /add waits for MeTube's yt-dlp extraction, which can take far longer than
// a /history call: Add must not inherit the shared client's short timeout.
func TestMeTubeAddOutlivesHistoryTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()
	hc := srv.Client()
	hc.Timeout = 50 * time.Millisecond
	mt := &MeTube{Base: srv.URL, HC: hc, AddTimeout: 5 * time.Second}
	if err := mt.Add(context.Background(), AddRequest{URL: "https://youtu.be/a", Folder: "users/a/youtube"}); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := mt.History(context.Background()); err == nil {
		t.Fatal("history should still time out")
	}
	if hc.Timeout != 50*time.Millisecond {
		t.Fatal("Add changed the shared client")
	}
}
