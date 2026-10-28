package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// migrateFixture: user "alex" exists; shares holds one tracked and one
// untracked youtube file, an "other" file and a top-level sentinel.
func migrateFixture(t *testing.T) (*Store, string, *fakeMeTube) {
	t.Helper()
	st, _ := testStore(t)
	if _, err := st.UpsertUser("sub-1", "alex", false); err != nil {
		t.Fatal(err)
	}
	shares := t.TempDir()
	for _, f := range []string{"youtube/a [x].mp4", "youtube/b.mp4", "other/c.mkv", "HealthCheck.txt"} {
		p := filepath.Join(shares, f)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte("data"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mt := &fakeMeTube{h: History{Done: []HistoryItem{{Title: "A Video", URL: "https://youtu.be/x",
		Folder: "youtube", Filename: "a [x].mp4", Status: "finished", Timestamp: 7}}}}
	return st, shares, mt
}

func TestMigrateDryRunMovesNothing(t *testing.T) {
	st, shares, mt := migrateFixture(t)
	var out bytes.Buffer
	if err := Migrate(context.Background(), st, mt, shares, "alex", false, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "youtube/a [x].mp4 -> users/alex/youtube/a [x].mp4\n") ||
		!strings.Contains(out.String(), "3 to move\n") {
		t.Fatalf("output:\n%s", out.String())
	}
	if !exists(filepath.Join(shares, "youtube/a [x].mp4")) || exists(filepath.Join(shares, "users")) {
		t.Fatal("dry run touched files")
	}
	if l, _ := st.ListDownloads(ListFilter{Admin: true}); len(l) != 0 {
		t.Fatalf("dry run created records: %+v", l)
	}
}

func TestMigrateApply(t *testing.T) {
	st, shares, mt := migrateFixture(t)
	var out bytes.Buffer
	if err := Migrate(context.Background(), st, mt, shares, "alex", true, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "3 moved\n") {
		t.Fatalf("output:\n%s", out.String())
	}
	if !exists(filepath.Join(shares, "users/alex/youtube/a [x].mp4")) || exists(filepath.Join(shares, "youtube/a [x].mp4")) {
		t.Fatal("file not moved")
	}
	d, err := st.GetDownload(ItemID("https://youtu.be/x"))
	if err != nil {
		t.Fatal(err)
	}
	u, _ := st.UserByDir("alex")
	if d.UserID != u.ID || d.Public || d.Target != "original" || d.Status != "finished" || d.NotifiedAt.IsZero() ||
		d.Title != "A Video" || d.SharedURL != "https://youtu.be/x" || d.Folder != "users/alex/youtube" ||
		d.Site != "youtube" || d.Size != 4 || d.FinishedAt.IsZero() {
		t.Fatalf("record: %+v", d)
	}
	ds, _ := st.DownloadsByFile("users/alex/other", "c.mkv")
	if len(ds) != 1 || len(ds[0].ID) != 16 || ds[0].Title != "c" || ds[0].SharedURL != "" {
		t.Fatalf("untracked record: %+v", ds)
	}
}

func TestMigrateIdempotent(t *testing.T) {
	st, shares, mt := migrateFixture(t)
	for i := 0; i < 2; i++ {
		var out bytes.Buffer
		if err := Migrate(context.Background(), st, mt, shares, "alex", true, &out); err != nil {
			t.Fatal(err)
		}
		if i == 1 && !strings.Contains(out.String(), "0 moved\n") {
			t.Fatalf("second run:\n%s", out.String())
		}
		if n := len(allDownloads(t, st)); n != 3 {
			t.Fatalf("run %d: %d rows, want 3", i+1, n)
		}
	}
}

func TestMigrateUnknownUser(t *testing.T) {
	st, shares, mt := migrateFixture(t)
	if err := Migrate(context.Background(), st, mt, shares, "nobody", true, &bytes.Buffer{}); err == nil {
		t.Fatal("want error")
	}
}

func TestMigrateLeavesSentinel(t *testing.T) {
	st, shares, mt := migrateFixture(t)
	if err := Migrate(context.Background(), st, mt, shares, "alex", true, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(shares, "HealthCheck.txt")) {
		t.Fatal("sentinel moved")
	}
}

func TestMigrateWorksWithoutMeTube(t *testing.T) {
	st, shares, mt := migrateFixture(t)
	mt.histErr = errors.New("down")
	var out bytes.Buffer
	if err := Migrate(context.Background(), st, mt, shares, "alex", true, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "warning:") || !strings.Contains(out.String(), "3 moved\n") {
		t.Fatalf("output:\n%s", out.String())
	}
	ds, _ := st.DownloadsByFile("users/alex/youtube", "a [x].mp4")
	if len(ds) != 1 || ds[0].ID == ItemID("https://youtu.be/x") || ds[0].Title != "a [x]" {
		t.Fatalf("%+v", ds)
	}
}

func TestMigrateTakenIDFallsBackToRandom(t *testing.T) {
	st, shares, mt := migrateFixture(t)
	u, _ := st.UserByDir("alex")
	st.CreateDownload(Download{ID: ItemID("https://youtu.be/x"), UserID: u.ID, Folder: "elsewhere", Filename: "z.mp4", Status: "finished"})
	var out bytes.Buffer
	if err := Migrate(context.Background(), st, mt, shares, "alex", true, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "random id") {
		t.Fatalf("no fallback note:\n%s", out.String())
	}
	ds, _ := st.DownloadsByFile("users/alex/youtube", "a [x].mp4")
	if len(ds) != 1 || ds[0].ID == ItemID("https://youtu.be/x") {
		t.Fatalf("%+v", ds)
	}
}

func allDownloads(t *testing.T, st *Store) []Download {
	t.Helper()
	l, err := st.ListDownloads(ListFilter{Admin: true})
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestMigrateRollbackFailureReportsGhost(t *testing.T) {
	st, shares, mt := migrateFixture(t)
	// A regular file where the destination folder must go makes MkdirAll fail.
	os.MkdirAll(filepath.Join(shares, "users/alex"), 0o755)
	os.WriteFile(filepath.Join(shares, "users/alex/youtube"), []byte("x"), 0o644)
	err := Migrate(context.Background(), st, mt, shares, "alex", true, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "after 0 moved") {
		t.Fatalf("err = %v", err)
	}
	if n := len(allDownloads(t, st)); n != 0 {
		t.Fatalf("record not rolled back: %d rows", n)
	}
}

func TestMigrateRerunReusesGhostRecord(t *testing.T) {
	st, shares, mt := migrateFixture(t)
	u, _ := st.UserByDir("alex")
	ghost, err := st.CreateDownload(Download{ID: "0123456789abcdef", UserID: u.ID, Site: "other", Status: "finished",
		Folder: "users/alex/other", Filename: "c.mkv"})
	if err != nil {
		t.Fatal(err)
	}
	if err := Migrate(context.Background(), st, mt, shares, "alex", true, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	ds, _ := st.DownloadsByFile("users/alex/other", "c.mkv")
	if len(ds) != 1 || ds[0].ID != ghost.ID {
		t.Fatalf("records: %+v", ds)
	}
	if !exists(filepath.Join(shares, "users/alex/other/c.mkv")) {
		t.Fatal("file not moved")
	}
	if n := len(allDownloads(t, st)); n != 3 {
		t.Fatalf("%d rows, want 3", n)
	}
}
