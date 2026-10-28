package main

import "testing"

func TestCleanTitle(t *testing.T) {
	cases := []struct {
		name, url, in, want string
	}{
		// Real titles from yt-dlp 2026.10.21, 2026-10-22.
		{"facebook drops engagement prefix",
			"https://www.facebook.com/watch/?v=2199471810998642",
			"332K views · 8.9K reactions | I Stole These Memes on Reels",
			"I Stole These Memes on Reels"},
		{"facebook prefix with comments and shares",
			"https://m.facebook.com/watch/?v=1&_rdr",
			"1.2M views · 40K reactions · 3K comments | Sunset timelapse",
			"Sunset timelapse"},
		{"facebook title without prefix is untouched",
			"https://www.facebook.com/reel/1/",
			"Grandma's 90th | part 2",
			"Grandma's 90th | part 2"},
		{"x drops t.co link and keeps uploader",
			"https://x.com/litteralyme0/status/2110310483568078903?s=20",
			"Miilato - https://t.co/yknmtwQGjA",
			"Miilato"},
		{"x collapses whitespace",
			"https://x.com/punkeydoodles8/status/2111963034059161910?s=20",
			"Punkey Doodles - PALADIN LOGIC UNMATCHED  Audio by @ZacSpeaksGiant",
			"Punkey Doodles - PALADIN LOGIC UNMATCHED Audio by @ZacSpeaksGiant"},
		{"x link mid-text",
			"https://twitter.com/a/status/1",
			"Someone - look at this https://t.co/abc123 lol",
			"Someone - look at this lol"},
		{"other sites untouched",
			"https://www.youtube.com/watch?v=x",
			"332K views · 8.9K reactions | not facebook  https://t.co/x",
			"332K views · 8.9K reactions | not facebook  https://t.co/x"},
		{"never returns empty",
			"https://x.com/a/status/1",
			"https://t.co/only",
			"https://t.co/only"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := CleanTitle(c.url, c.in); got != c.want {
				t.Errorf("CleanTitle(%q)\n got %q\nwant %q", c.in, got, c.want)
			}
		})
	}
}

func TestTrackerCleansTitles(t *testing.T) {
	e := newGrabEnv(t)
	_, tok := e.user(t, "alex", false)
	id := e.submit(t, tok, `{"url":"https://x.com/a/status/1"}`)
	e.tick(t)
	e.mt.setDone(HistoryItem{URL: "https://x.com/a/status/1", Title: "Miilato - https://t.co/yknmtwQGjA",
		Status: "finished", Folder: "users/alex/twitter", Filename: "m.mp4"})
	e.tick(t)
	if d := e.get(t, id); d.Title != "Miilato" {
		t.Fatalf("stored title = %q, want %q", d.Title, "Miilato")
	}
}
