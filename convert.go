package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Encoder string

const (
	EncVAAPI Encoder = "vaapi"   // hardware encoding on an Intel iGPU; opt in with ENCODER=vaapi
	EncX264  Encoder = "libx264" // software encoding; the default, works anywhere
)

func joinFilters(fs ...string) string {
	var out []string
	for _, f := range fs {
		if f != "" {
			out = append(out, f)
		}
	}
	return strings.Join(out, ",")
}

// ffmpegArgs builds the command. `out` ends in .tmp, so the format is forced
// with -f mp4. Scaling happens in software before hwupload: simpler and
// reliable across scale_vaapi versions; decode is software anyway (no VP9/AV1
// hardware decode on Skylake).
func ffmpegArgs(enc Encoder, device, in, out string, d Decision) []string {
	args := []string{"-hide_banner", "-nostdin", "-y", "-loglevel", "error", "-progress", "pipe:1", "-nostats"}
	if d.Action == ActionRemux {
		return append(args, "-i", in, "-map", "0:v:0", "-map", "0:a:0?", "-c", "copy",
			"-movflags", "+faststart", "-f", "mp4", out)
	}
	if enc == EncVAAPI {
		args = append(args, "-vaapi_device", device)
	}
	args = append(args, "-i", in, "-map", "0:v:0", "-map", "0:a:0?")
	// 4:2:0 H.264 needs even dimensions. scale=-2 gives that when resizing;
	// otherwise round down to even, or odd-width sources (some TikTok/Reddit)
	// fail outright with "width not divisible by 2".
	scale := "scale=trunc(iw/2)*2:trunc(ih/2)*2"
	switch {
	case d.Width > 0: // portrait: cap the short side, which is the width
		scale = fmt.Sprintf("scale=%d:-2", d.Width)
	case d.Height > 0:
		scale = fmt.Sprintf("scale=-2:%d", d.Height)
	}
	if enc == EncVAAPI {
		args = append(args, "-vf", joinFilters(scale, "format=nv12", "hwupload"), "-c:v", "h264_vaapi")
	} else {
		args = append(args, "-vf", scale, "-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p")
	}
	switch {
	case d.VideoKbps > 0:
		k := d.VideoKbps
		args = append(args, "-b:v", fmt.Sprintf("%dk", k), "-maxrate", fmt.Sprintf("%dk", k), "-bufsize", fmt.Sprintf("%dk", 2*k))
	case enc == EncVAAPI:
		args = append(args, "-qp", strconv.Itoa(qualityQP))
	default:
		args = append(args, "-crf", strconv.Itoa(qualityQP))
	}
	return append(args, "-c:a", "aac", "-b:a", fmt.Sprintf("%dk", audioKbps), "-ac", "2",
		"-movflags", "+faststart", "-f", "mp4", out)
}

type JobState string

const (
	JobQueued     JobState = "queued"
	JobConverting JobState = "converting"
	JobDone       JobState = "done"
	JobFailed     JobState = "failed"
)

type Job struct {
	State   JobState `json:"state"`
	Percent float64  `json:"percent"`
	Error   string   `json:"error,omitempty"`
}

type jobReq struct {
	itemID        string
	key, src, out string
	preset        Preset
	probe         Probe
	d             Decision
}

type activeConversion struct {
	itemID string
	cancel context.CancelFunc
	done   chan struct{}
}

// Converter runs ONE ffmpeg at a time (small hosts have few cores). Jobs live in
// memory: a restart loses in-flight work, and the user just taps again.
type Converter struct {
	ffmpeg, device, cacheDir string
	enc                      Encoder
	mu                       sync.Mutex
	jobs                     map[string]*Job
	reqs                     chan jobReq
	active                   map[string]activeConversion
	// Tombstones reject requests that loaded a download before its deletion.
	// IDs are never reused, and a restart drops those stale callers too.
	deleted map[string]bool
}

func NewConverter(ffmpeg string, enc Encoder, device, cacheDir string) (*Converter, error) {
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return nil, err
	}
	stale, _ := filepath.Glob(filepath.Join(cacheDir, "*.tmp"))
	for _, f := range stale {
		os.Remove(f)
	}
	return &Converter{ffmpeg: ffmpeg, enc: enc, device: device, cacheDir: cacheDir,
		jobs: map[string]*Job{}, reqs: make(chan jobReq, 64),
		active: map[string]activeConversion{}, deleted: map[string]bool{}}, nil
}

func jobKey(itemID, preset string) string { return itemID + "-" + preset }

func (c *Converter) CachePath(itemID, preset string) string {
	return filepath.Join(c.cacheDir, jobKey(itemID, preset)+".mp4")
}

func (c *Converter) Status(itemID, preset string) (Job, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	j, ok := c.jobs[jobKey(itemID, preset)]
	if !ok {
		return Job{}, false
	}
	return *j, true
}

