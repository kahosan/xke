package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"xke/backend"
)

// fakeX is a list of likes, the latest first, served in pages. A cursor
// names the post a page comes after, as X's do: likes added on top do not
// move it.
type fakeX struct {
	posts  []backend.Post
	size   int
	hidden map[string]bool // posts X leaves out of its pages
	failAt int             // the call that fails, from 1
	calls  []string
}

func (x *fakeX) page(_ context.Context, cursor string) ([]backend.Post, string, error) {
	x.calls = append(x.calls, cursor)
	if x.failAt > 0 && len(x.calls) == x.failAt {
		return nil, "", errors.New("the network is down")
	}
	start := 0
	if cursor != "" {
		i := slices.IndexFunc(x.posts, func(p backend.Post) bool { return p.ID == cursor })
		if i < 0 {
			return nil, "", nil
		}
		start = i + 1
	}
	if start >= len(x.posts) {
		return nil, "", nil
	}
	end := min(start+x.size, len(x.posts))
	var out []backend.Post
	for _, p := range x.posts[start:end] {
		if !x.hidden[p.ID] {
			out = append(out, p)
		}
	}
	return out, x.posts[end-1].ID, nil
}

// like makes a post with media of the kinds given; "stream" is a video
// without an MP4.
func like(id int, kinds ...string) backend.Post {
	p := backend.Post{ID: strconv.Itoa(id), Author: "mo13tto", Text: "post " + strconv.Itoa(id),
		Created: time.Date(2026, 9, 20, 17, 30, 0, 0, time.UTC)}
	for i, k := range kinds {
		m := backend.Media{Kind: k, Ext: "mp4", URL: fmt.Sprintf("https://video.twimg.com/ext_tw_video/%d%d/v.mp4", id, i)}
		switch k {
		case "photo":
			m.Ext, m.URL = "jpg", fmt.Sprintf("https://pbs.twimg.com/media/P%d_%d?format=jpg&name=orig", id, i)
		case "stream":
			m.Kind, m.URL = "video", ""
		}
		p.Media = append(p.Media, m)
	}
	return p
}

// likes makes n photo posts with IDs from first down.
func likes(first, n int) []backend.Post {
	out := make([]backend.Post, n)
	for i := range out {
		out[i] = like(first-i, "photo")
	}
	return out
}

func store(t *testing.T, s State) *Store {
	t.Helper()
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	st.State = s
	return st
}

func run1(t *testing.T, st *Store, x *fakeX, limit int) (fetchResult, error) {
	t.Helper()
	x.calls = nil
	return fetch(context.Background(), st, x.page, limit, func(string, ...any) {})
}

