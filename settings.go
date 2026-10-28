package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

// Settings API. Apprise URLs and token plaintext are secrets: they are
// accepted here but never returned (a token's plaintext only once, at creation).

// downloadOnlyLabel is how the "original" target reads in the targets list;
// the preset's own label stays "Original" where the file itself is meant.
const downloadOnlyLabel = "Download only"

const maxTokenLabel = 64

// notifyMode is "none" or "oidc"; an App without a Notifier behaves as none.
func (a *App) notifyMode() string {
	if a.Notifier != nil && a.Notifier.Mode == "oidc" {
		return "oidc"
	}
	return "none"
}

// notifyURLs are the URLs a notification for u goes to.
func (a *App) notifyURLs(u User) ([]string, error) {
	if a.notifyMode() == "oidc" {
		return a.Store.NotifyURLs(u.ID)
	}
	if a.Notifier == nil {
		return nil, nil
	}
	return a.Notifier.InstanceURLs, nil
}

func (a *App) getMe(w http.ResponseWriter, r *http.Request) {
	u := viewer(r)
	urls, err := a.notifyURLs(u)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "could not read settings")
		return
	}
	type target struct {
		Name  string `json:"name"`
		Label string `json:"label"`
	}
	targets := make([]target, 0, len(Presets))
	for _, p := range Presets {
		l := p.Label
		if p.Name == "original" {
			l = downloadOnlyLabel
		}
		targets = append(targets, target{p.Name, l})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name": u.Name, "dir": u.Dir, "admin": u.Admin, "auth": a.notifyMode(),
		"defaultTarget": u.DefaultTarget,
		"notify":        map[string]any{"set": len(urls) > 0, "count": len(urls), "instanceWide": a.notifyMode() == "none"},
		"targets":       targets,
	})
}

func (a *App) putMe(w http.ResponseWriter, r *http.Request) {
	var in struct {
		DefaultTarget string `json:"defaultTarget"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
		apiError(w, http.StatusBadRequest, "bad request")
		return
	}
	if _, ok := PresetByName(in.DefaultTarget); !ok {
		apiError(w, http.StatusBadRequest, "unknown target")
		return
	}
	if err := a.Store.SetDefaultTarget(viewer(r).ID, in.DefaultTarget); err != nil {
		apiError(w, http.StatusInternalServerError, "could not save")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) putNotify(w http.ResponseWriter, r *http.Request) {
	if a.notifyMode() != "oidc" {
		apiError(w, http.StatusConflict, "set APPRISE_URLS instead")
		return
	}
	var in struct {
		URLs []string `json:"urls"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
		apiError(w, http.StatusBadRequest, "bad request")
		return
	}
	urls := make([]string, 0, len(in.URLs))
	for _, s := range in.URLs {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if !strings.Contains(s, "://") {
			apiError(w, http.StatusBadRequest, "each URL must look like service://...")
			return
		}
		urls = append(urls, s)
	}
	if err := a.Store.SetNotifyURLs(viewer(r).ID, urls); err != nil {
		apiError(w, http.StatusInternalServerError, "could not save")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) deleteNotify(w http.ResponseWriter, r *http.Request) {
	if a.notifyMode() != "oidc" {
		apiError(w, http.StatusConflict, "set APPRISE_URLS instead")
		return
	}
	if err := a.Store.SetNotifyURLs(viewer(r).ID, nil); err != nil {
		apiError(w, http.StatusInternalServerError, "could not save")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) testNotify(w http.ResponseWriter, r *http.Request) {
	urls, err := a.notifyURLs(viewer(r))
	if err != nil {
		apiError(w, http.StatusInternalServerError, "could not read settings")
		return
	}
	if len(urls) == 0 || a.Notifier == nil || a.Notifier.Sender == nil {
		apiError(w, http.StatusConflict, "no notification URLs saved")
		return
	}
	// The Sender's errors never carry the URLs, but say nothing of them anyway.
	if err := a.Notifier.Sender.Send(r.Context(), urls, "Test from grab", "Notifications are working."); err != nil {
		apiError(w, http.StatusBadGateway, "notification failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *App) listTokens(w http.ResponseWriter, r *http.Request) {
	ts, err := a.Store.ListTokens(viewer(r).ID)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "could not list tokens")
		return
	}
	type view struct {
		ID       string     `json:"id"`
		Label    string     `json:"label"`
		Created  time.Time  `json:"created"`
		LastUsed *time.Time `json:"lastUsed"`
	}
	out := make([]view, 0, len(ts))
	for _, t := range ts {
		v := view{ID: t.ID, Label: t.Label, Created: t.CreatedAt}
		if !t.LastUsed.IsZero() {
			lu := t.LastUsed
			v.LastUsed = &lu
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *App) createToken(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Label string `json:"label"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil {
		apiError(w, http.StatusBadRequest, "bad request")
		return
	}
	label := strings.TrimSpace(in.Label)
	if label == "" || utf8.RuneCountInString(label) > maxTokenLabel {
		apiError(w, http.StatusBadRequest, "label is required, at most 64 characters")
		return
	}
	plain, t, err := a.Store.CreateToken(viewer(r).ID, label)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "could not create token")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": t.ID, "label": t.Label, "token": plain})
}

func (a *App) deleteToken(w http.ResponseWriter, r *http.Request) {
	if err := a.Store.DeleteToken(viewer(r).ID, r.PathValue("id")); err != nil && !errors.Is(err, ErrNotFound) {
		apiError(w, http.StatusInternalServerError, "could not delete token")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
