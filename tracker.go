package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"sync"
	"time"
)

// lostAfter: a download sent to MeTube that its history hasn't shown for this
// long (counted from QueuedAt) is assumed dropped (MeTube restarted, or
// someone removed it).
const lostAfter = 10 * time.Minute

// pairSlack: MeTube stamps an entry while handling /add, a moment before
// grab records QueuedAt; the slack also absorbs clock skew between containers.
const pairSlack = 5 * time.Second

// alreadyQueuedRetry spaces out retries after MeTube's "Already in queue":
// every /add makes MeTube run yt-dlp's extraction against the site again.
const alreadyQueuedRetry = time.Minute

// A failing Notify (an Apprise URL with a typo, a service that is down) is
// retried after notifyRetryMin, doubling up to notifyRetryMax, and dropped
// notifyGiveUp after its first failure. Each attempt may run apprise for up
// to 30 s inside the one tracker goroutine, so retrying every tick would
// delay everyone's downloads.
const (
	notifyRetryMin = time.Minute
	notifyRetryMax = time.Hour
	notifyGiveUp   = 24 * time.Hour
)

// notifyRetry is one download's failed-notification backoff.
type notifyRetry struct {
	first, next time.Time
	wait        time.Duration
}

// CookiePaths finds a user's cookie file for a site, as MeTube sees it.
type CookiePaths interface {
	Path(userDir, site string) (string, bool)
}

// Tracker moves grab's downloads along: it hands waiting ones to MeTube,
// copies MeTube's progress back, starts the conversion to the chosen target
// and announces the result. grab's store is the source of truth; MeTube's
// history is only read to learn how each download is doing.
type Tracker struct {
	Store     *Store
	MT        MeTubeClient
	SharesDir string
	Conv      Conversions
	Prober    MediaProber
	Cookies   CookiePaths // nil = no cookies
	// Notify announces a finished or failed download. nil = notifications
	// off: due downloads are marked notified without sending anything.
	Notify func(ctx context.Context, d Download, conversionError string) error
	Now    func() time.Time

	nudgeOnce sync.Once
	nudge     chan struct{}
	// retryAt holds back downloads MeTube answered "Already in queue" for.
	// Only Tick touches it, and Run never overlaps Ticks.
	retryAt map[string]time.Time
	// notifyRetry holds back downloads whose Notify failed. In memory only:
	// a restart retries them at once and starts their 24 h over.
	notifyRetry map[string]*notifyRetry
}

func (t *Tracker) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

func (t *Tracker) nudges() chan struct{} {
	t.nudgeOnce.Do(func() { t.nudge = make(chan struct{}, 1) })
	return t.nudge
}

// Nudge asks Run for a tick now, so a new share reaches MeTube within a
// second instead of at the next interval. It never blocks: one pending nudge
// covers any number of new shares.
func (t *Tracker) Nudge() {
	select {
	case t.nudges() <- struct{}{}:
	default:
	}
}

