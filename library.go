package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// ErrNotFinished: only a finished download with its file on disk can move.
var ErrNotFinished = errors.New("not finished")

// ErrInFlight: the download is still waiting for or inside MeTube.
var ErrInFlight = errors.New("still downloading")

// inFlight: MeTube may still write this download's file. Deleting it now
// would leave the file to land later with no record pointing at it.
func inFlight(d Download) bool {
	switch d.Status {
	case "waiting", "queued", "downloading":
		return true
	}
	return false
}

// Refresher asks a media server to rescan its library.
type Refresher interface {
	Refresh(ctx context.Context) error
}

// Jellyfin triggers a library scan so a newly public (or removed) file shows up.
type Jellyfin struct {
	URL, APIKey, LibraryID string
	HC                     *http.Client
}

func (j *Jellyfin) Refresh(ctx context.Context) error {
	endpoint := strings.TrimRight(j.URL, "/") + "/Library/Refresh"
	if j.LibraryID != "" {
		endpoint = strings.TrimRight(j.URL, "/") + "/Items/" + url.PathEscape(j.LibraryID) + "/Refresh"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return errors.New("jellyfin: bad URL")
	}
	req.Header.Set("Authorization", `MediaBrowser Token="`+j.APIKey+`"`)
	hc := j.HC
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		// url.Error carries only the URL, never the header; keep it terse anyway.
		return fmt.Errorf("jellyfin refresh: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("jellyfin refresh: HTTP %d", resp.StatusCode)
	}
	return nil
}

// Library owns what happens to downloaded files after MeTube is done: moving
// them between a user's folder and public/, deleting them, retention.
type Library struct {
	Store     *Store
	SharesDir string
	Conv      Conversions
	Jellyfin  Refresher // nil = off
	// RefreshDelay coalesces a burst of changes into one Jellyfin scan
	// (default 30 s).
	RefreshDelay time.Duration

	// mu serialises file moves and deletes: the existence check for a free
	// name and the rename must not interleave with another move.
	mu      sync.Mutex
	timerMu sync.Mutex
	timer   *time.Timer
}

// SetPublic moves a finished download's file into public/<site> or the
// owner's folder and updates every record that points at that file.
func (l *Library) SetPublic(ctx context.Context, d Download, public bool) (Download, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	// Re-read: the caller's copy may predate another move.
	d, err := l.Store.GetDownload(d.ID)
	if err != nil {
		return Download{}, err
	}
	src, ok := resolvePath(l.SharesDir, d.Folder, d.Filename)
	if !ok || d.Status != "finished" {
		return Download{}, ErrNotFinished
	}
	if st, err := os.Stat(src); err != nil || !st.Mode().IsRegular() {
		return Download{}, ErrNotFinished
	}
	if d.Public == public {
		return d, nil
	}
	owner, err := l.Store.UserByID(d.UserID)
	if err != nil {
		return Download{}, err
	}
	newFolder := path.Join("users", owner.Dir, d.Site)
	if public {
		newFolder = path.Join("public", d.Site)
	}
	dstDir := filepath.Join(l.SharesDir, filepath.FromSlash(newFolder))
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return Download{}, err
	}
	newName, err := freeName(dstDir, d.Filename, owner.Dir)
	if err != nil {
		return Download{}, err
	}
	dst := filepath.Join(dstDir, newName)
	if err := os.Rename(src, dst); err != nil {
		return Download{}, err
	}
	// Every record for the old file follows it, or they would point at nothing.
	records, err := l.Store.DownloadsByFile(d.Folder, d.Filename)
	if err == nil && !containsID(records, d.ID) {
		records = append(records, d)
	}
	var out Download
	for _, r := range records {
		u, uerr := l.Store.UpdateDownload(r.ID, func(x *Download) error {
			x.Folder, x.Filename, x.Public = newFolder, newName, public
			return nil
		})
		if uerr != nil && err == nil {
			err = uerr
		}
		if r.ID == d.ID {
			out = u
		}
	}
	if err != nil {
		// Put the file back so records and disk agree again.
		if rerr := os.Rename(dst, src); rerr != nil {
			log.Printf("library: roll back move: %v", rerr)
		}
		return Download{}, err
	}
	l.scheduleRefresh()
	return out, nil
}

