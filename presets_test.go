package main

import (
	"errors"
	"testing"
)

func mustPreset(t *testing.T, name string) Preset {
	t.Helper()
	p, ok := PresetByName(name)
	if !ok {
		t.Fatalf("no preset %q", name)
	}
	return p
}

var (
	h264mp4 = "mov,mp4,m4a,3gp,3g2,mj2"
	mkv     = "matroska,webm"
)

func TestDecide(t *testing.T) {
	cases := []struct {
		name   string
		probe  Probe
		preset string
		want   Decision
	}{
		{"compatible mp4 that fits is served as-is",
			Probe{VCodec: "h264", ACodec: "aac", Container: h264mp4, Height: 720, Size: 20e6, Duration: 60}, "messages",
			Decision{Action: ActionReady, EstBytes: 20e6}},
		{"compatible but in mkv is remuxed",
			Probe{VCodec: "h264", ACodec: "aac", Container: mkv, Height: 720, Size: 20e6, Duration: 60}, "messages",
			Decision{Action: ActionRemux, EstBytes: 20e6}},
		{"webm for original: 1.5x the source bitrate, capped by the 1080 ceiling",
			Probe{VCodec: "vp9", ACodec: "opus", Container: mkv, Height: 1080, Size: 50e6, Duration: 60}, "original",
			Decision{Action: ActionTranscode, VideoKbps: 8000, EstBytes: 60_960_000}},
		{"jef.paints: 1440x2560 VP9 at 4.9 Mbps must not become a 235 MB original (VAAPI constant-QP did 17.9 Mbps)",
			Probe{VCodec: "vp9", ACodec: "aac", Container: h264mp4, Width: 1440, Height: 2560, Size: 63_500_125, Duration: 104.375}, "original",
			Decision{Action: ActionTranscode, VideoKbps: 7300, EstBytes: 96_912_187}},
		{"small efficient vertical source is not inflated for messages",
			Probe{VCodec: "vp9", ACodec: "opus", Container: mkv, Width: 720, Height: 1280, Size: 1.4e6, Duration: 4}, "messages",
			Decision{Action: ActionTranscode, VideoKbps: 4200, EstBytes: 2_164_000}},
		{"original with unknown duration falls back to constant quality",
			Probe{VCodec: "vp9", ACodec: "opus", Container: mkv, Height: 1080, Size: 50e6}, "original",
			Decision{Action: ActionTranscode, EstBytes: 50e6}},
		{"webm for messages: 720p, bitrate capped by the 720p ceiling",
			Probe{VCodec: "vp9", ACodec: "opus", Container: mkv, Height: 1080, Size: 50e6, Duration: 60}, "messages",
			Decision{Action: ActionTranscode, VideoKbps: 5000, Height: 720, EstBytes: 38_460_000}},
		{"long h264 squeezed into messages",
			Probe{VCodec: "h264", ACodec: "aac", Container: h264mp4, Height: 1080, Size: 300e6, Duration: 600}, "messages",
			Decision{Action: ActionTranscode, VideoKbps: 1100, Height: 720, EstBytes: 92_100_000}},
		{"ten minutes cannot fit discord",
			Probe{VCodec: "h264", ACodec: "aac", Container: h264mp4, Height: 1080, Size: 300e6, Duration: 600}, "discord",
			Decision{Action: ActionTooLong, MaxDuration: 181}},
		{"vertical 1080x1920 Short for messages caps the SHORT side: 720 wide, not 406",
			Probe{VCodec: "vp9", ACodec: "opus", Container: mkv, Width: 1080, Height: 1920, Size: 30e6, Duration: 20}, "messages",
			Decision{Action: ActionTranscode, VideoKbps: 5000, Width: 720, EstBytes: 12_820_000}},
		{"vertical 720x1280 h264 mp4 already fits messages (short side 720)",
			Probe{VCodec: "h264", ACodec: "aac", Container: h264mp4, Width: 720, Height: 1280, Size: 20e6, Duration: 60}, "messages",
			Decision{Action: ActionReady, EstBytes: 20e6}},
		{"small 480p clip is ready for discord",
			Probe{VCodec: "h264", ACodec: "aac", Container: h264mp4, Height: 480, Size: 5e6, Duration: 60}, "discord",
			Decision{Action: ActionReady, EstBytes: 5e6}},
	}
	for _, c := range cases {
		got, err := Decide(c.probe, mustPreset(t, c.preset))
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s:\n got %+v\nwant %+v", c.name, got, c.want)
		}
	}
}

func TestDecideNeedsDurationForSizeTargets(t *testing.T) {
	_, err := Decide(Probe{VCodec: "vp9", Container: mkv, Height: 720, Size: 50e6}, mustPreset(t, "discord"))
	if !errors.Is(err, ErrNoDuration) {
		t.Fatalf("got %v", err)
	}
}

func TestPresetTable(t *testing.T) {
	want := map[string]Preset{
		"original": {Name: "original", Label: "Original"},
		"telegram": {Name: "telegram", Label: "Telegram", MaxBytes: 1_900_000_000, MaxHeight: 1080},
		"signal":   {Name: "signal", Label: "Signal", MaxBytes: 190_000_000, MaxHeight: 1080},
		"messages": {Name: "messages", Label: "Messages", MaxBytes: 95_000_000, MaxHeight: 720},
		"discord":  {Name: "discord", Label: "Discord", MaxBytes: 10_000_000, MaxHeight: 480},
	}
	if len(Presets) != len(want) {
		t.Fatalf("got %d presets", len(Presets))
	}
	for _, p := range Presets {
		if want[p.Name] != p {
			t.Errorf("preset %s = %+v", p.Name, p)
		}
	}
	if _, ok := PresetByName("nope"); ok {
		t.Error("unknown preset found")
	}
}
