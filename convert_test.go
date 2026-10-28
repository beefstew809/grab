package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestFFmpegArgsVAAPI(t *testing.T) {
	a := ffmpegArgs(EncVAAPI, "/dev/dri/renderD128", "in.webm", "out.mp4.tmp",
		Decision{Action: ActionTranscode, VideoKbps: 1100, Height: 720})
	s := strings.Join(a, " ")
	for _, want := range []string{"-vaapi_device /dev/dri/renderD128", "-vf scale=-2:720,format=nv12,hwupload",
		"-c:v h264_vaapi", "-b:v 1100k", "-maxrate 1100k", "-bufsize 2200k", "-c:a aac", "-b:a 128k",
		"-movflags +faststart", "-f mp4 out.mp4.tmp", "-progress pipe:1"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in: %s", want, s)
		}
	}
}

func TestFFmpegArgsQualityMode(t *testing.T) {
	v := strings.Join(ffmpegArgs(EncVAAPI, "/dev/dri/renderD128", "i", "o", Decision{Action: ActionTranscode}), " ")
	if !strings.Contains(v, "-qp 23") || !strings.Contains(v, "scale=trunc(iw/2)*2:trunc(ih/2)*2,format=nv12") || strings.Contains(v, "-b:v") {
		t.Errorf("vaapi quality mode: %s", v)
	}
	x := strings.Join(ffmpegArgs(EncX264, "", "i", "o", Decision{Action: ActionTranscode}), " ")
	if !strings.Contains(x, "-c:v libx264") || !strings.Contains(x, "-crf 23") || strings.Contains(x, "vaapi") {
		t.Errorf("x264 quality mode: %s", x)
	}
}

func TestFFmpegArgsRemux(t *testing.T) {
	a := ffmpegArgs(EncVAAPI, "/dev/dri/renderD128", "in.mkv", "out.tmp", Decision{Action: ActionRemux})
	s := strings.Join(a, " ")
	if !strings.Contains(s, "-c copy") || strings.Contains(s, "vaapi") || !slices.Contains(a, "+faststart") {
		t.Errorf("remux: %s", s)
	}
}

