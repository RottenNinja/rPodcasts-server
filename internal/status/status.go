// Package status keeps the live view of everything running in the background: jobs and streams.
package status

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

type Tracker struct {
	mu      sync.Mutex
	runs    map[int64]*Run
	streams map[int64]*Stream
	ended   []StreamView
	nextID  int64
}

const keepEnded = 50

func New() *Tracker {
	return &Tracker{runs: map[int64]*Run{}, streams: map[int64]*Stream{}}
}

// Run is one job while it runs.
type Run struct {
	JobID     int64
	Kind      string
	Label     string
	Reason    string
	ChannelID int64
	Started   time.Time
	cancel    context.CancelFunc

	mu    sync.Mutex
	stage string
	done  int
	total int
}

// Stage records what the job is doing now, e.g. "fetching dates" 7 of 20.
func (r *Run) Stage(stage string, done, total int) {
	r.mu.Lock()
	r.stage, r.done, r.total = stage, done, total
	r.mu.Unlock()
}

type RunView struct {
	JobID     int64
	Kind      string
	Label     string
	Reason    string
	ChannelID int64
	Stage     string
	Done      int
	Total     int
	Started   time.Time
}

func (v RunView) Percent() int {
	if v.Total <= 0 {
		return 0
	}
	return min(100, v.Done*100/v.Total)
}

func (v RunView) Elapsed() time.Duration { return time.Since(v.Started).Round(time.Second) }

func (t *Tracker) Start(jobID int64, kind, label, reason string, channelID int64, cancel context.CancelFunc) *Run {
	r := &Run{JobID: jobID, Kind: kind, Label: label, Reason: reason, ChannelID: channelID, Started: time.Now(), cancel: cancel, stage: "starting"}
	t.mu.Lock()
	t.runs[jobID] = r
	t.mu.Unlock()
	return r
}

func (t *Tracker) Finish(jobID int64) {
	t.mu.Lock()
	delete(t.runs, jobID)
	t.mu.Unlock()
}

func (t *Tracker) Cancel(jobID int64) bool {
	t.mu.Lock()
	r, ok := t.runs[jobID]
	t.mu.Unlock()
	if ok {
		r.cancel()
	}
	return ok
}

func (t *Tracker) Runs() []RunView {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]RunView, 0, len(t.runs))
	for _, r := range t.runs {
		r.mu.Lock()
		out = append(out, RunView{JobID: r.JobID, Kind: r.Kind, Label: r.Label, Reason: r.Reason, ChannelID: r.ChannelID, Stage: r.stage, Done: r.done, Total: r.total, Started: r.Started})
		r.mu.Unlock()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Started.Before(out[j].Started) })
	return out
}

// Running reports whether a refresh of the channel is running now.
func (t *Tracker) Running(channelID int64) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, r := range t.runs {
		if r.ChannelID == channelID && channelID != 0 {
			return true
		}
	}
	return false
}

// Stream is one downloader engine/ffmpeg stream the server is serving.
type Stream struct {
	ID        int64
	EpisodeID int64
	Title     string
	Mode      string
	Client    string
	Offset    int
	Started   time.Time
	Bytes     atomic.Int64
	cancel    context.CancelFunc
}

type StreamView struct {
	ID        int64
	EpisodeID int64
	Title     string
	Mode      string
	Client    string
	Offset    int
	Bytes     int64
	Started   time.Time
	Ended     time.Time
	Err       string
}

func (v StreamView) Elapsed() time.Duration {
	if !v.Ended.IsZero() {
		return v.Ended.Sub(v.Started).Round(time.Second)
	}
	return time.Since(v.Started).Round(time.Second)
}

func (s *Stream) view() StreamView {
	return StreamView{ID: s.ID, EpisodeID: s.EpisodeID, Title: s.Title, Mode: s.Mode, Client: s.Client, Offset: s.Offset, Bytes: s.Bytes.Load(), Started: s.Started}
}

func (t *Tracker) AddStream(episodeID int64, title, mode, client string, offset int, cancel context.CancelFunc) *Stream {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.nextID++
	s := &Stream{ID: t.nextID, EpisodeID: episodeID, Title: title, Mode: mode, Client: client, Offset: offset, Started: time.Now(), cancel: cancel}
	t.streams[s.ID] = s
	return s
}

// EndStream moves the stream to the recent list; err is nil when it finished or the client left.
func (t *Tracker) EndStream(s *Stream, err error) StreamView {
	v := s.view()
	v.Ended = time.Now()
	if err != nil {
		v.Err = err.Error()
	}
	t.mu.Lock()
	delete(t.streams, s.ID)
	t.ended = append([]StreamView{v}, t.ended...)
	if len(t.ended) > keepEnded {
		t.ended = t.ended[:keepEnded]
	}
	t.mu.Unlock()
	return v
}

// EndedStreams lists the most recent finished or failed streams, newest first.
func (t *Tracker) EndedStreams() []StreamView {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]StreamView(nil), t.ended...)
}

// StreamFailed reports whether the latest ended stream failed, for the nav's red dot.
func (t *Tracker) StreamFailed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.ended) > 0 && t.ended[0].Err != ""
}

func (t *Tracker) StopStream(id int64) bool {
	t.mu.Lock()
	s, ok := t.streams[id]
	t.mu.Unlock()
	if ok {
		s.cancel()
	}
	return ok
}

func (t *Tracker) Streams() []StreamView {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]StreamView, 0, len(t.streams))
	for _, s := range t.streams {
		out = append(out, s.view())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Started.Before(out[j].Started) })
	return out
}

// Busy is the number of jobs and streams running, for the nav badge.
func (t *Tracker) Busy() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.runs) + len(t.streams)
}
