package main

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"xke/backend"
)

// The fetch: xke pages through the likes, the latest like first, and adds
// to the manifest the posts it does not hold yet.
//
// The first fetch is the deep one. It walks down to the boundary — the
// post given with --until, older than which everything was downloaded
// before xke — or, with --all, to the end of the list. With --backfill it
// walks past the boundary to the end as well, keeping of those older posts
// the videos and GIFs alone. Stopped half way, it continues from the
// cursor in state.json.
//
// Every fetch after it reads what is new: it starts at the top again and
// stops at the boundary, or at the first page of nothing but posts the
// manifest holds.

// pager reads the page of likes at a cursor ("" for the first) and returns
// its posts and the cursor of the next page, "" after the last.
type pager func(ctx context.Context, cursor string) ([]backend.Post, string, error)

const (
	// maxEmptyPages is how many pages without a post in a row end the
	// list: X leaves posts out of a page (deleted ones, say), so one empty
	// page is not the end, whatever the kinds of media wanted.
	maxEmptyPages = 3
	maxPages      = 5000
)

// fetchResult says what a fetch added to the manifest.
type fetchResult struct {
	Pages   int
	Posts   int    // posts added
	Media   int    // items to download
	NoMP4   int    // videos X serves without an MP4
	Stopped string // boundary, caught-up, end, limit
}

// fetch adds the liked posts the manifest lacks. limit, when not 0, is how
// many posts it adds at most: reaching it ends the deep fetch for good.
func fetch(ctx context.Context, st *Store, next pager, limit int, logf func(string, ...any)) (fetchResult, error) {
	s := &st.State
	var res fetchResult
	save := func(items []Item, posts int) error {
		if err := st.Add(items); err != nil {
			return err
		}
		res.Posts += posts
		for _, it := range items {
			switch it.Skip {
			case "":
				res.Media++
			case skipNoMP4:
				res.NoMP4++
			}
		}
		return nil
	}
	// What the read of what is new finds waits here until that read meets
	// what the manifest holds: stopped before, it leaves the manifest as it
	// was, without a gap the next run would take for read.
	var (
		pending      []Item
		pendingPosts int
		pendingSeen  = map[string]bool{}
	)
	flush := func() error {
		err := save(pending, pendingPosts)
		pending, pendingPosts = nil, 0
		return err
	}
	finish := func(why string) (fetchResult, error) {
		if err := flush(); err != nil {
			return res, err
		}
		if why != "caught-up" {
			s.Done, s.Cursor, s.CursorOld = true, "", false
		}
		if s.Backfill && (why == "end" || why == "limit") {
			s.BackfillDone = true
		}
		res.Stopped = why
		return res, st.SaveState()
	}

	// cur is the page to read and old whether it is past the boundary.
	// head is the read of what is new, which a page of known posts ends;
	// an unfinished deep fetch then goes on at its cursor (jumped).
	cur, old := "", false
	head := s.Done || s.Cursor != ""
	jumped, empty, taken := false, 0, 0
	for res.Pages < maxPages {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		posts, nextCur, err := next(ctx, cur)
		if err != nil {
			return res, err
		}
		res.Pages++
		if len(posts) == 0 && jumped {
			// The saved cursor leads nowhere any more: walk from the top.
			s.Cursor, s.CursorOld = "", false
			cur, old, head, jumped, empty = "", false, false, false, 0
			continue
		}
		jumped = false
		if len(posts) == 0 {
			empty++
		} else {
			empty = 0
		}

		added, toCursor := 0, false
		for i, p := range posts {
			if !old && s.isBoundary(p.ID) {
				if !s.Backfill || s.BackfillDone {
					if len(s.Fence) == 0 {
						for _, after := range posts[i+1:] {
							s.Fence = append(s.Fence, after.ID)
						}
					}
					logf("page %d: reached the boundary (post %s)", res.Pages, p.ID)
					return finish("boundary")
				}
				// The backfill goes on, where it stopped when it has a cursor.
				toCursor = head && s.Cursor != "" && s.CursorOld
				if err := flush(); err != nil {
					return res, err
				}
				old, head = true, false
			}
			if st.Known(p.ID) || pendingSeen[p.ID] {
				continue
			}
			items := itemsOf(p, old)
			if head {
				pending = append(pending, items...)
				pendingPosts++
				pendingSeen[p.ID] = true
			} else if err := save(items, 1); err != nil {
				return res, err
			}
			added++
			taken++
			if limit > 0 && taken >= limit {
				logf("page %d: reached the limit of %d posts", res.Pages, limit)
				return finish("limit")
			}
		}
		logf("page %d: %d posts, %d new", res.Pages, len(posts), added)

		switch {
		case toCursor:
			cur, jumped = s.Cursor, true
			continue
		case head && len(posts) > 0 && added == 0:
			if s.Done {
				return finish("caught-up")
			}
			if err := flush(); err != nil {
				return res, err
			}
			cur, old, head, jumped = s.Cursor, s.CursorOld, false, true
			continue
		case nextCur == "" || empty >= maxEmptyPages:
			return finish("end")
		}
		cur = nextCur
		if !head {
			s.Cursor, s.CursorOld = cur, old
			if err := st.SaveState(); err != nil {
				return res, err
			}
		}
	}
	return res, fmt.Errorf("gave up after %d pages", maxPages)
}

// itemsOf makes a post's lines of the manifest: one per media item, or one
// for a post without any. Of a post older than the boundary the photos are
// not downloaded: the script that came before xke has them.
func itemsOf(p backend.Post, old bool) []Item {
	date := ""
	if !p.Created.IsZero() {
		date = p.Created.UTC().Format(time.RFC3339)
	}
	if len(p.Media) == 0 {
		return []Item{{Post: p.ID, Author: p.Author, Date: date, Kind: "none", Skip: skipNoMedia, Text: p.Text}}
	}
	items := make([]Item, 0, len(p.Media))
	for i, m := range p.Media {
		it := Item{Post: p.ID, Author: p.Author, Date: date, Index: i, Kind: m.Kind, URL: m.URL, Key: m.Key}
		if i == 0 {
			it.Text = p.Text
		}
		switch {
		case old && m.Kind == "photo":
			it.Skip = skipOld
		case m.URL == "" && m.Kind == "photo":
			it.Skip = skipNoURL
		case m.URL == "":
			it.Skip = skipNoMP4
		default:
			it.File = fileName(p, i, m.Ext)
		}
		items = append(items, it)
	}
	return items
}

// nameZone is the time zone of the date in a file's name.
var nameZone = func() *time.Location {
	zone, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		panic(err)
	}
	return zone
}()

var reUnsafe = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// fileName names a media item as the script before xke did:
// <day posted>_<author>_<post ID>_p<place in the post>.<ext>.
func fileName(p backend.Post, index int, ext string) string {
	author := reUnsafe.ReplaceAllString(p.Author, "")
	if author == "" {
		author = "unknown"
	}
	if ext = reUnsafe.ReplaceAllString(ext, ""); ext == "" {
		ext = "jpg"
	}
	return fmt.Sprintf("%s_%s_%s_p%d.%s", p.Created.In(nameZone).Format("2006-01-02"),
		author, reUnsafe.ReplaceAllString(p.ID, ""), index, ext)
}
