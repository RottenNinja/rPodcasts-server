// Package engine is the downloader engine: it lists pages of videos, opens one video and resolves an episode's media links.
package engine

import (
	"context"
	"errors"
	"time"
)

// Engine is what the server needs from a downloader engine; another one, or one per kind of site, can stand in for Command.
type Engine interface {
	List(ctx context.Context, url string, limit int, progress func(n int)) (ListInfo, error)
	Video(ctx context.Context, url string) (Video, error)
	Resolve(ctx context.Context, url, quality string) (Resolved, error)
	Version(ctx context.Context) string
	Update(ctx context.Context) error
	// Missing reports whether the engine must be installed before use.
	Missing() bool
	Install(ctx context.Context) error
}

type ListInfo struct {
	Title   string
	Author  string
	Website string
	Artwork string
	Entries []Entry
	// Count is how many the page says it holds, 0 when it doesn't say.
	Count int
	// Locked are the IDs of videos only paying members can open; they are not among Entries.
	Locked []string
}

type Entry struct {
	ID        string
	URL       string
	Title     string
	Duration  int
	Timestamp time.Time
	Thumbnail string
}

type Video struct {
	Title       string
	Published   time.Time
	Duration    int
	Description string
	Thumbnail   string
	Chapters    []Chapter
	// Media is "audio" when nothing in it has a picture, "video" when something has, and empty when the engine can't tell.
	Media string
}

type Chapter struct {
	Start int
	Title string
}

// Format is one resolved media link, with the headers it must be fetched with.
type Format struct {
	URL     string
	Ext     string
	VCodec  string
	ACodec  string
	Headers map[string]string
	Size    int64
	// Chunk is the most bytes the media host serves at full speed in one request; 0 means it has no such limit.
	Chunk int64
}

type Resolved struct {
	Single   *Format
	Video    *Format
	Audio    *Format
	Duration int
	Expires  time.Time
	// Details are what the same answer says about the video itself.
	Details Video
}

// None stands in when no engine is set, so only video channels and streams fail.
type None struct{}

var errNone = errors.New("no downloader engine is set (PODCASTS_ENGINE)")

func (None) List(context.Context, string, int, func(int)) (ListInfo, error) {
	return ListInfo{}, errNone
}
func (None) Video(context.Context, string) (Video, error)              { return Video{}, errNone }
func (None) Resolve(context.Context, string, string) (Resolved, error) { return Resolved{}, errNone }
func (None) Version(context.Context) string                            { return "" }
func (None) Update(context.Context) error                              { return errNone }
func (None) Missing() bool                                             { return false }
func (None) Install(context.Context) error                             { return errNone }
