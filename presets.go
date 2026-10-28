package main

import (
	"errors"
	"math"
)

// Preset is one "send to" target. Each has its OWN limits, so changing one
// never moves another (Signal and Messages used to match; they no longer do).
// Bytes are decimal: always at or under each app's real limit.
type Preset struct {
	Name      string `json:"name"`
	Label     string `json:"label"`
	MaxBytes  int64  `json:"maxBytes"`  // 0 = no size limit
	MaxHeight int    `json:"maxHeight"` // "720p": caps the SHORT side, so vertical video keeps it; 0 = source
}

var Presets = []Preset{
	{Name: "original", Label: "Original"},
	{Name: "telegram", Label: "Telegram", MaxBytes: 1_900_000_000, MaxHeight: 1080}, // 2 GB limit
	{Name: "signal", Label: "Signal", MaxBytes: 190_000_000, MaxHeight: 1080},       // 200 MB limit
	{Name: "messages", Label: "Messages", MaxBytes: 95_000_000, MaxHeight: 720},     // RCS ~100 MB
	{Name: "discord", Label: "Discord", MaxBytes: 10_000_000, MaxHeight: 480},       // free tier
}

func PresetByName(name string) (Preset, bool) {
	for _, p := range Presets {
		if p.Name == name {
			return p, true
		}
	}
	return Preset{}, false
}

const (
	audioKbps    = 128
	minVideoKbps = 300 // below this the result is mush: refuse instead
	sizeHeadroom = 0.97
	qualityQP    = 23 // constant-quality level when there's no size target
)

type Action string

const (
	ActionReady     Action = "ready"     // serve the source file untouched
	ActionRemux     Action = "remux"     // right codecs, wrong container: -c copy
	ActionTranscode Action = "transcode" // re-encode to H.264/AAC
	ActionTooLong   Action = "too_long"  // can't fit the size limit at a usable bitrate
)

type Decision struct {
	Action      Action
	VideoKbps   int     // transcode: target video bitrate; 0 = constant quality
	Height      int     // transcode: output height for landscape; 0 = keep source
	Width       int     // transcode: output width for portrait; 0 = keep source
	EstBytes    int64   // expected output size
	MaxDuration float64 // too_long: longest duration (s) this preset can carry
}

var ErrNoDuration = errors.New("duration unknown: can't fit a size limit")

func Decide(p Probe, pr Preset) (Decision, error) {
	fitsSize := pr.MaxBytes == 0 || p.Size <= pr.MaxBytes
	// The cap is on the short side: most shared clips are vertical, and
	// "720p" for a 1080x1920 Short means 720x1280, not 406x720.
	short, portrait := p.Height, p.Width > 0 && p.Width < p.Height
	if portrait {
		short = p.Width
	}
	fitsHeight := pr.MaxHeight == 0 || short <= pr.MaxHeight
	if p.Compatible() && fitsSize && fitsHeight {
		if p.IsMP4() {
			return Decision{Action: ActionReady, EstBytes: p.Size}, nil
		}
		return Decision{Action: ActionRemux, EstBytes: p.Size}, nil
	}
	var height, width int
	outShort := short
	if pr.MaxHeight > 0 && short > pr.MaxHeight {
		outShort = pr.MaxHeight
		if portrait {
			width = pr.MaxHeight
		} else {
			height = pr.MaxHeight
		}
	}
	if p.Duration <= 0 {
		if pr.MaxBytes == 0 { // can't derive a bitrate: constant quality
			return Decision{Action: ActionTranscode, Height: height, Width: width, EstBytes: p.Size}, nil
		}
		return Decision{}, ErrNoDuration
	}
	// Never ask for much more than the source carries. Without this, VAAPI's
	// constant-QP "Original" turned a 4.9 Mbps 1440x2560 VP9 (63.5 MB) into
	// 17.9 Mbps H.264 (235 MB). 1.5x covers H.264 needing more bits than VP9.
	kbps := max(int(float64(p.Size)*8/1000/p.Duration*1.5), minVideoKbps)
	kbps = min(kbps, ceilingKbps(outShort))
	if pr.MaxBytes > 0 {
		budget := float64(pr.MaxBytes) * sizeHeadroom * 8 / 1000 // kilobits
		fit := int(budget/p.Duration) - audioKbps
		if fit < minVideoKbps {
			return Decision{Action: ActionTooLong, MaxDuration: math.Floor(budget / float64(minVideoKbps+audioKbps))}, nil
		}
		kbps = min(kbps, fit)
	}
	est := int64(float64(kbps+audioKbps) * 1000 / 8 * p.Duration)
	return Decision{Action: ActionTranscode, VideoKbps: kbps, Height: height, Width: width, EstBytes: est}, nil
}

func ceilingKbps(short int) int {
	switch {
	case short <= 480:
		return 2500
	case short <= 720:
		return 5000
	case short <= 1080:
		return 8000
	default:
		return 16000
	}
}
