package backend

// What xke adds to the files copied from XDL (the commit in UPSTREAM): the
// posts an account liked, as xke reads them, and a download through the
// resumable engine. The other files here are XDL's, unchanged, so that a
// fix upstream is a copy away (xtimeline_test.go alone lost the test of a
// file xke does not take); downloadFileWithRetry and MediaKey are XDL's
// too, from files xke does not take whole.

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Media is one photo, video or GIF of a post.
type Media struct {
	Kind string // photo, video, animated_gif
	URL  string // the original picture, or the best MP4; "" when X serves none
	Ext  string // jpg, png… or mp4
	Key  string // the same whatever the size or quality: see MediaKey
}

// Post is a liked post.
type Post struct {
	ID      string
	Author  string // the handle
	Text    string
	Created time.Time // zero when X's date does not read
	Media   []Media
}

// Client reads X as the account of an auth token.
type Client struct{ s *xSession }

// NewClient returns a client for an auth_token cookie. ct0, X's CSRF
// cookie, may be "": X sets one on the first request.
func NewClient(authToken, ct0, proxy string) (*Client, error) {
	s, err := getXSession(authToken, proxy)
	if err != nil {
		return nil, err
	}
	if ct0 != "" {
		s.mu.Lock()
		s.csrf = ct0
		s.mu.Unlock()
	}
	return &Client{s: s}, nil
}

// Likes returns one page of the posts handle liked, the latest like first,
// and the cursor of the next page, "" after the last one. A page can be
// empty with more to come, when X leaves out every post of it.
func (c *Client) Likes(ctx context.Context, handle, cursor string) ([]Post, string, error) {
	u, err := c.s.lookupUser(ctx, cleanUsername(handle))
	if err != nil {
		return nil, "", err
	}
	pg, err := c.s.timelinePage(ctx, "likes", u.RestID, 100, cursor)
	if err != nil {
		return nil, "", err
	}
	next := pg.next
	if next == cursor {
		next = ""
	}
	return likedPosts(pg.posts), next, nil
}

func likedPosts(posts []post) []Post {
	out := make([]Post, 0, len(posts))
	for _, p := range posts {
		id, _ := strconv.ParseInt(strings.TrimSpace(p.ID), 10, 64)
		liked := Post{ID: p.ID, Author: p.Author.Handle, Text: p.Text, Created: postCreated(p.CreatedAt, id)}
		for _, m := range p.Media {
			liked.Media = append(liked.Media, likedMedia(m, id))
		}
		out = append(out, liked)
	}
	return out
}

// likedMedia picks what xke downloads of an attachment: the picture as it
// was uploaded, or the MP4 of the highest bitrate. A video without an MP4
// (X serves some as HLS only) gets no URL rather than its playlist.
func likedMedia(m attachment, postID int64) Media {
	out := Media{Kind: m.Kind}
	if m.Kind == "photo" {
		if m.Image != "" {
			out.URL = sizedImage(m.Image, "orig")
		}
		out.Ext = imageFormat(out.URL)
	} else {
		out.Ext = "mp4"
		for _, c := range m.Clips {
			if c.ContentType == "video/mp4" && c.URL != "" {
				out.URL = pickClip(m, "highest")
				break
			}
		}
	}
	if out.URL != "" {
		out.Key = MediaKey(out.URL, postID, m.Kind)
	} else {
		out.Key = m.Kind + ":" + firstNonEmpty(m.ID, m.Key, strconv.FormatInt(postID, 10))
	}
	return out
}

// twitterEpoch is the millisecond post IDs count from.
const twitterEpoch = 1288834974657

// postCreated reads X's date of a post, or the time in its ID.
func postCreated(date string, id int64) time.Time {
	if t, err := time.Parse(time.RubyDate, strings.TrimSpace(date)); err == nil {
		return t.UTC()
	}
	if id > 0 {
		return time.UnixMilli(id>>22 + twitterEpoch).UTC()
	}
	return time.Time{}
}

// Download saves url at path through the resumable engine: what a stop or
// a failure leaves in path.part, the next call continues.
func Download(ctx context.Context, client *http.Client, url, path string, retries int) error {
	return downloadFileWithRetry(ctx, client, url, path, DownloadOptions{RetryAttempts: retries})
}

// DownloadOptions are the settings a download follows.
type DownloadOptions struct {
	SkipExistingFiles     bool
	DeleteIncompleteFiles bool
	RetryAttempts         int
}

const maxRetries = 5

func (o DownloadOptions) retries() int { return min(max(o.RetryAttempts, 0), maxRetries) }

// downloadFileWithRetry downloads a file, each retry continuing where the
// last try stopped. A stop keeps what came, for the next run to continue;
// a file that fails every try loses it when the options say so.
func downloadFileWithRetry(ctx context.Context, client *http.Client, url, path string, opts DownloadOptions) error {
	if err := ensureDir(path); err != nil {
		return err
	}
	var err error
	for attempt := 0; attempt <= opts.retries(); attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(time.Duration(attempt) * 500 * time.Millisecond):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if err = fetchToPart(ctx, client, url, path); err == nil {
			if err = ctx.Err(); err != nil {
				return err
			}
			return os.Rename(partPath(path), path)
		}
		if ctx.Err() != nil || errors.Is(err, context.Canceled) {
			return err
		}
	}
	if opts.DeleteIncompleteFiles {
		removeParts(path)
	}
	return err
}

var numericID = regexp.MustCompile(`^[0-9]+$`)

// MediaKey identifies a media item independently of its size or quality:
//
//	pbs.twimg.com/media/<id>.jpg?name=orig        photo:<id>
//	video.twimg.com/amplify_video/<id>/…/x.mp4    video:<id>
//	video.twimg.com/ext_tw_video/<id>/…/x.mp4     video:<id>
//	video.twimg.com/tweet_video/<name>.mp4        gif:<name>
//	a text tweet                                  text:<tweet id>
func MediaKey(mediaURL string, tweetID int64, mediaType string) string {
	if mediaType == "text" {
		return "text:" + strconv.FormatInt(tweetID, 10)
	}
	u, err := url.Parse(mediaURL)
	if err != nil || u.Host == "" {
		return "url:" + mediaURL
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	base := strings.TrimSuffix(path.Base(u.Path), path.Ext(u.Path))
	switch {
	case strings.HasSuffix(u.Host, "pbs.twimg.com") && len(segs) >= 2 && segs[0] == "media":
		return "photo:" + base
	case strings.HasSuffix(u.Host, "video.twimg.com") && len(segs) >= 2 && segs[0] == "tweet_video":
		return "gif:" + base
	case strings.HasSuffix(u.Host, "video.twimg.com") && len(segs) >= 2 && numericID.MatchString(segs[1]):
		return "video:" + segs[1]
	}
	return "url:" + u.Host + u.Path
}
