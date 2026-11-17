package backend

import (
	"testing"
	"time"
)

func TestLikedPosts(t *testing.T) {
	page, err := parseTimelinePage([]byte(mediaPage))
	if err != nil {
		t.Fatal(err)
	}
	posts := likedPosts(page.posts)
	if len(posts) != 3 {
		t.Fatalf("got %d posts", len(posts))
	}
	pinned, video, gif := posts[0], posts[1], posts[2]
	if pinned.ID != "1" || pinned.Author != "alice" || !pinned.Created.Equal(time.Date(2026, 11, 10, 11, 43, 42, 0, time.UTC)) {
		t.Errorf("pinned = %+v", pinned)
	}
	if m := pinned.Media[0]; m != (Media{Kind: "photo", URL: "https://pbs.twimg.com/media/ABC?format=jpg&name=orig", Ext: "jpg", Key: "photo:ABC"}) {
		t.Errorf("photo = %+v", m)
	}
	if m := video.Media[0]; m != (Media{Kind: "video", URL: "https://video.twimg.com/ext_tw_video/200/pu/vid/avc1/1280x720/l.mp4", Ext: "mp4", Key: "video:200"}) {
		t.Errorf("video = %+v", m)
	}
	// A like of a repost is a like of the post itself.
	if m := gif.Media[0]; gif.ID != "5" || gif.Author != "bob" || m != (Media{Kind: "animated_gif", URL: "https://video.twimg.com/tweet_video/G.mp4", Ext: "mp4", Key: "gif:G"}) {
		t.Errorf("gif = %+v", gif)
	}

	// A video X serves as a stream alone: no playlist or poster in its place.
	stream := likedMedia(attachment{Kind: "video", ID: "9", Image: "https://pbs.twimg.com/ext_tw_video_thumb/9/pu/img/T.jpg",
		Clips: []clip{{ContentType: "application/x-mpegURL", URL: "https://video.twimg.com/ext_tw_video/9/pu/pl/x.m3u8"}}}, 3)
	if stream != (Media{Kind: "video", Ext: "mp4", Key: "video:9"}) {
		t.Errorf("stream = %+v", stream)
	}
	if photo := likedMedia(attachment{Kind: "photo", ID: "8"}, 3); photo.URL != "" || photo.Key != "photo:8" {
		t.Errorf("a photo without an address = %+v", photo)
	}

	// A date that does not read: the time in the post's ID.
	if got := postCreated("", 2101996669818777737).Format(time.DateTime); got != "2026-09-21 11:27:13" {
		t.Errorf("time of the ID = %s", got)
	}
	if !postCreated("nonsense", 0).IsZero() {
		t.Error("a time out of nothing")
	}
}