// Enqueue is idempotent: a queued or running job is returned as-is, and so is
// a finished one whose file still exists. A failed job, or a finished one whose
// file the 7-day sweep removed, is queued again.
func (c *Converter) Enqueue(itemID string, pr Preset, src string, p Probe, d Decision) Job {
	key := jobKey(itemID, pr.Name)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.deleted[itemID] {
		return Job{State: JobFailed, Error: "download deleted"}
	}
	if j, ok := c.jobs[key]; ok && j.State != JobFailed {
		if _, err := os.Stat(c.CachePath(itemID, pr.Name)); j.State != JobDone || err == nil {
			return *j
		}
	}
	j := &Job{State: JobQueued}
	c.jobs[key] = j
	select {
	case c.reqs <- jobReq{itemID: itemID, key: key, src: src, out: c.CachePath(itemID, pr.Name), preset: pr, probe: p, d: d}:
	default:
		j.State, j.Error = JobFailed, "conversion queue full; try again shortly"
	}
	return *j
}

// Delete joins any active worker before cleaning its output. Marking the ID
// first closes both races: a queued job cannot start, and a stale prepare or
// tracker request cannot enqueue work after the cleanup.
func (c *Converter) Delete(itemID string) error {
	c.mu.Lock()
	c.deleted[itemID] = true
	var waiting []chan struct{}
	for _, a := range c.active {
		if a.itemID == itemID {
			a.cancel()
			waiting = append(waiting, a.done)
		}
	}
	c.mu.Unlock()
	for _, done := range waiting {
		<-done
	}
	c.mu.Lock()
	for key, j := range c.jobs {
		if strings.HasPrefix(key, itemID+"-") {
			*j = Job{State: JobFailed, Error: "download deleted"}
		}
	}
	c.mu.Unlock()
	var errs []error
	for _, pr := range Presets {
		for _, suffix := range []string{"", ".tmp"} {
			if err := os.Remove(c.CachePath(itemID, pr.Name) + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

func (c *Converter) update(key string, f func(*Job)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if j, ok := c.jobs[key]; ok {
		f(j)
	}
}

func (c *Converter) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case r := <-c.reqs:
			c.process(ctx, r)
		}
	}
}

func (c *Converter) process(ctx context.Context, r jobReq) {
	c.mu.Lock()
	if c.deleted[r.itemID] {
		c.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	c.active[r.key] = activeConversion{itemID: r.itemID, cancel: cancel, done: done}
	c.mu.Unlock()
	defer func() {
		cancel()
		c.mu.Lock()
		delete(c.active, r.key)
		close(done)
		c.mu.Unlock()
	}()
	c.update(r.key, func(j *Job) { j.State, j.Percent = JobConverting, 0 })
	tmp := r.out + ".tmp"
	fail := func(msg string) {
		os.Remove(tmp)
		c.update(r.key, func(j *Job) { j.State, j.Error = JobFailed, msg })
	}
	d := r.d
	for attempt := 0; attempt < 2; attempt++ {
		if err := c.runFFmpeg(ctx, r, d, tmp); err != nil {
			fail(err.Error())
			return
		}
		st, err := os.Stat(tmp)
		if err != nil {
			fail(err.Error())
			return
		}
		if r.preset.MaxBytes == 0 || st.Size() <= r.preset.MaxBytes {
			if err := os.Rename(tmp, r.out); err != nil {
				fail(err.Error())
				return
			}
			c.update(r.key, func(j *Job) { j.State, j.Percent = JobDone, 100 })
			return
		}
		if d.VideoKbps == 0 {
			break
		}
		d.VideoKbps = d.VideoKbps * 9 / 10 // one retry at 90 %
	}
	fail(fmt.Sprintf("output is still over %s's %d-byte limit after a retry", r.preset.Label, r.preset.MaxBytes))
}

func (c *Converter) runFFmpeg(ctx context.Context, r jobReq, d Decision, tmp string) error {
	cmd := exec.CommandContext(ctx, c.ffmpeg, ffmpegArgs(c.enc, c.device, r.src, tmp, d)...)
	var stderr tailBuffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		v, ok := strings.CutPrefix(sc.Text(), "out_time_us=")
		if !ok || r.probe.Duration <= 0 {
			continue
		}
		if us, err := strconv.ParseFloat(v, 64); err == nil {
			pct := min(99, us/1e6/r.probe.Duration*100)
			c.update(r.key, func(j *Job) { j.Percent = pct })
		}
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("ffmpeg: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// Touch marks a cache file as used, which pushes back its expiry.
func (c *Converter) Touch(path string) {
	now := time.Now()
	os.Chtimes(path, now, now)
}

// Sweep deletes cache files unused for maxAge.
func (c *Converter) Sweep(maxAge time.Duration, now time.Time) (int, error) {
	files, err := filepath.Glob(filepath.Join(c.cacheDir, "*.mp4"))
	if err != nil {
		return 0, err
	}
	n := 0
	var errs []error
	for _, f := range files {
		st, err := os.Stat(f)
		if err != nil || now.Sub(st.ModTime()) < maxAge {
			continue
		}
		if err := os.Remove(f); err != nil {
			errs = append(errs, err)
			continue
		}
		n++
	}
	return n, errors.Join(errs...)
}

const tailMax = 2048

// tailBuffer keeps the last tailMax bytes of ffmpeg's stderr for the UI.
type tailBuffer struct{ b []byte }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > tailMax {
		t.b = t.b[len(t.b)-tailMax:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.b) }