// Run ticks at start, every interval and on each nudge, until ctx ends.
func (t *Tracker) Run(ctx context.Context, every time.Duration) {
	tk := time.NewTicker(every)
	defer tk.Stop()
	for {
		if err := t.Tick(ctx); err != nil {
			log.Printf("tracker: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
		case <-t.nudges():
		}
	}
}

// Tick runs the four steps in order: submit, sync, convert, notify. A step
// that fails is reported but doesn't stop the later ones: the list of
// finished downloads still gets notified while MeTube is down.
func (t *Tracker) Tick(ctx context.Context) error {
	active, err := t.Store.Active()
	if err != nil {
		return err
	}
	var errs []error
	errs = append(errs, t.submit(ctx, active))
	// Re-read: submit moved some waiting downloads to queued.
	if active, err = t.Store.Active(); err != nil {
		return errors.Join(append(errs, err)...)
	}
	errs = append(errs, t.sync(ctx, active))
	errs = append(errs, t.finishUp(ctx))
	return errors.Join(errs...)
}

// submit hands waiting downloads to MeTube. Two rules keep each MeTube
// entry attributable to exactly one grab download:
//   - one in flight per URL: MeTube keys its history by URL, so a second
//     download of the same link would be merged into the first;
//   - one unpaired download per folder: MeTube records yt-dlp's canonical
//     URL rather than the one it was sent (youtu.be/x?si=… comes back as
//     youtube.com/watch?v=x), so a queued download that MeTube's history
//     hasn't shown yet is found by folder and time (see sync). With only
//     one such download per folder, that match is unambiguous.
//
// "queued" means exactly that unpaired state: sync moves a download on to
// downloading, finished or error the first time an entry matches it.
func (t *Tracker) submit(ctx context.Context, active []Download) error {
	inFlight := map[string]bool{} // MeTube URLs
	unpaired := map[string]bool{} // folders
	for _, d := range active {
		if d.Status == "queued" || d.Status == "downloading" {
			inFlight[d.MeTubeURL] = true
		}
		if d.Status == "queued" {
			unpaired[d.Folder] = true
		}
	}
	if t.retryAt == nil {
		t.retryAt = map[string]time.Time{}
	}
	for _, d := range active {
		if d.Status != "waiting" || inFlight[d.MeTubeURL] || unpaired[d.Folder] || t.now().Before(t.retryAt[d.ID]) {
			continue
		}
		req := AddRequest{URL: d.MeTubeURL, Folder: d.Folder}
		if t.Cookies != nil {
			if u, err := t.Store.UserByID(d.UserID); err == nil {
				req.CookieFile, _ = t.Cookies.Path(u.Dir, d.Site)
			}
		}
		// Taken before the call: MeTube stamps its entry while handling it.
		sent := t.now()
		err := t.MT.Add(ctx, req)
		var refused *MeTubeError
		switch {
		case errors.Is(err, ErrAlreadyQueued):
			// Another spelling of this video is in MeTube's queue (maybe for
			// another user's folder). Nothing was queued for this one: it
			// stays waiting and is tried again once the other has had time.
			t.retryAt[d.ID] = t.now().Add(alreadyQueuedRetry)
		case errors.As(err, &refused):
			delete(t.retryAt, d.ID)
			t.update(d.ID, func(x *Download) { x.Status, x.Error = "error", refused.Msg })
		case maybeQueued(err):
			// MeTube got the request but didn't answer in time; it carries on
			// and usually queues it anyway. Treat it as sent: pairing finds
			// the entry, or the lost timer settles it. Resending would only
			// meet "Already in queue".
			delete(t.retryAt, d.ID)
			inFlight[d.MeTubeURL], unpaired[d.Folder] = true, true
			t.update(d.ID, func(x *Download) { x.Status, x.QueuedAt, x.Error = "queued", sent, "" })
		case err != nil:
			// MeTube is unreachable or unwell: everything stays waiting and
			// the next tick tries again. No point trying the rest now.
			return fmt.Errorf("submit: %w", err)
		default:
			delete(t.retryAt, d.ID)
			inFlight[d.MeTubeURL], unpaired[d.Folder] = true, true
			t.update(d.ID, func(x *Download) { x.Status, x.QueuedAt, x.Error = "queued", sent, "" })
		}
	}
	return nil
}

// sync copies MeTube's view of each queued or downloading download. A
// download is paired with one history entry the first time it is seen: an
// entry in its own folder (so another user's download of the same link never
// finishes it), stamped after it was sent and owned by no other download.
// It then adopts MeTube's URL and timestamp, so every later lookup is exact.
// The URL may differ from the one sent (MeTube records yt-dlp's canonical
// URL); SharedURL keeps the link the user sent.
func (t *Tracker) sync(ctx context.Context, active []Download) error {
	var mine []Download
	for _, d := range active {
		if d.Status == "queued" || d.Status == "downloading" {
			mine = append(mine, d)
		}
	}
	if len(mine) == 0 {
		return nil
	}
	h, err := t.MT.History(ctx)
	if err != nil {
		// Without a history nothing can be judged lost; wait for the next tick.
		return fmt.Errorf("sync: %w", err)
	}
	now := t.now()
	owned := map[string]map[entryKey]bool{} // folder → entries other downloads own; loaded on demand
	for _, d := range mine {
		var e HistoryItem
		where := ""
		if d.MeTubeTS != 0 {
			e, where = findEntry(h, d)
		} else {
			if owned[d.Folder] == nil {
				if owned[d.Folder], err = t.claimedIn(d.Folder); err != nil {
					return fmt.Errorf("sync: %w", err)
				}
			}
			e, where = pairEntry(h, d, owned[d.Folder])
		}
		if where != "" && owned[d.Folder] != nil {
			// The update below records it in the store too, but this pass's
			// cached set must see it now.
			owned[d.Folder][entryKey{e.URL, e.Timestamp}] = true
		}
		switch {
		case where == "":
			since := d.QueuedAt
			if since.IsZero() {
				since = d.CreatedAt
			}
			if now.Sub(since) >= lostAfter {
				t.update(d.ID, func(x *Download) { x.Status, x.Error = "error", "the downloader lost this download" })
			}
		case where != "done":
			title := CleanTitle(d.MeTubeURL, e.Title)
			if d.Status == "downloading" && d.Percent == e.Percent && (e.Title == "" || d.Title == title) {
				continue // nothing new: skip the write
			}
			t.update(d.ID, func(x *Download) {
				x.Status, x.Percent, x.MeTubeURL, x.MeTubeTS = "downloading", e.Percent, e.URL, e.Timestamp
				if e.Title != "" {
					x.Title = title
				}
			})
		case e.Status == "finished":
			size := e.Size
			if p, ok := resolvePath(t.SharesDir, d.Folder, e.Filename); ok && size == 0 {
				if st, err := os.Stat(p); err == nil {
					size = st.Size()
				}
			}
			t.update(d.ID, func(x *Download) {
				x.Status, x.Percent, x.Filename, x.Size, x.FinishedAt = "finished", 100, e.Filename, size, now
				x.MeTubeURL, x.MeTubeTS = e.URL, e.Timestamp
				if e.Title != "" {
					x.Title = CleanTitle(d.MeTubeURL, e.Title)
				}
			})
		default:
			msg := e.Error
			if msg == "" {
				msg = e.Msg
			}
			if msg == "" {
				msg = "the download failed"
			}
			t.update(d.ID, func(x *Download) { x.Status, x.Error, x.MeTubeURL, x.MeTubeTS = "error", msg, e.URL, e.Timestamp })
		}
	}
	return nil
}

// entryKey identifies one MeTube history entry. The URL alone doesn't:
// MeTube reuses it for every download of the same video.
type entryKey struct {
	url string
	ts  int64
}

func historyLists(h History) [][]HistoryItem { return [][]HistoryItem{h.Queue, h.Pending, h.Done} }

// listName says which list an entry came from: done entries are final.
func listName(i int) string { return [...]string{"queue", "pending", "done"}[i] }

// findEntry finds a paired download's own entry: same URL, folder and
// timestamp (MeTube keeps an entry's timestamp from queue through done).
func findEntry(h History, d Download) (HistoryItem, string) {
	for i, l := range historyLists(h) {
		for _, e := range l {
			if e.URL == d.MeTubeURL && e.Folder == d.Folder && e.Timestamp == d.MeTubeTS {
				return e, listName(i)
			}
		}
	}
	return HistoryItem{}, ""
}

// claimedIn returns the entries that downloads in folder are paired with.
func (t *Tracker) claimedIn(folder string) (map[entryKey]bool, error) {
	ds, err := t.Store.InFolder(folder)
	if err != nil {
		return nil, err
	}
	out := map[entryKey]bool{}
	for _, d := range ds {
		if d.MeTubeTS != 0 {
			out[entryKey{d.MeTubeURL, d.MeTubeTS}] = true
		}
	}
	return out, nil
}

// pairEntry finds the entry for a download not yet paired: one in its
// folder, created after it was sent (so never a stale entry from an earlier
// download of the same video) and owned by no other download. An entry
// under the URL grab sent wins; otherwise the oldest, since MeTube may have
// rewritten the URL to yt-dlp's canonical one.
func pairEntry(h History, d Download, owned map[entryKey]bool) (HistoryItem, string) {
	since := d.QueuedAt
	if since.IsZero() {
		since = d.CreatedAt
	}
	floor := since.Add(-pairSlack).UnixNano()
	var best HistoryItem
	where := ""
	better := func(e HistoryItem) bool {
		if exact, bestExact := e.URL == d.MeTubeURL, best.URL == d.MeTubeURL; exact != bestExact {
			return exact
		}
		return e.Timestamp < best.Timestamp
	}
	for i, l := range historyLists(h) {
		for _, e := range l {
			if e.Folder != d.Folder || e.Timestamp < floor || owned[entryKey{e.URL, e.Timestamp}] {
				continue
			}
			if where == "" || better(e) {
				best, where = e, listName(i)
			}
		}
	}
	return best, where
}

// maybeQueued: MeTube got the /add but grab stopped waiting for the answer.
// A connection that never opened (refused, or a dial timeout) is not that.
func maybeQueued(err error) bool {
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return false
	}
	var to interface{ Timeout() bool }
	return errors.Is(err, context.DeadlineExceeded) || errors.As(err, &to) && to.Timeout()
}

