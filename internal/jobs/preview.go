package jobs

import (
	"context"
	"errors"
	"fmt"
	"net"
	neturl "net/url"
	"strings"
	"time"

	"podcasts/internal/feed"
	"podcasts/internal/store"
)

// Preview is what an address turned out to hold, shown before it is followed.
type Preview struct {
	// URL is the address that gets followed: the feed itself when a website was given.
	URL         string
	Kind        string
	Title       string
	Author      string
	Artwork     string
	Description string
	// Episodes is how many the address says it holds, 0 when it doesn't say.
	Episodes int
	// Media is "audio", "video" or "mixed".
	Media string
	// Following is the channel that already follows this address, 0 when none does.
	Following int64
}

type previewKey struct{ url, username, password string }

type previewed struct {
	Preview
	at time.Time
}

const previewKept = 10 * time.Minute

// Preview reads an address without following it and refuses one with nothing to play; the answer is kept a while for Follow.
func (m *Manager) Preview(ctx context.Context, n store.NewChannel) (Preview, error) {
	key := previewKey{n.URL, n.Username, n.Password}
	m.previewMu.Lock()
	p, ok := m.previews[key]
	m.previewMu.Unlock()
	if !ok || time.Since(p.at) > previewKept {
		ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
		defer cancel()
		found, err := m.probe(ctx, n)
		if errors.Is(err, context.DeadlineExceeded) {
			return Preview{}, errors.New("the address took too long to answer")
		}
		if err != nil {
			return Preview{}, err
		}
		p = previewed{found, time.Now()}
		m.previewMu.Lock()
		for k, old := range m.previews {
			if time.Since(old.at) > previewKept {
				delete(m.previews, k)
			}
		}
		m.previews[key] = p
		m.previewMu.Unlock()
	}
	for _, u := range []string{p.URL, n.URL} {
		if ch, err := m.st.ChannelByURL(ctx, u); err == nil {
			p.Following = ch.ID
			break
		}
	}
	return p.Preview, nil
}

// probe is the first read of an address, kept to what a preview shows.
func (m *Manager) probe(ctx context.Context, n store.NewChannel) (Preview, error) {
	if !feed.IsWeb(n.URL) {
		return m.probeVideos(ctx, n.URL)
	}
	url := n.URL
	for hop := 0; hop < 3; hop++ {
		res, err := m.feed.Fetch(ctx, feed.Request{URL: url, Username: n.Username, Password: n.Password, Limit: 400})
		if err != nil {
			if errors.Is(err, feed.ErrPrivate) || ctx.Err() != nil {
				return Preview{}, err
			}
			// An address that refuses a plain fetch may still be a page of videos the downloader engine can read.
			p, verr := m.probeVideos(ctx, n.URL)
			if !errors.Is(verr, errNothing) {
				return p, verr
			}
			if de := (*net.DNSError)(nil); errors.As(err, &de) {
				return Preview{}, fmt.Errorf("the address can't be read: there is no site called %s", de.Name)
			}
			if ue := (*neturl.Error)(nil); errors.As(err, &ue) {
				err = ue.Err
			}
			return Preview{}, fmt.Errorf("the address can't be read: %v", err)
		}
		if res.Feed == nil {
			if len(res.FeedLinks) > 0 && hop < 2 {
				url = res.FeedLinks[0]
				continue
			}
			break
		}
		f := res.Feed
		if f.NewFeedURL != "" && f.NewFeedURL != res.URL && hop < 2 {
			url = f.NewFeedURL
			continue
		}
		audio, video := 0, 0
		for _, it := range f.Items {
			switch {
			case it.MediaURL == "":
			case it.Media == "audio":
				audio++
			default:
				video++
			}
		}
		if audio+video == 0 {
			break
		}
		p := Preview{URL: res.URL, Kind: "feed", Title: f.Title, Author: f.Author, Artwork: f.Artwork, Description: f.Description,
			Episodes: max(f.Total, audio+video), Media: "mixed"}
		switch {
		case video == 0:
			p.Media = "audio"
		case audio == 0:
			p.Media = "video"
		}
		return p, nil
	}
	return m.probeVideos(ctx, n.URL)
}

var errNothing = errors.New("nothing to play was found at this address: it is not a podcast feed, and not a page the downloader engine can read")

// probeVideos lists an address with the downloader engine and opens its first entry, which says whether it plays and what it is.
func (m *Manager) probeVideos(ctx context.Context, url string) (Preview, error) {
	m.EnsureEngine(ctx)
	info, err := m.engine.List(ctx, url, 5, nil)
	if err != nil {
		if ctx.Err() != nil {
			return Preview{}, ctx.Err()
		}
		return Preview{}, fmt.Errorf("%w (%s)", errNothing, strings.TrimRight(err.Error(), ": "))
	}
	if len(info.Entries) == 0 {
		return Preview{}, errNothing
	}
	top := info.Entries[0]
	v, err := m.engine.Video(ctx, top.URL)
	if err != nil {
		if ctx.Err() != nil {
			return Preview{}, ctx.Err()
		}
		return Preview{}, fmt.Errorf("the address lists episodes, but the first one can't be opened: %v", err)
	}
	p := Preview{URL: url, Kind: "video", Title: strings.TrimSuffix(info.Title, " - Videos"), Author: info.Author,
		Artwork: first(info.Artwork, top.Thumbnail, v.Thumbnail), Episodes: info.Count, Media: "video"}
	if v.Media == "audio" {
		p.Media = "audio"
	}
	if p.Title == "" {
		p.Title = top.Title
	}
	return p, nil
}

// Follow checks an address as Preview does, then adds the channel and starts its first read.
func (m *Manager) Follow(ctx context.Context, n store.NewChannel) (store.Channel, bool, error) {
	if ch, err := m.st.ChannelByURL(ctx, n.URL); err == nil {
		return ch, false, nil
	}
	p, err := m.Preview(ctx, n)
	if err != nil {
		return store.Channel{}, false, err
	}
	n.URL = p.URL
	if n.Title == p.Title {
		n.Title = ""
	}
	return m.FollowUnchecked(ctx, n)
}
