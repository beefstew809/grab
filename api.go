package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

type MediaProber interface {
	Probe(ctx context.Context, path string) (Probe, error)
}

type Conversions interface {
	Enqueue(itemID string, pr Preset, src string, p Probe, d Decision) Job
	Status(itemID, preset string) (Job, bool)
	CachePath(itemID, preset string) string
	Touch(path string)
	// Delete stops this item's conversions and prevents stale requests from
	// queuing more, then removes its finished and temporary cache files.
	Delete(itemID string) error
}

// App is the HTTP surface: the static PWA, the share API, /auth/* and /healthz.
type App struct {
	Static    fs.FS
	Store     *Store
	SharesDir string
	Prober    MediaProber
	Conv      Conversions
	// Library moves, deletes and sweeps downloaded files.
	Library *Library
	// Auth guards /api/*; nil means the API refuses everyone.
	Auth Auth
	// Health returns named checks; any non-nil error makes /healthz 503.
	// MeTube is deliberately NOT a check: dockerhealth restarts unhealthy
	// containers, and restarting this one can't fix MeTube.
	Health func() map[string]error
	// Nudge asks the tracker to tick now (nil = wait for its next tick).
	Nudge func()
	// Cookies holds per-user site cookies (nil = the cookie API is off).
	Cookies *CookieStore
	// Notifier supplies the notification mode, instance URLs and Sender for the
	// settings API (nil = AUTH=none with no URLs).
	Notifier *Notifier
	// HC resolves Facebook short links (nil = http.DefaultClient).
	HC *http.Client
}

// Item is one download as the API shows it to a viewer.
type Item struct {
	ID        string  `json:"id"`
	Title     string  `json:"title"`
	URL       string  `json:"url"`
	Source    string  `json:"source"` // the site folder: youtube, reddit, ..., other
	Status    string  `json:"status"` // waiting | queued | downloading | finished | error
	Percent   float64 `json:"percent"`
	Error     string  `json:"error,omitempty"`
	Size      int64   `json:"size"`
	Timestamp int64   `json:"timestamp"` // created, Unix nanoseconds
	Exists    bool    `json:"exists"`
	Public    bool    `json:"public"`
	Mine      bool    `json:"mine"`
	// Owner names someone else's download for an admin; empty otherwise.
	Owner  string `json:"owner,omitempty"`
	Target string `json:"target"`
	// NeedsCookies marks a failure that uploading the site's cookies may fix.
	NeedsCookies bool   `json:"needsCookies"`
	Path         string `json:"-"`
}

// ItemFrom builds the viewer's Item for a stored download. ownerName is only
// shown to an admin looking at another user's download.
func ItemFrom(d Download, sharesDir, ownerName string, viewer User) Item {
	it := Item{ID: d.ID, Title: cmp.Or(d.Title, d.SharedURL), URL: d.SharedURL, Source: d.Site, Status: d.Status,
		Percent: d.Percent, Error: d.Error, Size: d.Size, Timestamp: d.CreatedAt.UnixNano(), Public: d.Public,
		Mine: d.UserID == viewer.ID, Target: d.Target}
	it.NeedsCookies = d.Status == "error" && NeedsCookies(d.Error)
	if viewer.Admin && !it.Mine {
		it.Owner = ownerName
	}
	if d.Status == "finished" {
		if p, ok := resolvePath(sharesDir, d.Folder, d.Filename); ok {
			it.Path = p
			if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
				it.Exists = true
			}
		}
	}
	return it
}

// canSee: your own downloads, public ones, and everything for an admin.
func canSee(u User, d Download) bool { return d.UserID == u.ID || d.Public || u.Admin }

type PresetView struct {
	Name        string  `json:"name"`
	Label       string  `json:"label"`
	State       string  `json:"state"` // ready|convert|queued|converting|too_long|failed|missing
	EstBytes    int64   `json:"estBytes"`
	Percent     float64 `json:"percent,omitempty"`
	Error       string  `json:"error,omitempty"`
	MaxDuration float64 `json:"maxDuration,omitempty"`
}

type ItemDetail struct {
	Item
	Duration float64      `json:"duration"`
	Presets  []PresetView `json:"presets"`
}

