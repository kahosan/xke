// Command xke backs up the media of the posts an X account liked: fetch
// lists them in a manifest, download saves what the manifest lists, and
// sync does both, for cron.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // the zone of the dates in file names, wherever xke runs

	"xke/backend"
)

const usage = `xke backs up the media of the posts you liked on X.

  xke fetch     list new likes in <dir>/likes.jsonl
  xke download  download what the manifest lists into <dir>/likes/
  xke sync      both

The first fetch needs your handle and where to stop:

  xke sync --user <handle> --until <post ID or link> [--until …] [--backfill]
  xke sync --user <handle> --all

After it, "xke sync" reads what is new. "xke <command> -h" lists the flags.
`

// multi is a flag given several times.
type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(s string) error { *m = append(*m, s); return nil }

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	if len(args) == 0 || !slices.Contains([]string{"fetch", "download", "sync"}, args[0]) {
		fmt.Fprint(os.Stderr, usage)
		if len(args) > 0 && slices.Contains([]string{"help", "-h", "-help", "--help"}, args[0]) {
			return 0
		}
		return 2
	}
	cmd := args[0]
	fs := flag.NewFlagSet("xke "+cmd, flag.ContinueOnError)
	var until multi
	dir := fs.String("dir", ".", "the data directory: state.json, likes.jsonl, downloaded.jsonl, token and likes/")
	out := fs.String("out", "", "the media folder (default <dir>/likes)")
	proxy := fs.String("proxy", "", "a proxy URL; without it xke changes nothing about the network")
	token := fs.String("token", "", "the auth_token cookie, or a whole Cookie header (default $XKE_TOKEN, then <dir>/token)")
	user := fs.String("user", "", "your handle, the first time")
	fs.Var(&until, "until", "a post ID or link the fetch stops at: the latest like downloaded before xke (repeatable)")
	all := fs.Bool("all", false, "no boundary: fetch the whole list of likes")
	backfill := fs.Bool("backfill", false, "fetch the videos and GIFs older than the boundary as well, once")
	limit := fs.Int("limit", 0, "add at most this many posts, then stop for good")
	jobs := fs.Int("jobs", 8, "downloads at once")
	retries := fs.Int("retries", 2, "retries of a download that fails")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "xke: unexpected argument %q\n", fs.Arg(0))
		return 2
	}

	st, err := Open(*dir)
	if err != nil {
		return fail(err)
	}
	defer st.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	code := 0
	if cmd != "download" {
		if err := prepare(st, *user, until, *all, *backfill); err != nil {
			return fail(err)
		}
		if err := runFetch(ctx, st, *token, *proxy, *limit); err != nil {
			code = fail(err)
			if cmd == "fetch" || ctx.Err() != nil {
				return code
			}
		}
	}
	if cmd != "fetch" {
		media := *out
		if media == "" {
			media = filepath.Join(*dir, "likes")
		}
		res, err := download(ctx, st, downloadOptions{Out: media, Proxy: *proxy, Jobs: *jobs, Retries: *retries}, logf)
		fmt.Printf("download: %d downloaded, %d already there, %d failed (of %d) in %s\n",
			res.Downloaded, res.Had, res.Failed, res.Wanted, media)
		if err != nil {
			return fail(err)
		}
		if res.Failed > 0 {
			code = 1 // the next run tries them again
		}
	}
	return code
}

func logf(format string, args ...any) { fmt.Printf(format+"\n", args...) }

func fail(err error) int {
	if errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "xke: stopped; the next run continues")
	} else {
		fmt.Fprintln(os.Stderr, "xke:", err)
	}
	return 1
}