// freeName returns name, or name with " (<dir>)" and then " (<dir> 2)", ...
// before the extension, whichever doesn't exist in dir yet.
func freeName(dir, name, ownerDir string) (string, error) {
	if _, err := os.Lstat(filepath.Join(dir, name)); errors.Is(err, os.ErrNotExist) {
		return name, nil
	} else if err != nil {
		return "", err
	}
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for n := 1; ; n++ {
		tag := ownerDir
		if n > 1 {
			tag = fmt.Sprintf("%s %d", ownerDir, n)
		}
		suffix := " (" + tag + ")" + ext
		// Linux filesystems generally limit each component to 255 bytes.
		// Reserve space for the suffix without cutting a UTF-8 character.
		room := 255 - len(suffix)
		if room < 1 {
			return "", errors.New("filename extension and collision suffix are too long")
		}
		short := stem
		if len(short) > room {
			short = short[:room]
			for !utf8.ValidString(short) {
				short = short[:len(short)-1]
			}
		}
		cand := short + suffix
		if _, err := os.Lstat(filepath.Join(dir, cand)); errors.Is(err, os.ErrNotExist) {
			return cand, nil
		} else if err != nil {
			return "", err
		}
	}
}

func containsID(ds []Download, id string) bool {
	for _, d := range ds {
		if d.ID == id {
			return true
		}
	}
	return false
}

// Delete removes the record, its conversion-cache entries and, unless
// another record still points at it, the file. MeTube history is kept: its
// URL key may already identify another user's newer download.
// A download still in flight is refused with ErrInFlight.
func (l *Library) Delete(ctx context.Context, d Download) error {
	_, err := l.remove(ctx, d.ID, time.Time{})
	return err
}

// remove deletes the record id as it is now, not as the caller last saw it:
// a move may have changed its folder since. A non-zero sweepBefore makes it a
// retention delete, which leaves the row alone if it has become public or is
// no longer older than sweepBefore. It reports whether the record was removed.
func (l *Library) remove(ctx context.Context, id string, sweepBefore time.Time) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	d, err := l.Store.GetDownload(id)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !sweepBefore.IsZero() && (d.Public || inFlight(d) || !d.CreatedAt.Before(sweepBefore)) {
		return false, nil
	}
	if inFlight(d) {
		return false, ErrInFlight
	}
	shared := false
	if d.Filename != "" {
		others, err := l.Store.DownloadsByFile(d.Folder, d.Filename)
		if err != nil {
			return false, err
		}
		for _, o := range others {
			if o.ID != d.ID {
				shared = true
			}
		}
	}
	if !shared {
		if p, ok := resolvePath(l.SharesDir, d.Folder, d.Filename); ok {
			if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
				return false, err
			}
		}
	}
	if err := l.Conv.Delete(d.ID); err != nil {
		return false, err
	}
	if err := l.Store.DeleteDownload(d.ID); err != nil {
		return false, err
	}
	if d.Public {
		l.scheduleRefresh()
	}
	return true, nil
}

// Sweep deletes private downloads created more than maxAge ago; public ones
// are never swept. maxAge 0 turns retention off. It returns how many went.
func (l *Library) Sweep(ctx context.Context, maxAge time.Duration, now time.Time) (int, error) {
	if maxAge <= 0 {
		return 0, nil
	}
	cutoff := now.Add(-maxAge)
	old, err := l.Store.ExpiredPrivate(cutoff)
	if err != nil {
		return 0, err
	}
	n := 0
	var first error
	for _, d := range old {
		// The list may be stale by now; remove re-checks the fresh row.
		removed, err := l.remove(ctx, d.ID, cutoff)
		if err != nil {
			if first == nil {
				first = err
			}
			continue
		}
		if removed {
			n++
		}
	}
	return n, first
}

// scheduleRefresh asks Jellyfin to rescan once RefreshDelay has passed since
// the first change of a burst; later changes inside the window ride along.
func (l *Library) scheduleRefresh() {
	if l.Jellyfin == nil {
		return
	}
	delay := l.RefreshDelay
	if delay == 0 {
		delay = 30 * time.Second
	}
	l.timerMu.Lock()
	defer l.timerMu.Unlock()
	if l.timer != nil {
		return
	}
	l.timer = time.AfterFunc(delay, func() {
		l.timerMu.Lock()
		l.timer = nil
		l.timerMu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := l.Jellyfin.Refresh(ctx); err != nil {
			log.Printf("library: %v", err)
		}
	})
}