// Routes: everything under /api/ needs a user; the install shell (static
// files), /healthz and /auth/* stay public so a browser can install the PWA
// and log in.
func (a *App) Routes() http.Handler {
	api := http.NewServeMux()
	api.HandleFunc("POST /api/downloads", a.submit)
	api.HandleFunc("GET /api/items", a.listItems)
	api.HandleFunc("GET /api/items/{id}", validID(a.getItem))
	api.HandleFunc("POST /api/items/{id}/prepare", validID(a.prepare))
	api.HandleFunc("GET /api/items/{id}/file", validID(a.file))
	api.HandleFunc("POST /api/items/{id}/public", validID(a.setPublic))
	api.HandleFunc("DELETE /api/items/{id}", validID(a.deleteItem))
	api.HandleFunc("GET /api/me", a.getMe)
	api.HandleFunc("PUT /api/me", a.putMe)
	api.HandleFunc("PUT /api/me/notify", a.putNotify)
	api.HandleFunc("DELETE /api/me/notify", a.deleteNotify)
	api.HandleFunc("POST /api/me/notify/test", a.testNotify)
	api.HandleFunc("GET /api/tokens", a.listTokens)
	api.HandleFunc("POST /api/tokens", a.createToken)
	api.HandleFunc("DELETE /api/tokens/{id}", a.deleteToken)
	api.HandleFunc("GET /api/cookies", a.listCookies)
	api.HandleFunc("PUT /api/cookies/{site}", a.putCookies)
	api.HandleFunc("DELETE /api/cookies/{site}", a.deleteCookies)

	mux := http.NewServeMux()
	mux.Handle("/api/", requireUser(a.Auth, api))
	mux.HandleFunc("GET /healthz", a.healthz)
	// "/" rather than "GET /": ServeMux rejects "GET /" next to "/api/" as
	// conflicting patterns, so the method check lives in getOnly.
	mux.Handle("/", getOnly(noCache(http.FileServerFS(a.Static))))
	if a.Auth != nil {
		a.Auth.Register(mux)
	}
	return mux
}

// requireUser fails closed: an App built without an Auth serves no API.
func requireUser(auth Auth, h http.Handler) http.Handler {
	if auth == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			apiError(w, http.StatusUnauthorized, "login required")
		})
	}
	return auth.Wrap(h)
}

func getOnly(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// noCache: without it a CDN keeps .js/.css at the edge for an hour, so a
// release keeps serving the previous app.js (1.1.3's filename fix didn't reach
// phones) and the cached copy is handed out without the login proxy's check.
func noCache(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		h.ServeHTTP(w, r)
	})
}

