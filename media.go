package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Probe is what ffprobe says about a file: enough to decide whether it can be
// shared as-is and how to convert it if not.
type Probe struct {
	Duration  float64 `json:"duration"`
	Size      int64   `json:"size"`
	Width     int     `json:"width"`
	Height    int     `json:"height"`
	VCodec    string  `json:"vcodec"`
	ACodec    string  `json:"acodec"`
	Container string  `json:"container"` // ffprobe format_name, e.g. "mov,mp4,m4a,3gp,3g2,mj2"
}

// Compatible means it plays everywhere once in MP4: H.264 video, AAC or no audio.
func (p Probe) Compatible() bool {
	return p.VCodec == "h264" && (p.ACodec == "aac" || p.ACodec == "")
}

func (p Probe) IsMP4() bool { return strings.Contains(p.Container, "mp4") }

type ffprobeOut struct {
	Format struct {
		Duration   string `json:"duration"`
		Size       string `json:"size"`
		FormatName string `json:"format_name"`
	} `json:"format"`
	Streams []struct {
		CodecType string `json:"codec_type"`
		CodecName string `json:"codec_name"`
		Width     int    `json:"width"`
		Height    int    `json:"height"`
	} `json:"streams"`
}

// parseFFprobe takes the FIRST video and FIRST audio stream, matching the
// -map 0:v:0 / 0:a:0 the converter uses.
func parseFFprobe(b []byte) (Probe, error) {
	var o ffprobeOut
	if err := json.Unmarshal(b, &o); err != nil {
		return Probe{}, fmt.Errorf("ffprobe output: %w", err)
	}
	p := Probe{Container: o.Format.FormatName}
	p.Duration, _ = strconv.ParseFloat(o.Format.Duration, 64)
	p.Size, _ = strconv.ParseInt(o.Format.Size, 10, 64)
	for _, s := range o.Streams {
		switch {
		case s.CodecType == "video" && p.VCodec == "":
			p.VCodec, p.Width, p.Height = s.CodecName, s.Width, s.Height
		case s.CodecType == "audio" && p.ACodec == "":
			p.ACodec = s.CodecName
		}
	}
	return p, nil
}

// FFprobe memoises by path, keyed on mtime+size so a replaced file is re-probed.
type FFprobe struct {
	bin  string
	mu   sync.Mutex
	memo map[string]memoEntry
}

type memoEntry struct {
	mod  time.Time
	size int64
	p    Probe
}

func NewFFprobe(bin string) *FFprobe { return &FFprobe{bin: bin, memo: map[string]memoEntry{}} }

func (f *FFprobe) Probe(ctx context.Context, path string) (Probe, error) {
	st, err := os.Stat(path)
	if err != nil {
		return Probe{}, err
	}
	f.mu.Lock()
	m, ok := f.memo[path]
	f.mu.Unlock()
	if ok && m.mod.Equal(st.ModTime()) && m.size == st.Size() {
		return m.p, nil
	}
	out, err := exec.CommandContext(ctx, f.bin, "-v", "error",
		"-show_entries", "format=duration,size,format_name:stream=codec_type,codec_name,width,height",
		"-of", "json", path).Output()
	if err != nil {
		return Probe{}, fmt.Errorf("ffprobe: %w", err)
	}
	p, err := parseFFprobe(out)
	if err != nil {
		return Probe{}, err
	}
	f.mu.Lock()
	f.memo[path] = memoEntry{st.ModTime(), st.Size(), p}
	f.mu.Unlock()
	return p, nil
}
