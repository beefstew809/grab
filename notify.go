package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"
	"unicode/utf8"
)

func humanBytes(n int64) string {
	if n >= 1e9 {
		return fmt.Sprintf("%.1f GB", float64(n)/1e9)
	}
	return fmt.Sprintf("%.1f MB", float64(n)/1e6)
}

// Sender delivers one message to a list of Apprise URLs.
type Sender interface {
	Send(ctx context.Context, urls []string, title, body string) error
}

// AppriseCLI shells out to the apprise command. The URLs hold credentials, so
// they go through a 0600 config file (not argv, which other processes can
// read) and neither they nor apprise's output ever reach an error or the log.
type AppriseCLI struct{ Bin, TmpDir string }

func (a AppriseCLI) Send(ctx context.Context, urls []string, title, body string) error {
	bin := a.Bin
	if bin == "" {
		bin = "apprise"
	}
	f, err := os.CreateTemp(a.TmpDir, "apprise-*.cfg") // CreateTemp is 0600
	if err != nil {
		return errors.New("apprise: cannot create config file")
	}
	defer os.Remove(f.Name())
	_, werr := f.WriteString(strings.Join(urls, "\n") + "\n")
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return errors.New("apprise: cannot write config file")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// Stdout/Stderr are nil (discarded): apprise echoes the target URLs.
	err = exec.CommandContext(ctx, bin, "--config", f.Name(), "--title", title, "--body", body).Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &ee):
		return fmt.Errorf("apprise exited %d", ee.ExitCode())
	case ctx.Err() != nil:
		return errors.New("apprise timed out")
	default:
		return errors.New("apprise could not be run")
	}
}

// Notifier builds the messages and picks the recipients.
type Notifier struct {
	Store        *Store
	Sender       Sender
	BaseURL      *url.URL
	InstanceURLs []string // AUTH=none only
	Mode         string   // "none" | "oidc"
}

// Notify returns nil when done (sent, or nothing to send) and an error when
// the tracker should retry on its next tick. conversionError describes a
// target that couldn't be prepared; the original download is still finished.
func (n *Notifier) Notify(ctx context.Context, d Download, conversionError string) error {
	var urls []string
	if n.Mode == "oidc" {
		var err error
		if urls, err = n.Store.NotifyURLs(d.UserID); err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil // user is gone
			}
			return err
		}
	} else {
		urls = n.InstanceURLs
	}
	if len(urls) == 0 {
		return nil
	}
	title, body := n.message(d, conversionError)
	return n.Sender.Send(ctx, urls, title, body)
}

// StartupNotice says whether the instance-wide notifications (AUTH=none) are
// on, counting the URLs without printing them. With oidc each user sets their
// own, so there is nothing to say: "".
func (n *Notifier) StartupNotice() string {
	if n.Mode == "oidc" {
		return ""
	}
	switch len(n.InstanceURLs) {
	case 0:
		return "notifications: off (APPRISE_URLS is empty)"
	case 1:
		return "notifications: 1 Apprise URL configured"
	default:
		return fmt.Sprintf("notifications: %d Apprise URLs configured", len(n.InstanceURLs))
	}
}

func (n *Notifier) message(d Download, conversionError string) (title, body string) {
	base := strings.TrimRight(n.BaseURL.String(), "/")
	if d.Status != "error" && conversionError != "" {
		target := d.Target
		if pr, ok := PresetByName(target); ok {
			target = pr.Label
		}
		return "Conversion unavailable: " + d.Title, fmt.Sprintf("Downloaded, but could not prepare %s: %s\nThe original download is still available.\n%s/?item=%s",
			target, lastChars(conversionError, 300), base, d.ID)
	}
	if d.Status != "error" {
		return "Ready: " + d.Title, fmt.Sprintf("%s · %s\n%s/?item=%s", d.Site, humanBytes(d.Size), base, d.ID)
	}
	name := d.Title
	if name == "" {
		name = d.SharedURL
	}
	body = lastChars(d.Error, 300)
	if NeedsCookies(d.Error) {
		body += fmt.Sprintf("\nThis site may need your cookies: %s/?settings#cookies-%s", base, d.Site)
	}
	return "Failed: " + name, body
}

// lastChars keeps the tail (yt-dlp puts the cause last), cutting on a rune boundary.
func lastChars(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[len(s)-n:]
	for len(s) > 0 && !utf8.RuneStart(s[0]) {
		s = s[1:]
	}
	return s
}