// validID rejects anything that isn't an ItemID before it reaches a cache
// path. {id} is URL-decoded, so "..%2Fx" would otherwise escape the cache dir.
func validID(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if len(id) != 16 || strings.Trim(id, "0123456789abcdef") != "" {
			apiError(w, http.StatusNotFound, "not found")
			return
		}
		h(w, r)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func apiError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// viewer is the user Auth.Wrap put on the request; every /api/ handler has one.
func viewer(r *http.Request) User {
	u, _ := UserFrom(r.Context())
	return u
}

// findItem loads {id} for the viewer. Downloads they can't see are 404, the
// same as ones that don't exist. It writes the error response itself and
// returns ok=false on failure.
func (a *App) findItem(w http.ResponseWriter, r *http.Request) (Item, bool) {
	u := viewer(r)
	d, err := a.Store.GetDownload(r.PathValue("id"))
	if err == nil && !canSee(u, d) {
		err = ErrNotFound
	}
	switch {
	case errors.Is(err, ErrNotFound):
		apiError(w, http.StatusNotFound, "not found")
		return Item{}, false
	case err != nil:
		log.Printf("api: load download: %v", err)
		apiError(w, http.StatusInternalServerError, "internal error")
		return Item{}, false
	}
	owner := ""
	if u.Admin && d.UserID != u.ID {
		if o, err := a.Store.UserByID(d.UserID); err == nil {
			owner = o.Name
		}
	}
	return ItemFrom(d, a.SharesDir, owner, u), true
}

type submitRequest struct {
	URL    string `json:"url"`
	Text   string `json:"text"`
	Target string `json:"target"`
}

// submit records a share as a waiting download; the tracker hands it to
// MeTube. Nothing here talks to MeTube, so a share is accepted (and kept)
// while the downloader is down.
func (a *App) submit(w http.ResponseWriter, r *http.Request) {
	u := viewer(r)
	var req submitRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		apiError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	shared, err := SharedURL(req.URL, req.Text)
	if err != nil {
		apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	target := cmp.Or(req.Target, u.DefaultTarget, "original")
	if _, ok := PresetByName(target); !ok {
		apiError(w, http.StatusBadRequest, "unknown target")
		return
	}
	// The same user sharing the same link again gets the record they have:
	// two records would point at one file. A failed one is retried in place,
	// e.g. after the user uploaded cookies for the site.
	switch prev, err := a.Store.UserShared(u.ID, shared); {
	case err == nil && prev.Status != "error":
		writeJSON(w, http.StatusOK, map[string]any{"id": prev.ID, "existing": true})
		return
	case err == nil:
		if _, err := a.Store.UpdateDownload(prev.ID, func(x *Download) error {
			x.Status, x.Error, x.Percent = "waiting", "", 0
			x.QueuedAt, x.NotifiedAt, x.MeTubeTS = time.Time{}, time.Time{}, 0
			// Retention counts from the retry, not from the first share.
			x.CreatedAt = time.Now()
			return nil
		}); err != nil {
			log.Printf("api: retry download: %v", err)
			apiError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if a.Nudge != nil {
			a.Nudge()
		}
		writeJSON(w, http.StatusOK, map[string]string{"id": prev.ID})
		return
	case !errors.Is(err, ErrNotFound):
		log.Printf("api: look up download: %v", err)
		apiError(w, http.StatusInternalServerError, "internal error")
		return
	}
	site := SiteFor(shared)
	hc := a.HC
	if hc == nil {
		hc = http.DefaultClient
	}
	d, err := a.Store.CreateDownload(Download{UserID: u.ID, SharedURL: shared,
		MeTubeURL: NormalizeFacebook(r.Context(), hc, shared), Site: site, Target: target,
		Folder: "users/" + u.Dir + "/" + site, Status: "waiting"})
	if err != nil {
		log.Printf("api: create download: %v", err)
		apiError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if a.Nudge != nil {
		a.Nudge()
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": d.ID})
}

func (a *App) preset(w http.ResponseWriter, r *http.Request) (Preset, bool) {
	pr, ok := PresetByName(r.URL.Query().Get("preset"))
	if !ok {
		apiError(w, http.StatusBadRequest, "unknown preset")
	}
	return pr, ok
}

func (a *App) listItems(w http.ResponseWriter, r *http.Request) {
	u := viewer(r)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 50
	}
	ds, err := a.Store.ListDownloads(ListFilter{ViewerID: u.ID, Admin: u.Admin, Limit: min(limit, 200)})
	if err != nil {
		log.Printf("api: list downloads: %v", err)
		apiError(w, http.StatusInternalServerError, "internal error")
		return
	}
	var names map[string]User
	if u.Admin {
		if names, err = a.Store.Users(); err != nil {
			log.Printf("api: list users: %v", err)
		}
	}
	items := make([]Item, 0, len(ds))
	for _, d := range ds {
		items = append(items, ItemFrom(d, a.SharesDir, names[d.UserID].Name, u))
	}
	writeJSON(w, http.StatusOK, items)
}

func (a *App) cached(id string, pr Preset) (string, os.FileInfo, bool) {
	p := a.Conv.CachePath(id, pr.Name)
	st, err := os.Stat(p)
	return p, st, err == nil
}

func (a *App) presetView(it Item, p Probe, probeErr error, pr Preset) PresetView {
	v := PresetView{Name: pr.Name, Label: pr.Label}
	if !it.Exists {
		v.State = "missing"
		return v
	}
	if _, st, ok := a.cached(it.ID, pr); ok {
		v.State, v.EstBytes = "ready", st.Size()
		return v
	}
	if probeErr != nil {
		v.State, v.Error = "failed", "couldn't read the video: "+probeErr.Error()
		return v
	}
	d, err := Decide(p, pr)
	if err != nil {
		v.State, v.Error = "failed", err.Error()
		return v
	}
	v.EstBytes = d.EstBytes
	switch d.Action {
	case ActionReady:
		v.State = "ready"
	case ActionTooLong:
		v.State, v.MaxDuration = "too_long", d.MaxDuration
	default:
		v.State = "convert"
		if j, ok := a.Conv.Status(it.ID, pr.Name); ok {
			switch j.State {
			case JobQueued:
				v.State = "queued"
			case JobConverting:
				v.State, v.Percent = "converting", j.Percent
			case JobFailed:
				v.State, v.Error = "failed", j.Error
			}
			// JobDone with no cache file means it was swept: offer "convert" again.
		}
	}
	return v
}

func (a *App) getItem(w http.ResponseWriter, r *http.Request) {
	it, ok := a.findItem(w, r)
	if !ok {
		return
	}
	d := ItemDetail{Item: it}
	var p Probe
	var perr error
	if it.Exists {
		p, perr = a.Prober.Probe(r.Context(), it.Path)
		d.Duration = p.Duration
	}
	if it.Status == "finished" {
		for _, pr := range Presets {
			d.Presets = append(d.Presets, a.presetView(it, p, perr, pr))
		}
	}
	writeJSON(w, http.StatusOK, d)
}

func (a *App) prepare(w http.ResponseWriter, r *http.Request) {
	pr, ok := a.preset(w, r)
	if !ok {
		return
	}
	it, ok := a.findItem(w, r)
	if !ok {
		return
	}
	if !it.Exists {
		writeJSON(w, http.StatusConflict, a.presetView(it, Probe{}, nil, pr))
		return
	}
	p, err := a.Prober.Probe(r.Context(), it.Path)
	if err != nil {
		writeJSON(w, http.StatusConflict, a.presetView(it, p, err, pr))
		return
	}
	if _, _, ok := a.cached(it.ID, pr); ok {
		writeJSON(w, http.StatusOK, a.presetView(it, p, nil, pr))
		return
	}
	d, err := Decide(p, pr)
	if err != nil || d.Action == ActionTooLong {
		writeJSON(w, http.StatusConflict, a.presetView(it, p, nil, pr))
		return
	}
	if d.Action == ActionReady {
		writeJSON(w, http.StatusOK, a.presetView(it, p, nil, pr))
		return
	}
	a.Conv.Enqueue(it.ID, pr, it.Path, p, d)
	writeJSON(w, http.StatusAccepted, a.presetView(it, p, nil, pr))
}

// file serves the converted copy when there is one, else the source if it
// is already fine for the preset.
func (a *App) file(w http.ResponseWriter, r *http.Request) {
	pr, ok := a.preset(w, r)
	if !ok {
		return
	}
	it, ok := a.findItem(w, r)
	if !ok {
		return
	}
	if wantDownload(r) {
		w.Header().Set("Content-Disposition", contentDisposition(it.Title))
	}
	if p, _, ok := a.cached(it.ID, pr); ok {
		a.Conv.Touch(p)
		serveMP4(w, r, p)
		return
	}
	if !it.Exists {
		apiError(w, http.StatusNotFound, "file missing")
		return
	}
	p, err := a.Prober.Probe(r.Context(), it.Path)
	if err != nil {
		apiError(w, http.StatusConflict, "couldn't read the video")
		return
	}
	if d, err := Decide(p, pr); err == nil && d.Action == ActionReady {
		serveMP4(w, r, it.Path)
		return
	}
	apiError(w, http.StatusConflict, "not ready")
}

// wantDownload: the PWA's fallback link when it can't share the file itself
// (Firefox has no file sharing; Chrome refuses shares over its size limit).
func wantDownload(r *http.Request) bool { return r.URL.Query().Get("download") == "1" }

// contentDisposition names the saved file after the title: an ASCII fallback
// for old clients plus the exact UTF-8 name (RFC 6266/5987). Characters that
// are illegal in filenames become spaces; trailing dots go (Android and
// Windows both mangle "Title....mp4").
func contentDisposition(title string) string {
	clean := strings.Map(func(r rune) rune {
		if r < 0x20 || strings.ContainsRune(`\/:*?"<>|`, r) {
			return ' '
		}
		return r
	}, title)
	// Collapse dot runs: Chrome refuses to share "Title....mp4" (see app.js).
	for strings.Contains(clean, "..") {
		clean = strings.ReplaceAll(clean, "..", ".")
	}
	clean = strings.TrimRight(strings.Join(strings.Fields(clean), " "), ". ")
	if runes := []rune(clean); len(runes) > 80 {
		clean = strings.TrimRight(string(runes[:80]), ". ")
	}
	if clean == "" {
		clean = "video"
	}
	ascii := strings.Map(func(r rune) rune {
		if r > 0x7e {
			return ' '
		}
		return r
	}, clean)
	ascii = strings.TrimRight(strings.Join(strings.Fields(ascii), " "), ". ")
	if ascii == "" {
		ascii = "video"
	}
	var enc strings.Builder
	for _, b := range []byte(clean + ".mp4") {
		if 'a' <= b && b <= 'z' || 'A' <= b && b <= 'Z' || '0' <= b && b <= '9' || strings.IndexByte("-._~", b) >= 0 {
			enc.WriteByte(b)
		} else {
			fmt.Fprintf(&enc, "%%%02X", b)
		}
	}
	return fmt.Sprintf(`attachment; filename="%s.mp4"; filename*=UTF-8''%s`, ascii, enc.String())
}

func serveMP4(w http.ResponseWriter, r *http.Request, path string) {
	f, err := os.Open(path)
	if err != nil {
		apiError(w, http.StatusNotFound, "file missing")
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "video/mp4")
	http.ServeContent(w, r, "", st.ModTime(), f)
}

// healthz needs no login, so it answers only "ok" or "fail" per check; the
// reason (which can name paths on the host) goes to the server log.
func (a *App) healthz(w http.ResponseWriter, r *http.Request) {
	out, code := map[string]string{}, http.StatusOK
	if a.Health != nil {
		for name, err := range a.Health() {
			out[name] = "ok"
			if err != nil {
				log.Printf("healthz: %s: %v", name, err)
				out[name], code = "fail", http.StatusServiceUnavailable
			}
		}
	}
	writeJSON(w, code, out)
}

// ownedDownload loads {id} for a change that only the owner or an admin may
// make: 404 when the viewer can't see it, 403 when they can but don't own it.
func (a *App) ownedDownload(w http.ResponseWriter, r *http.Request) (Download, bool) {
	u := viewer(r)
	d, err := a.Store.GetDownload(r.PathValue("id"))
	if err == nil && !canSee(u, d) {
		err = ErrNotFound
	}
	switch {
	case errors.Is(err, ErrNotFound):
		apiError(w, http.StatusNotFound, "not found")
		return Download{}, false
	case err != nil:
		log.Printf("api: load download: %v", err)
		apiError(w, http.StatusInternalServerError, "internal error")
		return Download{}, false
	}
	if d.UserID != u.ID && !u.Admin {
		apiError(w, http.StatusForbidden, "not yours")
		return Download{}, false
	}
	return d, true
}

func (a *App) setPublic(w http.ResponseWriter, r *http.Request) {
	d, ok := a.ownedDownload(w, r)
	if !ok {
		return
	}
	var req struct {
		Public *bool `json:"public"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil || req.Public == nil {
		apiError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	d, err := a.Library.SetPublic(r.Context(), d, *req.Public)
	switch {
	case errors.Is(err, ErrNotFinished):
		apiError(w, http.StatusConflict, "not finished")
		return
	case err != nil:
		log.Printf("api: set public: %v", err)
		apiError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, ItemFrom(d, a.SharesDir, "", viewer(r)))
}

func (a *App) deleteItem(w http.ResponseWriter, r *http.Request) {
	d, ok := a.ownedDownload(w, r)
	if !ok {
		return
	}
	switch err := a.Library.Delete(r.Context(), d); {
	case errors.Is(err, ErrInFlight):
		apiError(w, http.StatusConflict, "still downloading")
		return
	case err != nil:
		log.Printf("api: delete: %v", err)
		apiError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