func newTestConverter(t *testing.T) *Converter {
	t.Helper()
	requireFFmpeg(t)
	c, err := NewConverter("ffmpeg", EncX264, "", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go c.Run(ctx)
	return c
}

func waitJob(t *testing.T, c *Converter, id, preset string) Job {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if j, ok := c.Status(id, preset); ok && (j.State == JobDone || j.State == JobFailed) {
			return j
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("job did not finish")
	return Job{}
}

// moovBeforeMdat walks top-level MP4 boxes: faststart means moov comes first.
func moovBeforeMdat(t *testing.T, path string) bool {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for off := 0; off+8 <= len(b); {
		size := int(uint32(b[off])<<24 | uint32(b[off+1])<<16 | uint32(b[off+2])<<8 | uint32(b[off+3]))
		switch typ := string(b[off+4 : off+8]); typ {
		case "moov":
			return true
		case "mdat":
			return false
		}
		if size < 8 {
			return false
		}
		off += size
	}
	return false
}

func convertAndCheck(t *testing.T, src string, pr Preset) (Probe, string) {
	t.Helper()
	c := newTestConverter(t)
	fp := NewFFprobe("ffprobe")
	in, err := fp.Probe(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	d, err := Decide(in, pr)
	if err != nil {
		t.Fatal(err)
	}
	c.Enqueue("item1", pr, src, in, d)
	if j := waitJob(t, c, "item1", pr.Name); j.State != JobDone {
		t.Fatalf("job: %+v", j)
	}
	out := c.CachePath("item1", pr.Name)
	got, err := fp.Probe(context.Background(), out)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Compatible() || !got.IsMP4() {
		t.Errorf("output not H.264/AAC MP4: %+v", got)
	}
	if !moovBeforeMdat(t, out) {
		t.Error("output is not faststart")
	}
	return got, out
}

func TestConvertWebmOriginal(t *testing.T) {
	got, _ := convertAndCheck(t, fixture(t, "vp9.webm"), mustPreset(t, "original"))
	if got.Height != 720 {
		t.Errorf("original must keep height, got %d", got.Height)
	}
}

func TestConvertRemuxMKV(t *testing.T) {
	convertAndCheck(t, fixture(t, "h264.mkv"), mustPreset(t, "original"))
}

func TestConvertFitsSizeAndHeight(t *testing.T) {
	pr := Preset{Name: "tiny", Label: "Tiny", MaxBytes: 300_000, MaxHeight: 480}
	got, out := convertAndCheck(t, fixture(t, "vp9.webm"), pr)
	st, _ := os.Stat(out)
	if st.Size() > pr.MaxBytes || got.Height != 480 {
		t.Errorf("size %d height %d", st.Size(), got.Height)
	}
}

func TestConvertFailureReported(t *testing.T) {
	c := newTestConverter(t)
	c.Enqueue("bad", mustPreset(t, "original"), "/nonexistent.webm", Probe{Duration: 4}, Decision{Action: ActionTranscode})
	j := waitJob(t, c, "bad", "original")
	if j.State != JobFailed || j.Error == "" {
		t.Fatalf("got %+v", j)
	}
	if _, err := os.Stat(c.CachePath("bad", "original")); err == nil {
		t.Error("failed job left a cache file")
	}
}

func TestEnqueueIdempotent(t *testing.T) {
	c, err := NewConverter("ffmpeg", EncX264, "", t.TempDir()) // worker NOT started
	if err != nil {
		t.Fatal(err)
	}
	pr := mustPreset(t, "discord")
	c.Enqueue("a", pr, "x", Probe{}, Decision{Action: ActionTranscode})
	c.Enqueue("a", pr, "x", Probe{}, Decision{Action: ActionTranscode})
	if len(c.reqs) != 1 {
		t.Fatalf("queued %d requests, want 1", len(c.reqs))
	}
	if j, _ := c.Status("a", "discord"); j.State != JobQueued {
		t.Fatalf("got %+v", j)
	}
}

func TestNewConverterRemovesTmpAndSweepExpires(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a-original.mp4.tmp"), []byte("half"), 0o644)
	c, err := NewConverter("ffmpeg", EncX264, "", dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "a-original.mp4.tmp")); err == nil {
		t.Error("tmp survived startup")
	}
	now := time.Now()
	old, fresh := c.CachePath("old", "original"), c.CachePath("fresh", "original")
	os.WriteFile(old, []byte("o"), 0o644)
	os.WriteFile(fresh, []byte("f"), 0o644)
	os.Chtimes(old, now.Add(-8*24*time.Hour), now.Add(-8*24*time.Hour))
	n, err := c.Sweep(7*24*time.Hour, now)
	if err != nil || n != 1 {
		t.Fatalf("swept %d, %v", n, err)
	}
	if _, err := os.Stat(old); err == nil {
		t.Error("old file survived")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("fresh file removed")
	}
}

func TestTailBufferKeepsEnd(t *testing.T) {
	var tb tailBuffer
	tb.Write(bytes.Repeat([]byte("a"), 5000))
	tb.Write([]byte("THE END"))
	if s := tb.String(); len(s) > tailMax || !strings.HasSuffix(s, "THE END") {
		t.Fatalf("len %d", len(s))
	}
}

// Review #1: after Sweep removes a cached file, Convert must queue a new job
// instead of returning the stale "done" one forever.
func TestEnqueueRequeuesSweptDoneJob(t *testing.T) {
	c, err := NewConverter("ffmpeg", EncX264, "", t.TempDir()) // worker NOT started
	if err != nil {
		t.Fatal(err)
	}
	pr := mustPreset(t, "discord")
	c.mu.Lock()
	c.jobs[jobKey("a", "discord")] = &Job{State: JobDone, Percent: 100} // file already swept
	c.mu.Unlock()
	j := c.Enqueue("a", pr, "x", Probe{}, Decision{Action: ActionTranscode})
	if j.State != JobQueued || len(c.reqs) != 1 {
		t.Fatalf("got %+v, reqs=%d", j, len(c.reqs))
	}
}

// Review (minor, re-graded): with no scaling requested, an odd-width source
// must still convert -- libx264/h264_vaapi in 4:2:0 need even dimensions.
func TestConvertOddWidth(t *testing.T) {
	got, _ := convertAndCheck(t, fixture(t, "odd.webm"), mustPreset(t, "original"))
	if got.Width%2 != 0 || got.Height != 720 {
		t.Errorf("got %dx%d", got.Width, got.Height)
	}
}

func TestFFmpegArgsPortraitScalesWidth(t *testing.T) {
	s := strings.Join(ffmpegArgs(EncVAAPI, "/dev/dri/renderD128", "i", "o",
		Decision{Action: ActionTranscode, VideoKbps: 5000, Width: 720}), " ")
	if !strings.Contains(s, "-vf scale=720:-2,format=nv12,hwupload") {
		t.Errorf("portrait must scale width: %s", s)
	}
}

func TestConvertPortraitKeepsShortSide(t *testing.T) {
	pr := Preset{Name: "p", Label: "P", MaxBytes: 50_000_000, MaxHeight: 480}
	got, _ := convertAndCheck(t, fixture(t, "portrait.webm"), pr)
	if got.Width != 480 || got.Height != 854 {
		t.Errorf("got %dx%d, want 480x854", got.Width, got.Height)
	}
}
