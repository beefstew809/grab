package main

import (
	"cmp"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

// Clips are generated, not committed: 4 s of testsrc2 720p plus a sine tone.
var (
	fixtureDir  string
	fixtureOnce sync.Once
	fixtureErr  error
)

// requireFFmpeg skips without ffmpeg, unless SHARE_REQUIRE_FFMPEG=1 (the image
// build sets it) -- there a missing ffmpeg must fail, never silently skip.
func requireFFmpeg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		if os.Getenv("SHARE_REQUIRE_FFMPEG") == "1" {
			t.Fatal("ffmpeg required but not on PATH")
		}
		t.Skip("ffmpeg not on PATH")
	}
}

var fixtureArgs = map[string][]string{
	"h264.mp4": {"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-c:a", "aac"},
	"h264.mkv": {"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-c:a", "aac"},
	"vp9.webm": {"-c:v", "libvpx-vp9", "-deadline", "realtime", "-cpu-used", "8", "-b:v", "1500k", "-c:a", "libopus"},
	// Odd width, as some TikTok/Reddit sources have: yuv420p H.264 needs even.
	"portrait.webm": {"-c:v", "libvpx-vp9", "-deadline", "realtime", "-cpu-used", "8", "-b:v", "800k", "-c:a", "libopus"},
	"odd.webm":      {"-c:v", "libvpx-vp9", "-deadline", "realtime", "-cpu-used", "8", "-b:v", "800k", "-c:a", "libopus"},
}

// fixtureSrc overrides the lavfi source. testsrc2 rounds yuv420p widths to
// even, so the odd fixture goes through 4:4:4 before cropping to 405.
var fixtureSrc = map[string]string{
	"odd.webm":      "testsrc2=size=406x720:rate=30:duration=4,format=yuv444p,crop=405:720",
	"portrait.webm": "testsrc2=size=720x1280:rate=30:duration=4", // a vertical Short
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	requireFFmpeg(t)
	fixtureOnce.Do(func() {
		fixtureDir, fixtureErr = os.MkdirTemp("", "share-fixtures")
		if fixtureErr != nil {
			return
		}
		for n, codec := range fixtureArgs {
			args := []string{"-hide_banner", "-loglevel", "error", "-y",
				"-f", "lavfi", "-i", cmp.Or(fixtureSrc[n], "testsrc2=size=1280x720:rate=30:duration=4"),
				"-f", "lavfi", "-i", "sine=frequency=440:duration=4"}
			args = append(args, codec...)
			args = append(args, "-shortest", filepath.Join(fixtureDir, n))
			if out, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
				fixtureErr = &fixtureError{n, string(out)}
				return
			}
		}
	})
	if fixtureErr != nil {
		t.Fatal(fixtureErr)
	}
	return filepath.Join(fixtureDir, name)
}

type fixtureError struct{ name, out string }

func (e *fixtureError) Error() string { return "fixture " + e.name + ": " + e.out }
