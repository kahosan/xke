package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"xke/backend"
)

// The download: every item of the manifest that is to be downloaded and is
// not in downloaded.jsonl goes to the media folder under its name, through
// XDL's resumable engine, and gets the time it was posted as its
// modification time. A file that is there already — the script before xke
// named its files the same way — is kept as it is and counted as had.

type downloadOptions struct {
	Out     string // the media folder
	Proxy   string // "" changes nothing: Go's default, the environment's
	Jobs    int    // downloads at once
	Retries int
}

type downloadResult struct {
	Wanted     int // items not downloaded before
	Downloaded int
	Had        int // the file was there already
	Failed     int
}

func download(ctx context.Context, st *Store, opt downloadOptions, logf func(string, ...any)) (downloadResult, error) {
	var res downloadResult
	done, err := st.Downloaded()
	if err != nil {
		return res, err
	}
	var todo []Item
	for _, it := range st.Items {
		if it.Skip != "" || it.File == "" || it.URL == "" || done[it.File] {
			continue
		}
		done[it.File] = true // a file listed twice downloads once
		todo = append(todo, it)
	}
	res.Wanted = len(todo)
	if len(todo) == 0 {
		return res, nil
	}
	client, err := backend.CreateHTTPClient(opt.Proxy, 0)
	if err != nil {
		return res, err
	}
	if err := os.MkdirAll(opt.Out, 0o755); err != nil {
		return res, err
	}

	var downloaded, had, failed, finished atomic.Int64
	work := make(chan Item)
	var workers sync.WaitGroup
	for range min(max(opt.Jobs, 1), len(todo)) {
		workers.Go(func() {
			for it := range work {
				path := filepath.Join(opt.Out, it.File)
				err := downloadItem(ctx, client, it, path, opt.Retries)
				hadFile := errors.Is(err, errHad)
				switch {
				case err == nil:
					downloaded.Add(1)
				case hadFile:
					had.Add(1)
				case ctx.Err() != nil:
					continue // stopped: the next run continues the .part
				default:
					failed.Add(1)
					logf("failed: %s: %v", it.File, err)
					continue
				}
				if err := st.MarkDownloaded(it, hadFile); err != nil {
					logf("downloaded.jsonl: %v", err)
				}
				if n := finished.Add(1); n%25 == 0 {
					logf("%d of %d files", n, len(todo))
				}
			}
		})
	}
	for _, it := range todo {
		if ctx.Err() != nil {
			break
		}
		work <- it
	}
	close(work)
	workers.Wait()
	res.Downloaded, res.Had, res.Failed = int(downloaded.Load()), int(had.Load()), int(failed.Load())
	return res, ctx.Err()
}

// errHad says the file was there before the download.
var errHad = errors.New("the file is there already")

func downloadItem(ctx context.Context, client *http.Client, it Item, path string, retries int) error {
	if _, err := os.Stat(path); err == nil {
		return errHad
	}
	if err := backend.Download(ctx, client, it.URL, path, retries); err != nil {
		return err
	}
	if posted, err := time.Parse(time.RFC3339, it.Date); err == nil {
		// The file dates from the post, as far as the file system says.
		_ = os.Chtimes(path, posted, posted)
	}
	return nil
}