// prepare applies the flags of a fetch to the state, and refuses a first
// fetch that says neither where to stop nor to fetch everything.
func prepare(st *Store, user string, until []string, all, backfill bool) error {
	s := &st.State
	if handle := strings.TrimPrefix(strings.TrimSpace(user), "@"); handle != "" {
		if s.User != "" && !strings.EqualFold(s.User, handle) {
			return fmt.Errorf("%s holds the likes of @%s, not of @%s: use another --dir", st.Dir, s.User, handle)
		}
		s.User = handle
	}
	if s.User == "" {
		return errors.New("the first fetch needs your handle: --user <handle>")
	}
	for _, u := range until {
		id, err := postID(u)
		if err != nil {
			return fmt.Errorf("--until: %w", err)
		}
		if !slices.Contains(s.Boundary, id) {
			s.Boundary = append(s.Boundary, id)
		}
	}
	switch {
	case (all || s.All) && len(s.Boundary) > 0:
		return errors.New("--all and --until exclude each other")
	case all:
		s.All = true
	case len(s.Boundary) == 0 && !s.All:
		return errors.New("the first fetch needs --until <post ID or link>, the latest like downloaded before xke, or --all")
	}
	if backfill {
		if len(s.Boundary) == 0 {
			return errors.New("--backfill reads what is older than the boundary: it needs --until")
		}
		if !s.Backfill || s.BackfillDone {
			// Start it (again): the deep fetch walks from the top once more.
			s.Backfill, s.BackfillDone = true, false
			s.Done, s.Cursor, s.CursorOld = false, "", false
		}
	}
	return st.SaveState()
}

func runFetch(ctx context.Context, st *Store, tokenFlag, proxy string, limit int) error {
	token, ct0, err := credentials(tokenFlag, st.Dir)
	if err != nil {
		return err
	}
	client, err := backend.NewClient(token, ct0, proxy)
	if err != nil {
		return err
	}
	page := func(ctx context.Context, cursor string) ([]backend.Post, string, error) {
		ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		return client.Likes(ctx, st.State.User, cursor)
	}
	res, err := fetch(ctx, st, page, limit, logf)
	fmt.Printf("fetch: %d new posts on %d pages, %d files to download\n", res.Posts, res.Pages, res.Media)
	if res.NoMP4 > 0 {
		fmt.Printf("fetch: %d videos have no MP4 and are skipped (grep no-mp4 %s)\n", res.NoMP4, st.path("likes.jsonl"))
	}
	if res.Stopped == "limit" {
		fmt.Fprintf(os.Stderr, "xke: WARNING: stopped at --limit %d; the likes older than that are not read, and later runs do not go back for them\n", limit)
	}
	return err
}

// credentials finds the auth token: the flag, XKE_TOKEN, then the token
// file of the data directory. Each holds the auth_token cookie alone or a
// whole Cookie header.
func credentials(flagValue, dir string) (token, ct0 string, err error) {
	raw := flagValue
	if raw == "" {
		raw = os.Getenv("XKE_TOKEN")
	}
	if raw == "" {
		path := filepath.Join(dir, "token")
		if data, err := os.ReadFile(path); err == nil {
			raw = string(data)
			if info, err := os.Stat(path); err == nil && info.Mode().Perm()&0o077 != 0 {
				fmt.Fprintf(os.Stderr, "xke: WARNING: others can read %s: chmod 600 it\n", path)
			}
		}
	}
	if token, ct0 = parseCookie(raw); token == "" {
		return "", "", fmt.Errorf("no auth token: pass --token, set XKE_TOKEN, or write it to %s (chmod 600)", filepath.Join(dir, "token"))
	}
	return token, ct0, nil
}

// parseCookie reads the auth_token and ct0 of a Cookie header, or takes a
// value without "=" as the auth_token itself.
func parseCookie(raw string) (token, ct0 string) {
	raw = strings.TrimSpace(raw)
	if len(raw) >= 7 && strings.EqualFold(raw[:7], "cookie:") {
		raw = raw[7:]
	}
	if !strings.Contains(raw, "=") {
		return strings.TrimSpace(raw), ""
	}
	for _, part := range strings.Split(raw, ";") {
		name, value, _ := strings.Cut(strings.TrimSpace(part), "=")
		switch strings.TrimSpace(name) {
		case "auth_token":
			token = strings.TrimSpace(value)
		case "ct0":
			ct0 = strings.TrimSpace(value)
		}
	}
	return token, ct0
}
