package main

import (
	"context"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"
)

func TestDeleteKeepsOtherUsersCompletionHistory(t *testing.T) {
	e := newLibEnv(t)
	now := time.Now()
	video := "https://www.youtube.com/watch?v=same"
	a := e.add(t, e.alex, video, "users/alex/youtube", "a.mp4", false)
	e.write(t, "users/alex/youtube/a.mp4", "alice")
	b, err := e.st.CreateDownload(Download{UserID: e.bob.ID, MeTubeURL: video, Site: "youtube", Target: "original", Folder: "users/bob/youtube", Status: "downloading", QueuedAt: now.Add(-time.Minute), MeTubeTS: now.Add(-time.Minute).UnixNano()})
	if err != nil {
		t.Fatal(err)
	}
	e.write(t, "users/bob/youtube/b.mp4", "bob")
	e.mt.h.Done = []HistoryItem{{URL: video, Folder: b.Folder, Timestamp: b.MeTubeTS, Filename: "b.mp4", Status: "finished"}}
	mt := e.mt
	if err := e.lib.Delete(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	tr := &Tracker{Store: e.st, MT: mt, SharesDir: e.shares, Conv: e.conv, Now: func() time.Time { return now.Add(11 * time.Minute) }}
	if err := tr.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := e.st.GetDownload(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "finished" {
		t.Fatalf("Bob's file exists, but deleting Alice's record changed Bob to %q (%s)", got.Status, got.Error)
	}
}

func TestFreeNameHandlesLongCollisions(t *testing.T) {
	if dir := os.Getenv("GRAB_TEST_COLLISION_DIR"); dir != "" {
		original := os.Getenv("GRAB_TEST_COLLISION_NAME")
		for i := 0; i < 2; i++ {
			name, err := freeName(dir, original, "alice")
			if err != nil {
				t.Fatal(err)
			}
			if len(name) > 255 || !utf8.ValidString(name) || name == original {
				t.Fatalf("invalid collision name (%d bytes): %q", len(name), name)
			}
			if _, err := os.Lstat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("candidate already exists: %v", err)
			}
			if err := os.WriteFile(filepath.Join(dir, name), []byte("new"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	for _, tc := range []struct{ label, name string }{
		{"ascii", strings.Repeat("v", 246) + ".mp4"},
		{"utf8", strings.Repeat("界", 82) + ".mp4"},
	} {
		t.Run(tc.label, func(t *testing.T) {
			name := tc.name
			dir := t.TempDir()
			original := filepath.Join(dir, name)
			if err := os.WriteFile(original, []byte("original"), 0o600); err != nil {
				t.Fatal(err)
			}
			bin, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, bin, "-test.run=^TestFreeNameHandlesLongCollisions$")
			cmd.Env = append(os.Environ(), "GRAB_TEST_COLLISION_DIR="+dir, "GRAB_TEST_COLLISION_NAME="+name)
			out, err := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatal("freeName did not return on a long collision")
			}
			if err != nil {
				t.Fatalf("collision helper: %v: %s", err, out)
			}
			data, err := os.ReadFile(original)
			if err != nil || string(data) != "original" {
				t.Fatal("collision overwrote original")
			}
		})
	}
}

func TestFreeNamePropagatesFilesystemErrors(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := freeName(file, "video.mp4", "alice"); !errors.Is(err, syscall.ENOTDIR) {
		t.Fatalf("want ENOTDIR, got %v", err)
	}
}

func TestTrackerNotificationReportsConversionOutcome(t *testing.T) {
	for _, tc := range []struct {
		name    string
		target  string
		probe   Probe
		job     Job
		cache   bool
		ready   bool
		pending bool
		reason  string
	}{
		{name: "failed job", target: "discord", job: Job{State: JobFailed, Error: "encoder broke"}, reason: "encoder broke"},
		{name: "probe failure", target: "discord", reason: "read the downloaded video"},
		{name: "too long", target: "discord", probe: Probe{Duration: 3600, Size: 1_000_000_000, Height: 1080, VCodec: "vp9"}, reason: "too long"},
		{name: "unknown duration", target: "discord", probe: Probe{Size: 1_000_000_000, Height: 1080, VCodec: "vp9"}, reason: "duration unknown"},
		{name: "ready source", target: "discord", probe: Probe{Duration: 60, Size: 1_000_000, Height: 480, VCodec: "h264", ACodec: "aac", Container: "mp4"}, ready: true},
		{name: "converted cache", target: "discord", cache: true, ready: true},
		{name: "download only", target: "original", ready: true},
		{name: "conversion queued", target: "discord", probe: Probe{Duration: 60, Size: 20_000_000, Height: 1080, VCodec: "vp9"}, pending: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newLibEnv(t)
			src := e.write(t, "users/alex/youtube/a.mp4", "video")
			d := e.add(t, e.alex, "https://youtu.be/a", "users/alex/youtube", "a.mp4", false)
			d, err := e.st.UpdateDownload(d.ID, func(d *Download) error { d.Target = tc.target; d.Title = "A video"; return nil })
			if err != nil {
				t.Fatal(err)
			}
			prober := fakeProber{}
			if tc.probe != (Probe{}) {
				prober[src] = tc.probe
			}
			if tc.job.State != "" {
				e.conv.jobs[d.ID+"-discord"] = tc.job
			}
			if tc.cache {
				if err := os.WriteFile(e.conv.CachePath(d.ID, "discord"), []byte("converted"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			sender := &fakeSender{}
			base, _ := url.Parse("https://grab.example.com")
			n := &Notifier{Store: e.st, Sender: sender, BaseURL: base, Mode: "none", InstanceURLs: []string{"json://example.com"}}
			tr := &Tracker{Store: e.st, SharesDir: e.shares, Conv: e.conv, Prober: prober, Notify: n.Notify}
			if err := tr.finishUp(context.Background()); err != nil {
				t.Fatal(err)
			}
			got, err := e.st.GetDownload(d.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Status != "finished" || !exists(src) {
				t.Fatal("conversion outcome changed the downloaded original")
			}
			if tc.pending {
				if len(sender.got) != 0 || !got.NotifiedAt.IsZero() {
					t.Fatal("notified before conversion finished")
				}
				return
			}
			if len(sender.got) != 1 || got.NotifiedAt.IsZero() {
				t.Fatalf("notification missing: %+v", sender.got)
			}
			msg := sender.got[0]
			if tc.ready {
				if !strings.HasPrefix(msg.title, "Ready:") {
					t.Fatalf("ready download: %+v", msg)
				}
			} else {
				if strings.HasPrefix(msg.title, "Ready:") || !strings.Contains(msg.body, "Discord") || !strings.Contains(msg.body, tc.reason) || !strings.Contains(msg.body, "original download is still available") {
					t.Fatalf("missing target conversion failure: %+v", msg)
				}
			}
			if !strings.Contains(msg.body, "/?item="+d.ID) {
				t.Fatalf("missing item link: %+v", msg)
			}
		})
	}
}

func TestDeleteStopsActiveConversion(t *testing.T) {
	e := newLibEnv(t)
	d := e.add(t, e.alex, "https://youtu.be/a", "users/alex/youtube", "a.mp4", false)
	src := e.write(t, "users/alex/youtube/a.mp4", "video")
	dir := t.TempDir()
	bin := filepath.Join(dir, "ffmpeg")
	started, release := filepath.Join(dir, "started"), filepath.Join(dir, "release")
	script := "#!/bin/sh\nfor arg do out=\"$arg\"; done\nprintf encoded > \"$out\"\ntouch '" + started + "'\nwhile [ ! -f '" + release + "' ]; do sleep 0.01; done\n"
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	conv, err := NewConverter(bin, EncX264, "", filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	e.lib.Conv = conv
	pr, _ := PresetByName("telegram")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conv.Enqueue(d.ID, pr, src, Probe{}, Decision{Action: ActionTranscode})
	done := make(chan struct{})
	go func() {
		defer close(done)
		conv.Run(ctx)
	}()
	for !exists(started) {
		if ctx.Err() != nil {
			t.Fatal("conversion did not start")
		}
		time.Sleep(time.Millisecond)
	}
	if err := e.lib.Delete(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.GetDownload(d.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("record not deleted")
	}
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	waitJob(t, conv, d.ID, pr.Name)
	cancel()
	<-done
	for _, suffix := range []string{"", ".tmp"} {
		if exists(conv.CachePath(d.ID, pr.Name) + suffix) {
			t.Fatal("converter left a file after Delete removed the source and record")
		}
	}
}

// An alias keeps the source available, so an uncancelled job would succeed.
// The second item also proves deletion does not stop unrelated conversions.
func TestDeletePreventsQueuedAndStaleConversions(t *testing.T) {
	for _, queued := range []bool{true, false} {
		t.Run(map[bool]string{true: "queued", false: "stale request"}[queued], func(t *testing.T) {
			e := newLibEnv(t)
			src := e.write(t, "users/alex/youtube/a.mp4", "video")
			d := e.add(t, e.alex, "https://youtu.be/a", "users/alex/youtube", "a.mp4", false)
			alias := e.add(t, e.alex, "https://www.youtube.com/watch?v=a", d.Folder, d.Filename, false)
			dir := t.TempDir()
			bin := filepath.Join(dir, "ffmpeg")
			if err := os.WriteFile(bin, []byte("#!/bin/sh\nfor arg do out=\"$arg\"; done\nprintf encoded > \"$out\"\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			conv, err := NewConverter(bin, EncX264, "", filepath.Join(dir, "cache"))
			if err != nil {
				t.Fatal(err)
			}
			e.lib.Conv = conv
			pr := mustPreset(t, "telegram")
			if queued {
				conv.Enqueue(d.ID, pr, src, Probe{}, Decision{Action: ActionTranscode})
			}
			if err := e.lib.Delete(context.Background(), d); err != nil {
				t.Fatal(err)
			}
			if !queued {
				conv.Enqueue(d.ID, pr, src, Probe{}, Decision{Action: ActionTranscode})
			}
			conv.Enqueue(alias.ID, pr, src, Probe{}, Decision{Action: ActionTranscode})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			go func() { defer close(done); conv.Run(ctx) }()
			if job := waitJob(t, conv, alias.ID, pr.Name); job.State != JobDone {
				t.Fatalf("surviving item's conversion failed: %+v", job)
			}
			cancel()
			<-done
			for _, suffix := range []string{"", ".tmp"} {
				if exists(conv.CachePath(d.ID, pr.Name) + suffix) {
					t.Fatal("deleted item's queued/stale conversion left a file")
				}
			}
		})
	}
}
