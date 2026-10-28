package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Migrate adopts downloads left under SHARES_DIR/<site>/ by the old
// single-user app as userDir's private files: each is moved to
// users/<userDir>/<site>/ and gets a finished store record. Without apply it
// only prints the plan. Re-running is safe: moved files are gone from the old
// location, and an occupied target is skipped.
func Migrate(ctx context.Context, st *Store, mt MeTubeClient, sharesDir, userDir string, apply bool, out io.Writer) error {
	user, err := st.UserByDir(userDir)
	if err != nil {
		return fmt.Errorf("user %q: %w (they must log in once first)", userDir, err)
	}

	// MeTube history only supplies ids and titles, so being unreachable is a
	// warning, not a failure.
	history := map[[2]string]HistoryItem{} // (site, filename) -> entry
	if mt != nil {
		if h, err := mt.History(ctx); err != nil {
			fmt.Fprintln(out, "warning: MeTube history unavailable; using random ids and filename titles")
		} else {
			for _, it := range h.Done {
				history[[2]string{it.Folder, it.Filename}] = it
			}
		}
	}

	sites := append(append([]string{}, CookieSites...), "other")
	used := map[string]bool{} // ids handed out this run (a dry run inserts nothing)
	n, moved := 0, 0
	for _, site := range sites {
		entries, err := os.ReadDir(filepath.Join(sharesDir, site))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		for _, e := range entries {
			if !e.Type().IsRegular() {
				continue
			}
			name := e.Name()
			src := filepath.Join(sharesDir, site, name)
			folder := "users/" + userDir + "/" + site
			dst := filepath.Join(sharesDir, folder, name)
			if _, err := os.Lstat(dst); err == nil {
				continue
			} else if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) { // ENOTDIR: MkdirAll reports it below
				return err
			}
			info, err := os.Stat(src)
			if err != nil {
				return err
			}

			d := Download{UserID: user.ID, Site: site, Target: "original", Folder: folder, Filename: name,
				Title: strings.TrimSuffix(name, filepath.Ext(name)), Status: "finished",
				Size: info.Size(), FinishedAt: info.ModTime(), NotifiedAt: time.Now()}
			note := ""
			if it, ok := history[[2]string{site, name}]; ok {
				id := ItemID(it.URL)
				_, gerr := st.GetDownload(id)
				switch {
				case gerr == nil || used[id]:
					note = "note: " + site + "/" + name + " gets a random id (" + id + " is already taken)"
				case !errors.Is(gerr, ErrNotFound):
					return gerr
				default:
					d.ID, d.SharedURL, d.MeTubeURL, d.MeTubeTS = id, it.URL, it.URL, it.Timestamp
					d.Title = CleanTitle(it.URL, cmp.Or(it.Title, d.Title))
				}
			}
			if d.ID == "" {
				d.ID = randHex(8)
			}
			used[d.ID] = true

			fmt.Fprintf(out, "%s/%s -> %s/%s\n", site, name, folder, name)
			if note != "" {
				fmt.Fprintln(out, note)
			}
			n++
			if !apply {
				continue
			}
			// Record first, so a failed insert never leaves an untracked move;
			// undo it if the move fails. A record already at this destination
			// (a run killed between insert and rename) is reused, never doubled.
			prior, err := st.DownloadsByFile(folder, name)
			if err != nil {
				return err
			}
			if len(prior) == 0 {
				if _, err := st.CreateDownload(d); err != nil {
					return fmt.Errorf("after %d moved: %w", moved, err)
				}
			}
			err = os.MkdirAll(filepath.Dir(dst), 0o755)
			if err == nil {
				err = os.Rename(src, dst)
			}
			if err != nil {
				err = fmt.Errorf("after %d moved: %w", moved, err)
				if len(prior) == 0 {
					if derr := st.DeleteDownload(d.ID); derr != nil {
						err = errors.Join(err, fmt.Errorf("rollback of record %s failed: %w", d.ID, derr))
					}
				}
				return err
			}
			moved++
		}
	}
	if apply {
		fmt.Fprintf(out, "%d moved\n", n)
	} else {
		fmt.Fprintf(out, "%d to move\n", n)
	}
	return nil
}

// runMigrate is the `grab migrate` entry point; args exclude the subcommand.
func runMigrate(args []string, getenv func(string) string, out io.Writer) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	fs.SetOutput(out)
	user := fs.String("user", "", "directory name of the user who will own the files (required)")
	apply := fs.Bool("apply", false, "move the files; without it only the plan is printed")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *user == "" {
		return errors.New("--user is required")
	}
	cfg, err := LoadConfig(getenv)
	if err != nil {
		return err
	}
	st, err := OpenStore(filepath.Join(cfg.DataDir, "grab.sqlite3"))
	if err != nil {
		return err
	}
	defer st.Close()
	mt := &MeTube{Base: cfg.MeTubeURL, HC: &http.Client{Timeout: 10 * time.Second}}
	return Migrate(context.Background(), st, mt, cfg.SharesDir, *user, *apply, out)
}