// finishUp starts the conversion to each download's target and notifies the
// ones that are due. It only looks at downloads not yet notified: once the
// owner has been told, the target copy is made on demand like any other, so
// a restart (which forgets conversion jobs) never re-converts old downloads.
func (t *Tracker) finishUp(ctx context.Context) error {
	ds, err := t.Store.Unnotified()
	if err != nil {
		return err
	}
	if t.notifyRetry == nil {
		t.notifyRetry = map[string]*notifyRetry{}
	}
	pending := map[string]bool{}
	var errs []error
	for _, d := range ds {
		pending[d.ID] = true
		r := t.notifyRetry[d.ID]
		if r != nil && t.now().Before(r.next) {
			continue
		}
		ready, conversionError := t.due(ctx, d)
		if !ready {
			continue
		}
		if t.Notify != nil {
			if err := t.Notify(ctx, d, conversionError); err != nil {
				if t.backOff(d.ID, r) {
					// The error comes from Notify, which never puts a URL in it.
					log.Printf("tracker: giving up notifying %s after %v of failures: %v", d.ID, notifyGiveUp, err)
				} else {
					errs = append(errs, fmt.Errorf("notify %s: %w", d.ID, err))
					continue
				}
			}
		}
		delete(t.notifyRetry, d.ID)
		now := t.now()
		t.update(d.ID, func(x *Download) { x.NotifiedAt = now })
	}
	for id := range t.notifyRetry {
		if !pending[id] { // deleted, or notified elsewhere
			delete(t.notifyRetry, id)
		}
	}
	return errors.Join(errs...)
}

