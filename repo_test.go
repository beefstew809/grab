package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoPersonalStrings keeps the tree publishable: no file may mention the
// owner's hosts, domains or networks. The deny-list is supplied through
// GRAB_DENYLIST (comma separated, case-insensitive) so it never has to live
// in the repository; without it the test skips.
func TestNoPersonalStrings(t *testing.T) {
	raw := os.Getenv("GRAB_DENYLIST")
	if strings.TrimSpace(raw) == "" {
		t.Skip("GRAB_DENYLIST not set")
	}
	var deny []string
	for _, s := range strings.Split(raw, ",") {
		if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
			deny = append(deny, s)
		}
	}
	if len(deny) == 0 {
		t.Skip("GRAB_DENYLIST has no entries")
	}
	scanned := 0
	// .superpowers/ is local planning scratch (gitignored) that never ships.
	skip := map[string]bool{".git": true, ".superpowers": true}
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if skip[filepath.ToSlash(path)] {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil // a worktree's .git is a file
		}
		if d.IsDir() {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		scanned++
		low := strings.ToLower(string(b))
		for i, s := range deny {
			if strings.Contains(low, s) {
				// The entry's position, not its text: CI logs must not repeat it.
				t.Errorf("%s contains denied entry #%d", path, i+1)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned == 0 {
		t.Fatal("no files were scanned")
	}
}
