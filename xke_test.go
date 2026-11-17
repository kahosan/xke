package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"xke/backend"
)

func TestItemsAndNames(t *testing.T) {
	p := backend.Post{ID: "2101996669818777737", Author: "mo13tto", Text: "hi",
		Created: time.Date(2026, 9, 21, 11, 27, 13, 0, time.UTC),
		Media: []backend.Media{
			{Kind: "photo", Ext: "jpg", URL: "https://pbs.twimg.com/media/A?format=jpg&name=orig", Key: "photo:A"},
			{Kind: "photo", Ext: "png", URL: "https://pbs.twimg.com/media/B?format=png&name=orig"},
			{Kind: "video", Ext: "mp4"},
			{Kind: "animated_gif", Ext: "mp4", URL: "https://video.twimg.com/tweet_video/G.mp4"},
		}}
	items := itemsOf(p, false)
	want := []string{
		"2026-09-21_mo13tto_2101996669818777737_p0.jpg",
		"2026-09-21_mo13tto_2101996669818777737_p1.png",
		"",
		"2026-09-21_mo13tto_2101996669818777737_p3.mp4",
	}
	for i, it := range items {
		if it.File != want[i] || it.Index != i || it.Date != "2026-09-21T11:27:13Z" {
			t.Errorf("item %d = %+v, want file %q", i, it, want[i])
		}
	}
	if items[2].Skip != skipNoMP4 || items[0].Skip != "" || items[0].Text != "hi" || items[1].Text != "" {
		t.Errorf("items = %+v", items)
	}
	// The day in a name is Shanghai's: 16:00 UTC is the next day there.
	for when, day := range map[string]string{"2026-09-20T15:59:59Z": "2026-09-20", "2026-09-20T16:00:00Z": "2026-09-21"} {
		p.Created, _ = time.Parse(time.RFC3339, when)
		if got := fileName(p, 0, "jpg"); !strings.HasPrefix(got, day+"_") {
			t.Errorf("posted %s: %s", when, got)
		}
	}
	// Past the boundary the photos stay, the videos are wanted.
	old := itemsOf(p, true)
	if old[0].Skip != skipOld || old[0].File != "" || old[3].Skip != "" || old[3].File == "" {
		t.Errorf("old items = %+v", old)
	}
	if none := itemsOf(backend.Post{ID: "5", Author: "a"}, false); len(none) != 1 || none[0].Kind != "none" || none[0].Skip != skipNoMedia {
		t.Errorf("a post without media = %+v", none)
	}
	odd := fileName(backend.Post{ID: "7", Author: "../../etc"}, 0, "j/pg")
	if strings.ContainsAny(odd, `/\`) || !strings.HasSuffix(odd, "_etc_7_p0.jpg") {
		t.Errorf("unsafe name %q", odd)
	}
}

func TestPrepare(t *testing.T) {
	st := store(t, State{})
	if err := prepare(st, "", nil, false, false); err == nil {
		t.Error("a first fetch without a handle was accepted")
	}
	if err := prepare(st, "@me", nil, false, false); err == nil {
		t.Error("a first fetch without --until or --all was accepted")
	}
	if err := prepare(st, "", []string{"nope"}, false, false); err == nil {
		t.Error("a boundary that is no post was accepted")
	}
	err := prepare(st, "", []string{"https://x.com/mo13tto/status/2101996669818777737?s=20", "2101996669818777737", " 123456 "}, false, false)
	if err != nil || strings.Join(st.State.Boundary, " ") != "2101996669818777737 123456" || st.State.User != "me" {
		t.Fatalf("state %+v, %v", st.State, err)
	}
	if err := prepare(st, "", nil, true, false); err == nil {
		t.Error("--all with a boundary was accepted")
	}
	if err := prepare(st, "other", nil, false, false); err == nil {
		t.Error("another account in the same directory was accepted")
	}
	// The later runs take no flags; --backfill starts the deep fetch again.
	st.State.Done = true
	if err := prepare(st, "", nil, false, false); err != nil || !st.State.Done {
		t.Fatalf("a run without flags: %v, %+v", err, st.State)
	}
	if err := prepare(st, "", nil, false, true); err != nil || st.State.Done || !st.State.Backfill {
		t.Fatalf("--backfill: %v, %+v", err, st.State)
	}

	// What is saved is what the next run opens, and one xke at a time.
	if _, err := Open(st.Dir); err == nil {
		t.Error("a second xke opened the directory")
	}
	if err := st.Add(itemsOf(like(9, "photo", "photo"), false)); err != nil {
		t.Fatal(err)
	}
	st.Close()
	again, err := Open(st.Dir)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if !again.Known("9") || len(again.Items) != 2 || !again.State.Backfill || again.State.User != "me" {
		t.Errorf("reopened: %+v, %d items", again.State, len(again.Items))
	}
}

func TestParseCookie(t *testing.T) {
	for raw, want := range map[string][2]string{
		" abc123\n":               {"abc123", ""},
		"auth_token=abc; ct0=def": {"abc", "def"},
		"Cookie: lang=en; ct0=def; auth_token=abc; twid=u%3D1": {"abc", "def"},
		"lang=en": {"", ""},
		"":        {"", ""},
	} {
		if token, ct0 := parseCookie(raw); token != want[0] || ct0 != want[1] {
			t.Errorf("parseCookie(%q) = %q, %q", raw, token, ct0)
		}
	}
}

func TestDownload(t *testing.T) {
	var missing atomic.Bool
	missing.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/c.jpg" && missing.Load() {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte("media " + r.URL.Path))
	}))
	defer srv.Close()
	st := store(t, State{})
	posted := "2026-09-21T11:27:13Z"
	err := st.Add([]Item{
		{Post: "1", Kind: "photo", File: "a.jpg", URL: srv.URL + "/a.jpg", Date: posted},
		{Post: "2", Kind: "photo", File: "b.jpg", URL: srv.URL + "/b.jpg", Date: posted},
		{Post: "3", Kind: "photo", File: "c.jpg", URL: srv.URL + "/c.jpg", Date: posted},
		{Post: "4", Kind: "photo", URL: srv.URL + "/d.jpg", Skip: skipOld},
		{Post: "5", Kind: "none", Skip: skipNoMedia},
	})
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(st.Dir, "likes")
	os.MkdirAll(out, 0o755)
	os.WriteFile(filepath.Join(out, "b.jpg"), []byte("the script's"), 0o644) // downloaded before xke
	run := func() downloadResult {
		t.Helper()
		res, err := download(context.Background(), st, downloadOptions{Out: out, Jobs: 2}, func(string, ...any) {})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if res := run(); res != (downloadResult{Wanted: 3, Downloaded: 1, Had: 1, Failed: 1}) {
		t.Fatalf("first run = %+v", res)
	}
	if data, _ := os.ReadFile(filepath.Join(out, "a.jpg")); string(data) != "media /a.jpg" {
		t.Errorf("a.jpg = %q", data)
	}
	info, err := os.Stat(filepath.Join(out, "a.jpg"))
	if want, _ := time.Parse(time.RFC3339, posted); err != nil || !info.ModTime().Equal(want) {
		t.Errorf("a.jpg is dated %v, want %v", info.ModTime(), want)
	}
	if data, _ := os.ReadFile(filepath.Join(out, "b.jpg")); string(data) != "the script's" {
		t.Errorf("b.jpg was replaced: %q", data)
	}
	if names, _ := filepath.Glob(filepath.Join(out, "*")); len(names) != 2 {
		t.Errorf("the folder holds %v", names)
	}
	// What failed is tried again, what was downloaded is not.
	if res := run(); res != (downloadResult{Wanted: 1, Failed: 1}) {
		t.Fatalf("second run = %+v", res)
	}
	missing.Store(false)
	if res := run(); res != (downloadResult{Wanted: 1, Downloaded: 1}) {
		t.Fatalf("third run = %+v", res)
	}
	os.Remove(filepath.Join(out, "a.jpg")) // moved away: not downloaded again
	if res := run(); res != (downloadResult{}) {
		t.Fatalf("fourth run = %+v", res)
	}
}
