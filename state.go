package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

// xke's files, all in the data directory:
//
//	state.json        the account, the boundary, and where an unfinished fetch continues
//	likes.jsonl       the manifest: a line per media item of every liked post xke saw
//	downloaded.jsonl  a line per file downloaded
//	token             the auth token (0600), unless given another way
//	likes/            the media

// State is state.json.
type State struct {
	User string `json:"user"` // the handle whose likes xke reads
	// Boundary is the posts given with --until: the fetch stops at the
	// first it meets, for what is older was downloaded before xke. Fence
	// is the posts that followed it on its page, boundaries as well, so
	// that the fetch still stops there once the boundary post is gone.
	Boundary []string `json:"boundary,omitempty"`
	Fence    []string `json:"fence,omitempty"`
	All      bool     `json:"all,omitempty"` // no boundary: the whole list
	// Backfill wants the videos and GIFs older than the boundary too.
	Backfill     bool `json:"backfill,omitempty"`
	BackfillDone bool `json:"backfill_done,omitempty"`
	// Done says the first, deep fetch came to its end; runs after it read
	// what is new. Until then Cursor is where it continues, and CursorOld
	// whether that is past the boundary.
	Done      bool   `json:"done"`
	Cursor    string `json:"cursor,omitempty"`
	CursorOld bool   `json:"cursor_old,omitempty"`
}

func (s *State) isBoundary(id string) bool {
	for _, list := range [][]string{s.Boundary, s.Fence} {
		for _, b := range list {
			if b == id {
				return true
			}
		}
	}
	return false
}

// Reasons an item is in the manifest without being downloaded.
const (
	skipNoMedia = "no-media"        // a post without photos or videos
	skipNoMP4   = "no-mp4"          // a video X serves as a stream only
	skipNoURL   = "no-url"          // a photo X gave no address of
	skipOld     = "before-boundary" // a photo older than the boundary
)

// Item is a line of the manifest: one media item of a liked post, or the
// post itself (Kind none) when it has none.
type Item struct {
	Post   string `json:"post"`
	Author string `json:"author"`
	Date   string `json:"date,omitempty"` // when it was posted, RFC 3339 in UTC
	Index  int    `json:"index"`          // the item's place in the post, from 0
	Kind   string `json:"kind"`           // photo, video, animated_gif, none
	URL    string `json:"url,omitempty"`
	Key    string `json:"key,omitempty"`  // X's ID of the media
	File   string `json:"file,omitempty"` // its name in likes/
	Skip   string `json:"skip,omitempty"` // why it is not downloaded
	Text   string `json:"text,omitempty"` // the post's text, on its first line
}

// Record is a line of downloaded.jsonl.
type Record struct {
	File string `json:"file"`
	Post string `json:"post"`
	At   string `json:"at"`
	Had  bool   `json:"had,omitempty"` // the file was there already
}

// Store is the data directory.
type Store struct {
	Dir   string
	State State
	Items []Item
	known map[string]bool // the posts in the manifest

	mu   sync.Mutex
	lock *os.File
}

// Open reads the data directory, making it when it is not there, and
// holds it against another xke until Close.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	s := &Store{Dir: dir, known: map[string]bool{}}
	lock, err := os.OpenFile(s.path(".lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("another xke is using %s", dir)
	}
	s.lock = lock
	data, err := os.ReadFile(s.path("state.json"))
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(data, &s.State); err != nil {
			return nil, fmt.Errorf("%s: %w", s.path("state.json"), err)
		}
	}
	err = readLines(s.path("likes.jsonl"), func(line []byte) {
		var it Item
		if json.Unmarshal(line, &it) == nil && it.Post != "" {
			s.Items = append(s.Items, it)
			s.known[it.Post] = true
		}
	})
	return s, err
}

func (s *Store) Close() {
	if s.lock != nil {
		s.lock.Close()
	}
}

func (s *Store) path(name string) string { return filepath.Join(s.Dir, name) }

// Known reports whether the manifest holds a post.
func (s *Store) Known(post string) bool { return s.known[post] }

// SaveState writes state.json, whole or not at all.
func (s *Store) SaveState() error {
	data, err := json.MarshalIndent(s.State, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path("state.json.tmp")
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path("state.json"))
}

// Add appends a post's items to the manifest.
func (s *Store) Add(items []Item) error {
	if len(items) == 0 {
		return nil
	}
	if err := appendLines(s.path("likes.jsonl"), items); err != nil {
		return err
	}
	s.Items = append(s.Items, items...)
	for _, it := range items {
		s.known[it.Post] = true
	}
	return nil
}

// Downloaded returns the files downloaded.jsonl holds.
func (s *Store) Downloaded() (map[string]bool, error) {
	done := map[string]bool{}
	err := readLines(s.path("downloaded.jsonl"), func(line []byte) {
		var r Record
		if json.Unmarshal(line, &r) == nil && r.File != "" {
			done[r.File] = true
		}
	})
	return done, err
}

// MarkDownloaded adds a file to downloaded.jsonl; download workers call it.
func (s *Store) MarkDownloaded(it Item, had bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return appendLines(s.path("downloaded.jsonl"), []Record{{
		File: it.File, Post: it.Post, At: time.Now().UTC().Format(time.RFC3339), Had: had,
	}})
}

// readLines calls fn with each line of a file; a missing file has none.
func readLines(path string, fn func([]byte)) error {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		if len(sc.Bytes()) > 0 {
			fn(sc.Bytes())
		}
	}
	return sc.Err()
}

func appendLines[T any](path string, values []T) error {
	var b strings.Builder
	for _, v := range values {
		line, err := json.Marshal(v)
		if err != nil {
			return err
		}
		b.Write(line)
		b.WriteByte('\n')
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	_, err = f.WriteString(b.String())
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

var rePostID = regexp.MustCompile(`(?:^|/status(?:es)?/)(\d{5,25})(?:$|[/?#])`)

// postID reads a post's ID from the ID itself or from a link to the post.
func postID(s string) (string, error) {
	if m := rePostID.FindStringSubmatch(strings.TrimSpace(s)); m != nil {
		return m[1], nil
	}
	return "", fmt.Errorf("%q is not a post ID or a link to a post", s)
}
