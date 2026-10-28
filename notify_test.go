package main

import (
	"bytes"
	"context"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestHumanBytes(t *testing.T) {
	for in, want := range map[int64]string{12_300_000: "12.3 MB", 1_950_000_000: "1.9 GB", 0: "0.0 MB"} {
		if got := humanBytes(in); got != want {
			t.Errorf("%d: got %s want %s", in, got, want)
		}
	}
}

type sent struct {
	urls        []string
	title, body string
}
type fakeSender struct{ got []sent }

func (f *fakeSender) Send(_ context.Context, urls []string, title, body string) error {
	f.got = append(f.got, sent{urls, title, body})
	return nil
}

func testNotifier(t *testing.T, mode string) (*Notifier, *fakeSender, *Store) {
	t.Helper()
	st, _ := testStore(t)
	fs := &fakeSender{}
	base, _ := url.Parse("https://grab.example.com")
	return &Notifier{Store: st, Sender: fs, BaseURL: base, Mode: mode}, fs, st
}

func TestNotifyUsesOwnersURLs(t *testing.T) {
	n, fs, st := testNotifier(t, "oidc")
	for _, id := range []string{"alex", "bob"} {
		if _, err := st.UpsertUser(id, id, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SetNotifyURLs("alex", []string{"tgram://tok/1", "json://example.com/hook"}); err != nil {
		t.Fatal(err)
	}
	if err := n.Notify(context.Background(), Download{ID: "0123456789abcdef", UserID: "alex", Title: "A", Site: "youtube", Status: "finished"}, ""); err != nil {
		t.Fatal(err)
	}
	if len(fs.got) != 1 || !reflect.DeepEqual(fs.got[0].urls, []string{"tgram://tok/1", "json://example.com/hook"}) {
		t.Fatalf("got %+v", fs.got)
	}
	if err := n.Notify(context.Background(), Download{ID: "fedcba9876543210", UserID: "bob", Title: "B", Status: "finished"}, ""); err != nil {
		t.Fatal(err)
	}
	if len(fs.got) != 1 {
		t.Fatalf("Bob has no URLs but Send was called: %+v", fs.got)
	}
}

func TestNotifyNoneModeUsesInstanceURLs(t *testing.T) {
	n, fs, _ := testNotifier(t, "none")
	n.InstanceURLs = []string{"json://example.com/a"}
	if err := n.Notify(context.Background(), Download{ID: "0123456789abcdef", UserID: "local", Title: "T", Status: "finished"}, ""); err != nil {
		t.Fatal(err)
	}
	if len(fs.got) != 1 || fs.got[0].urls[0] != "json://example.com/a" {
		t.Fatalf("got %+v", fs.got)
	}
	n.InstanceURLs = nil
	if err := n.Notify(context.Background(), Download{ID: "0123456789abcdef", UserID: "local", Status: "finished"}, ""); err != nil || len(fs.got) != 1 {
		t.Fatalf("empty instance URLs must send nothing: %v %+v", err, fs.got)
	}
}

func TestReadyMessage(t *testing.T) {
	n, _, _ := testNotifier(t, "none")
	title, body := n.message(Download{ID: "0123456789abcdef", Title: "Cats", Site: "youtube", Size: 12_300_000, Status: "finished"}, "")
	if title != "Ready: Cats" {
		t.Errorf("title %q", title)
	}
	for _, want := range []string{"youtube · 12.3 MB", "https://grab.example.com/?item=0123456789abcdef"} {
		if !strings.Contains(body, want) {
			t.Errorf("body %q lacks %q", body, want)
		}
	}
}

func TestErrorMessageFallsBackToSharedURLAndTruncates(t *testing.T) {
	n, _, _ := testNotifier(t, "none")
	title, body := n.message(Download{SharedURL: "https://example.com/v", Error: strings.Repeat("x", 400) + "TAIL", Status: "error"}, "")
	if title != "Failed: https://example.com/v" || len(body) != 300 || !strings.HasSuffix(body, "TAIL") {
		t.Errorf("title %q body len %d", title, len(body))
	}
}

func TestErrorMessageCookieHint(t *testing.T) {
	n, _, _ := testNotifier(t, "none")
	_, body := n.message(Download{Site: "youtube", Error: "Sign in to confirm you're not a bot", Status: "error"}, "")
	if !strings.Contains(body, "?settings#cookies-youtube") {
		t.Errorf("body %q", body)
	}
}

// fakeApprise writes an executable script that records its args and copies
// the --config file, then runs extra.
func fakeApprise(t *testing.T, extra string) (bin, argsFile, cfgCopy string) {
	t.Helper()
	dir := t.TempDir()
	bin, argsFile, cfgCopy = filepath.Join(dir, "apprise"), filepath.Join(dir, "args"), filepath.Join(dir, "cfg")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsFile + "\ncp \"$2\" " + cfgCopy + "\n" + extra + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return
}

func TestAppriseCLIArgsAndCleanup(t *testing.T) {
	bin, argsFile, cfgCopy := fakeApprise(t, "exit 0")
	tmp := t.TempDir()
	err := AppriseCLI{Bin: bin, TmpDir: tmp}.Send(context.Background(), []string{"json://a.example.com", "json://b.example.com"}, "T", "B\nline2")
	if err != nil {
		t.Fatal(err)
	}
	args, _ := os.ReadFile(argsFile)
	lines := strings.Split(strings.TrimRight(string(args), "\n"), "\n")
	if len(lines) != 7 || lines[0] != "--config" || lines[2] != "--title" || lines[3] != "T" || lines[4] != "--body" || lines[5] != "B" || lines[6] != "line2" {
		t.Errorf("args %q", lines)
	}
	cfg, _ := os.ReadFile(cfgCopy)
	if string(cfg) != "json://a.example.com\njson://b.example.com\n" {
		t.Errorf("config %q", cfg)
	}
	if left, _ := os.ReadDir(tmp); len(left) != 0 {
		t.Errorf("temp file left behind: %v", left)
	}
}

func TestAppriseFailureDoesNotLogURL(t *testing.T) {
	const secret = "tgram://123456:SECRETTOKEN/chat"
	bin, _, _ := fakeApprise(t, "echo "+secret+"; echo "+secret+" >&2; exit 1")
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	err := AppriseCLI{Bin: bin, TmpDir: t.TempDir()}.Send(context.Background(), []string{secret}, "T", "B")
	if err == nil || err.Error() != "apprise exited 1" {
		t.Fatalf("err %v", err)
	}
	if strings.Contains(err.Error(), "SECRETTOKEN") || strings.Contains(logs.String(), "SECRETTOKEN") {
		t.Errorf("secret leaked: %v / %s", err, logs.String())
	}
}

func TestStartupNotice(t *testing.T) {
	for _, c := range []struct {
		n    *Notifier
		want string
	}{
		{&Notifier{Mode: "oidc", InstanceURLs: nil}, ""},
		{&Notifier{Mode: "none"}, "notifications: off (APPRISE_URLS is empty)"},
		{&Notifier{Mode: "none", InstanceURLs: []string{"tgram://secret/1", "mailto://x"}}, "notifications: 2 Apprise URLs configured"},
		{&Notifier{Mode: "none", InstanceURLs: []string{"tgram://secret/1"}}, "notifications: 1 Apprise URL configured"},
	} {
		if got := c.n.StartupNotice(); got != c.want {
			t.Errorf("%s %d: got %q, want %q", c.n.Mode, len(c.n.InstanceURLs), got, c.want)
		}
	}
}
