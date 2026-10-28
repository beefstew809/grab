package main

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"
)

// HistoryItem is the subset of a MeTube /history entry we use. MeTube sends
// null for absent strings/numbers; encoding/json leaves those as zero values.
type HistoryItem struct {
	Title     string  `json:"title"`
	URL       string  `json:"url"`
	Folder    string  `json:"folder"`
	Filename  string  `json:"filename"`
	Status    string  `json:"status"`
	Error     string  `json:"error"`
	Msg       string  `json:"msg"`
	Percent   float64 `json:"percent"`
	Size      int64   `json:"size"`
	Timestamp int64   `json:"timestamp"`
}

type History struct {
	Done    []HistoryItem `json:"done"`
	Queue   []HistoryItem `json:"queue"`
	Pending []HistoryItem `json:"pending"`
}

// AddRequest is one download handed to MeTube. Folder is relative to
// MeTube's download dir, which is SHARES_DIR here.
type AddRequest struct {
	URL, Folder string
	CookieFile  string // "" = none
}

// MeTubeClient is the part of MeTube grab uses; tests swap in a fake.
type MeTubeClient interface {
	History(ctx context.Context) (History, error)
	Add(ctx context.Context, r AddRequest) error
	// Delete forgets finished entries; best effort.
	Delete(ctx context.Context, urls []string) error
}

// MeTubeError is MeTube refusing a download: its JSON {"status":"error"} or
// a 4xx. Retrying the same request won't help. Any other Add error (no
// connection, a timeout, a 5xx) means MeTube itself is unwell: try again later.
type MeTubeError struct{ Msg string }

func (e *MeTubeError) Error() string { return "metube: " + e.Msg }

// ErrAlreadyQueued: MeTube answered {"status":"ok","msg":"Already in queue: …"}.
// Its key (yt-dlp's webpage_url) is already queued, possibly for another
// user's folder, so it dropped this request. Nothing was queued: try later.
var ErrAlreadyQueued = errors.New("metube: already in queue")

// defaultAddTimeout: MeTube runs yt-dlp's extraction (a fetch of the page,
// sometimes several) before it answers /add.
const defaultAddTimeout = 120 * time.Second

type MeTube struct {
	Base string
	HC   *http.Client
	// AddTimeout bounds /add, which waits for yt-dlp's extraction: far
	// longer than HC's timeout allows. 0 = defaultAddTimeout.
	AddTimeout time.Duration
}

func (m *MeTube) History(ctx context.Context) (History, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(m.Base, "/")+"/history", nil)
	if err != nil {
		return History{}, err
	}
	resp, err := m.HC.Do(req)
	if err != nil {
		return History{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return History{}, fmt.Errorf("metube /history: %s", resp.Status)
	}
	var h History
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		return History{}, fmt.Errorf("metube /history: %w", err)
	}
	return h, nil
}

// Add queues a download. Video as H.264 MP4 at the best quality: the format
// every share target plays without a conversion, when the site has it.
// The cookie file goes through ytdl_options_overrides (MeTube needs
// ALLOW_YTDL_OPTIONS_OVERRIDES=true); no other override is ever sent.
func (m *MeTube) Add(ctx context.Context, r AddRequest) error {
	body := map[string]any{"url": r.URL, "download_type": "video", "codec": "h264", "format": "mp4",
		"quality": "best", "folder": r.Folder}
	if r.CookieFile != "" {
		body["ytdl_options_overrides"] = map[string]string{"cookiefile": r.CookieFile}
	}
	// A copy of HC with a longer timeout: MeTube answers /add only after
	// yt-dlp has extracted the page. HC itself is shared and keeps its own.
	hc := *m.HC
	hc.Timeout = cmp.Or(m.AddTimeout, defaultAddTimeout)
	code, reply, err := m.post(ctx, &hc, "/add", body)
	if err != nil {
		return err
	}
	var st struct{ Status, Msg string }
	isJSON := json.Unmarshal(reply, &st) == nil
	switch {
	case isJSON && st.Status == "error":
		return &MeTubeError{Msg: cmp.Or(st.Msg, "refused without a reason")}
	case code/100 == 2 && isJSON && strings.HasPrefix(st.Msg, "Already in queue"):
		return ErrAlreadyQueued
	case code/100 == 2:
		return nil
	case code/100 == 4:
		return &MeTubeError{Msg: cmp.Or(strings.TrimSpace(string(reply)), http.StatusText(code))}
	default:
		return fmt.Errorf("metube /add: HTTP %d", code)
	}
}

func (m *MeTube) Delete(ctx context.Context, urls []string) error {
	code, _, err := m.post(ctx, m.HC, "/delete", map[string]any{"ids": urls, "where": "done"})
	if err == nil && code/100 != 2 {
		err = fmt.Errorf("metube /delete: HTTP %d", code)
	}
	return err
}

// post sends JSON and returns the status and up to 64 KB of the reply.
func (m *MeTube) post(ctx context.Context, hc *http.Client, path string, body any) (int, []byte, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(m.Base, "/")+path, bytes.NewReader(b))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	reply, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, reply, nil
}

// ItemID was the id of a download when the list came from MeTube's history
// (which is keyed by URL). New downloads get random ids; downloads adopted
// from that era keep this one, so their links stay valid.
func ItemID(url string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(url)))
	return hex.EncodeToString(sum[:])[:16]
}

// resolvePath maps a download's folder and filename to a file under sharesDir, refusing
// anything that could escape it. MeTube writes these names, but they come
// from video titles, so treat them as untrusted.
func resolvePath(sharesDir, folder, filename string) (string, bool) {
	if filename == "" {
		return "", false
	}
	for _, part := range []string{folder, filename} {
		if filepath.IsAbs(part) || strings.ContainsRune(part, 0) {
			return "", false
		}
		for _, seg := range strings.Split(filepath.ToSlash(part), "/") {
			if seg == ".." {
				return "", false
			}
		}
	}
	p := filepath.Join(sharesDir, folder, filename)
	rel, err := filepath.Rel(sharesDir, p)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return "", false
	}
	return p, true
}
