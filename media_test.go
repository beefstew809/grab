package main

import (
	"context"
	"testing"
)

func TestParseFFprobe(t *testing.T) {
	js := `{"streams":[{"codec_type":"video","codec_name":"vp9","width":1920,"height":1080},
	                   {"codec_type":"audio","codec_name":"opus"},
	                   {"codec_type":"audio","codec_name":"aac"}],
	        "format":{"duration":"61.500000","size":"52428800","format_name":"matroska,webm"}}`
	p, err := parseFFprobe([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	want := Probe{Duration: 61.5, Size: 52428800, Width: 1920, Height: 1080, VCodec: "vp9", ACodec: "opus", Container: "matroska,webm"}
	if p != want {
		t.Fatalf("got %+v want %+v", p, want)
	}
	if p.Compatible() || p.IsMP4() {
		t.Fatal("vp9 webm must be neither compatible nor mp4")
	}
}

func TestCompatible(t *testing.T) {
	cases := []struct {
		p    Probe
		want bool
	}{
		{Probe{VCodec: "h264", ACodec: "aac"}, true},
		{Probe{VCodec: "h264"}, true}, // silent clip
		{Probe{VCodec: "h264", ACodec: "opus"}, false},
		{Probe{VCodec: "av1", ACodec: "aac"}, false},
	}
	for _, c := range cases {
		if got := c.p.Compatible(); got != c.want {
			t.Errorf("%+v: got %v", c.p, got)
		}
	}
}

func TestParseFFprobeRejectsGarbage(t *testing.T) {
	if _, err := parseFFprobe([]byte("not json")); err == nil {
		t.Fatal("want error")
	}
}

func TestFFprobeRealFiles(t *testing.T) {
	f := NewFFprobe("ffprobe")
	for name, want := range map[string]Probe{
		"h264.mp4": {VCodec: "h264", ACodec: "aac", Height: 720},
		"h264.mkv": {VCodec: "h264", ACodec: "aac", Height: 720},
		"vp9.webm": {VCodec: "vp9", ACodec: "opus", Height: 720},
	} {
		p, err := f.Probe(context.Background(), fixture(t, name))
		if err != nil {
			t.Fatal(name, err)
		}
		if p.VCodec != want.VCodec || p.ACodec != want.ACodec || p.Height != want.Height || p.Duration < 3.5 || p.Size == 0 {
			t.Errorf("%s: got %+v", name, p)
		}
		if (name == "h264.mp4") != p.IsMP4() {
			t.Errorf("%s: IsMP4=%v container=%q", name, p.IsMP4(), p.Container)
		}
	}
}