// backOff records a failed Notify for id (r is its current backoff, nil
// after a first failure) and reports whether to give up on it.
func (t *Tracker) backOff(id string, r *notifyRetry) bool {
	now := t.now()
	if r == nil {
		r = &notifyRetry{first: now, wait: notifyRetryMin}
		t.notifyRetry[id] = r
	} else {
		r.wait = min(2*r.wait, notifyRetryMax)
	}
	r.next = now.Add(r.wait)
	return now.Sub(r.first) >= notifyGiveUp
}

// due reports whether the owner should hear about d now, starting the target
// conversion on the way (the same rules as the prepare endpoint). A finished
// download is due once its file is visible (NFS can lag MeTube) and the
// target copy is ready, can't be made, or failed: the message then says
// what happened instead of arriving before the file can be shared.
func (t *Tracker) due(ctx context.Context, d Download) (bool, string) {
	if d.Status == "error" {
		return true, ""
	}
	src, ok := resolvePath(t.SharesDir, d.Folder, d.Filename)
	if !ok {
		return false, ""
	}
	if st, err := os.Stat(src); err != nil || !st.Mode().IsRegular() {
		return false, ""
	}
	pr, ok := PresetByName(d.Target)
	if !ok || pr.Name == "original" {
		return true, ""
	}
	if _, err := os.Stat(t.Conv.CachePath(d.ID, pr.Name)); err == nil {
		return true, ""
	}
	if j, ok := t.Conv.Status(d.ID, pr.Name); ok {
		switch j.State {
		case JobFailed:
			return true, "conversion failed: " + j.Error
		case JobQueued, JobConverting:
			return false, ""
		}
		// JobDone without a cache file: swept already, convert again.
	}
	p, err := t.Prober.Probe(ctx, src)
	if err != nil {
		log.Printf("tracker: probe %s: %v", d.ID, err)
		return true, "couldn't read the downloaded video"
	}
	dec, err := Decide(p, pr)
	if err != nil {
		return true, err.Error()
	}
	if dec.Action == ActionTooLong {
		return true, fmt.Sprintf("video is too long (maximum about %.0f seconds)", dec.MaxDuration)
	}
	if dec.Action == ActionReady {
		return true, ""
	}
	t.Conv.Enqueue(d.ID, pr, src, p, dec)
	return false, ""
}

// update logs instead of failing the tick: one bad row must not stall the
// rest. fn runs inside the store's transaction and must not call the store.
func (t *Tracker) update(id string, fn func(*Download)) {
	if _, err := t.Store.UpdateDownload(id, func(d *Download) error { fn(d); return nil }); err != nil &&
		!errors.Is(err, ErrNotFound) {
		log.Printf("tracker: update %s: %v", id, err)
	}
}