// held is the posts of the manifest, as "first-last" ranges of IDs.
func held(st *Store) string {
	var ids []int
	for _, it := range st.Items {
		id, _ := strconv.Atoi(it.Post)
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	slices.Reverse(ids)
	var parts []string
	for i := 0; i < len(ids); {
		j := i
		for j+1 < len(ids) && ids[j+1] == ids[j]-1 {
			j++
		}
		parts = append(parts, fmt.Sprintf("%d-%d", ids[i], ids[j]))
		i = j + 1
	}
	return strings.Join(parts, " ")
}

func TestBoundary(t *testing.T) {
	x := &fakeX{posts: likes(1000, 100), size: 10}
	st := store(t, State{User: "me", Boundary: []string{"975"}})
	res, err := run1(t, st, x, 0)
	if err != nil || res.Stopped != "boundary" || res.Posts != 25 || res.Media != 25 || res.Pages != 3 {
		t.Fatalf("first fetch = %+v, %v", res, err)
	}
	if held(st) != "1000-976" || !st.State.Done || st.State.Cursor != "" {
		t.Fatalf("held %s, state %+v", held(st), st.State)
	}
	if got := strings.Join(st.State.Fence, " "); got != "974 973 972 971" {
		t.Errorf("fence = %s", got)
	}

	// Later: three new likes, and the boundary post is gone. The fetch
	// stops at the first page of known posts, far above the boundary.
	x.posts = slices.DeleteFunc(append(likes(1003, 3), x.posts...), func(p backend.Post) bool { return p.ID == "975" })
	res, err = run1(t, st, x, 0)
	if err != nil || res.Stopped != "caught-up" || res.Posts != 3 || res.Pages != 2 {
		t.Fatalf("second fetch = %+v, %v", res, err)
	}
	if held(st) != "1003-976" {
		t.Errorf("held %s", held(st))
	}

	// A manifest that lost its posts: the fence stops the fetch where the
	// boundary was.
	lost := store(t, st.State)
	res, err = run1(t, lost, x, 0)
	if err != nil || res.Stopped != "boundary" || held(lost) != "1003-976" {
		t.Fatalf("without the boundary post = %+v, %v, held %s", res, err, held(lost))
	}
}

func TestStoppedReadOfNewLikesLeavesNoGap(t *testing.T) {
	x := &fakeX{posts: likes(1000, 40), size: 10}
	st := store(t, State{User: "me", Boundary: []string{"970"}})
	if _, err := run1(t, st, x, 0); err != nil {
		t.Fatal(err)
	}
	x.posts = append(likes(1025, 25), x.posts...)
	x.failAt = 2
	if res, err := run1(t, st, x, 0); err == nil || res.Posts != 0 || held(st) != "1000-971" {
		t.Fatalf("stopped fetch = %+v, %v, held %s", res, err, held(st))
	}
	x.failAt = 0
	res, err := run1(t, st, x, 0)
	if err != nil || res.Posts != 25 || res.Stopped != "caught-up" || held(st) != "1025-971" {
		t.Fatalf("fetch after it = %+v, %v, held %s", res, err, held(st))
	}
}

func TestDeepFetchContinues(t *testing.T) {
	x := &fakeX{posts: likes(1000, 100), size: 10, failAt: 4}
	st := store(t, State{User: "me", Boundary: []string{"945"}})
	if _, err := run1(t, st, x, 0); err == nil || held(st) != "1000-971" || st.State.Done || st.State.Cursor != "971" {
		t.Fatalf("stopped: %v, held %s, state %+v", err, held(st), st.State)
	}
	// Two new likes, then where it stopped: no page between is read again.
	x.failAt = 0
	x.posts = append(likes(1002, 2), x.posts...)
	res, err := run1(t, st, x, 0)
	if err != nil || res.Stopped != "boundary" || held(st) != "1002-946" || !st.State.Done {
		t.Fatalf("continued = %+v, %v, held %s", res, err, held(st))
	}
	if got := strings.Join(x.calls, ","); got != ",993,971,961,951" {
		t.Errorf("pages read: %q", got)
	}

	// A cursor X no longer knows: the walk starts over from the top.
	stale := store(t, State{User: "me", Boundary: []string{"945"}, Cursor: "gone"})
	if err := stale.Add(itemsOf(like(1002, "photo"), false)); err != nil {
		t.Fatal(err)
	}
	x.size = 1
	res, err = run1(t, stale, x, 0)
	if err != nil || res.Stopped != "boundary" || held(stale) != "1002-946" {
		t.Fatalf("stale cursor = %+v, %v, held %s", res, err, held(stale))
	}
}

func TestBackfill(t *testing.T) {
	posts := likes(1000, 40)
	posts[20] = like(980, "photo", "video") // older than the boundary
	posts[30] = like(970, "animated_gif")   // older still
	posts[5] = like(995, "photo", "stream") // newer: a video without an MP4
	posts[6] = like(994)                    // newer: no media
	x := &fakeX{posts: posts, size: 10}
	st := store(t, State{User: "me", Boundary: []string{"985"}, Backfill: true})
	res, err := run1(t, st, x, 0)
	if err != nil || res.Stopped != "end" || res.Posts != 40 || res.NoMP4 != 1 {
		t.Fatalf("backfill = %+v, %v", res, err)
	}
	// 14 photos of the 15 newer posts, and the two videos older than the boundary.
	if res.Media != 16 || !st.State.BackfillDone || !st.State.Done || len(st.State.Fence) != 0 {
		t.Errorf("media %d, state %+v", res.Media, st.State)
	}
	var files []string
	for _, it := range st.Items {
		id, _ := strconv.Atoi(it.Post)
		if id <= 985 && it.Skip == "" {
			files = append(files, it.File)
		}
		if id <= 985 && it.Kind == "photo" && it.Skip != skipOld {
			t.Errorf("an old photo is to be downloaded: %+v", it)
		}
	}
	if got := strings.Join(files, " "); got != "2026-09-21_mo13tto_980_p1.mp4 2026-09-21_mo13tto_970_p0.mp4" {
		t.Errorf("old media = %s", got)
	}
	// After it, the boundary stops the fetch again.
	x.posts = append(likes(1011, 11), x.posts...)
	x.hidden = map[string]bool{"1000": true}
	st2 := store(t, st.State)
	if res, err := run1(t, st2, x, 0); err != nil || res.Stopped != "boundary" || res.Posts != 25 {
		t.Fatalf("after the backfill = %+v, %v", res, err)
	}
}

func TestLimitAndEmptyPages(t *testing.T) {
	x := &fakeX{posts: likes(1000, 60), size: 10}
	st := store(t, State{User: "me", All: true})
	res, err := run1(t, st, x, 12)
	if err != nil || res.Stopped != "limit" || res.Posts != 12 || !st.State.Done {
		t.Fatalf("limit = %+v, %v", res, err)
	}
	// The next run does not go back for what the limit left.
	if res, err := run1(t, st, x, 0); err != nil || res.Stopped != "caught-up" || res.Posts != 0 || held(st) != "1000-989" {
		t.Fatalf("after the limit = %+v, %v, held %s", res, err, held(st))
	}

	// X leaves a whole page out: the list goes on after it.
	x.hidden = map[string]bool{}
	for id := 990; id > 980; id-- {
		x.hidden[strconv.Itoa(id)] = true
	}
	all := store(t, State{User: "me", All: true})
	res, err = run1(t, all, x, 0)
	if err != nil || res.Stopped != "end" || res.Posts != 50 || held(all) != "1000-991 980-941" {
		t.Fatalf("with an empty page = %+v, %v, held %s", res, err, held(all))
	}
}
