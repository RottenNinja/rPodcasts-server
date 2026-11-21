// Package library evaluates filters and playlists over a snapshot of the store, for the web UI and the API alike.
package library

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"podcasts/internal/filter"
	"podcasts/internal/store"
)

type View struct {
	*store.Library
	Env     filter.Env
	Items   []filter.Item
	Prefs   store.Prefs
	members map[int64][]int
}

type Loader struct {
	st    *store.Store
	mu    sync.Mutex
	last  *View
	built time.Time
}

func NewLoader(st *store.Store) *Loader { return &Loader{st: st} }

// Load returns the current view, rebuilt when the library changed or a minute passed.
func (l *Loader) Load(ctx context.Context) (*View, error) {
	lib, err := l.st.Library(ctx)
	if err != nil {
		return nil, err
	}
	prefs, err := l.st.Prefs(ctx)
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.last != nil && l.last.Seq == lib.Seq && l.last.Prefs == prefs && time.Since(l.built) < time.Minute {
		return l.last, nil
	}
	v := build(lib, prefs)
	l.last, l.built = v, time.Now()
	return v, nil
}

// UniqueLabels gives every option a label no other option shares, so labels read back to one id.
func UniqueLabels(opts []filter.Option) []filter.Option {
	count := map[string]int{}
	for _, o := range opts {
		count[strings.ToLower(o.Label)]++
	}
	seen := map[string]int{}
	out := make([]filter.Option, len(opts))
	for i, o := range opts {
		k := strings.ToLower(o.Label)
		out[i] = o
		if count[k] > 1 {
			seen[k]++
			if seen[k] > 1 {
				out[i].Label = o.Label + " (" + strconv.Itoa(seen[k]) + ")"
			}
		}
	}
	return out
}

func build(lib *store.Library, prefs store.Prefs) *View {
	v := &View{Library: lib, Prefs: prefs, members: map[int64][]int{}}
	v.Env = filter.Env{Now: time.Now(), Loc: prefs.Location()}
	chIDs := make([]int64, 0, len(lib.Channels))
	for id := range lib.Channels {
		chIDs = append(chIDs, id)
	}
	sort.Slice(chIDs, func(i, j int) bool {
		return strings.ToLower(lib.Channels[chIDs[i]].Name()) < strings.ToLower(lib.Channels[chIDs[j]].Name())
	})
	var chOpts []filter.Option
	for _, id := range chIDs {
		chOpts = append(chOpts, filter.Option{ID: strconv.FormatInt(id, 10), Label: lib.Channels[id].Name()})
	}
	v.Env.Channels = UniqueLabels(chOpts)
	v.Items = make([]filter.Item, len(lib.Episodes))
	for i, e := range lib.Episodes {
		ch := lib.Channels[e.ChannelID]
		p := lib.Plays[e.ID]
		v.Items[i] = filter.Item{Title: e.Title, Channel: strconv.FormatInt(e.ChannelID, 10), ChannelName: ch.Name(), Length: int64(e.Duration),
			Published: e.Published.Unix(), Media: e.Media, Season: int64(e.Season), Number: int64(e.Number), Type: e.Type, Position: int64(p.Position),
			Played: p.Played, Starred: p.Starred, Tags: ch.Tags}
		if e.Published.IsZero() {
			v.Items[i].Published = 0
		}
	}
	// $playlist counts the listener's own playlists that don't use it themselves.
	for _, p := range lib.Playlists {
		f, err := filter.Parse(p.Filter, v.Env)
		if p.Builtin != "" || err != nil || f.UsesPlaylist {
			continue
		}
		for i := range v.Items {
			if f.Match(&v.Items[i]) {
				v.Items[i].InPlaylist = true
			}
		}
	}
	return v
}

// Parse reads a filter as the listener types it (labels, not ids).
func (v *View) Parse(text string) (*filter.Filter, *filter.Error) { return filter.Parse(text, v.Env) }

func (v *View) Stored(display string) (string, *filter.Error) { return filter.ToStored(display, v.Env) }

func (v *View) Display(stored string) string { return filter.ToDisplay(stored, v.Env) }

// Match returns the indexes of the matching episodes, newest first.
func (v *View) Match(f *filter.Filter) []int {
	var out []int
	for i := range v.Items {
		if f.Match(&v.Items[i]) {
			out = append(out, i)
		}
	}
	return out
}

// shuffleKey places an episode in a playlist's shuffle; app/lib/data/library.dart has the same sum.
func shuffleKey(seed, id int64) uint32 {
	var h uint32
	for _, n := range [2]int64{id, seed} {
		for s := 0; s < 32; s += 8 {
			h += uint32(n>>s) & 0xff
			h += h << 10
			h ^= h >> 6
		}
	}
	h += h << 3
	h ^= h >> 11
	h += h << 15
	return h
}

// Sort orders idx; seed fixes the shuffle order, so it only changes when the seed does.
func (v *View) Sort(idx []int, how string, manual map[int64]float64, seed int64) {
	eps := v.Episodes
	if how == "shuffle" {
		keys := make(map[int]uint32, len(idx))
		for _, i := range idx {
			keys[i] = shuffleKey(seed, eps[i].ID)
		}
		sort.Slice(idx, func(i, j int) bool {
			a, b := idx[i], idx[j]
			if keys[a] != keys[b] {
				return keys[a] < keys[b]
			}
			return eps[a].ID < eps[b].ID
		})
		return
	}
	pub := func(i int) int64 { return eps[i].Published.Unix() }
	less := map[string]func(a, b int) bool{
		"newest":   func(a, b int) bool { return pub(a) > pub(b) },
		"oldest":   func(a, b int) bool { return pub(a) < pub(b) },
		"shortest": func(a, b int) bool { return eps[a].Duration < eps[b].Duration },
		"longest":  func(a, b int) bool { return eps[a].Duration > eps[b].Duration },
		"manual": func(a, b int) bool {
			pa, oka := manual[eps[a].ID]
			pb, okb := manual[eps[b].ID]
			switch {
			case oka && okb:
				return pa < pb
			case oka != okb:
				return oka
			}
			return pub(a) < pub(b)
		},
	}[how]
	if less == nil {
		less = func(a, b int) bool { return pub(a) > pub(b) }
	}
	sort.SliceStable(idx, func(i, j int) bool { return less(idx[i], idx[j]) })
}

// Playlist evaluates a playlist's stored filter in its show order.
func (v *View) Playlist(p store.Playlist, manual map[int64]float64) ([]int, *filter.Error) {
	f, err := filter.Parse(p.Filter, v.Env)
	if err != nil {
		return nil, err
	}
	idx := v.Match(f)
	v.Sort(idx, p.Sort, manual, p.ShuffleSeed())
	return idx, nil
}

// PlaylistIDs are the ids of a playlist's episodes, in no set order.
func (v *View) PlaylistIDs(p store.Playlist) ([]int64, *filter.Error) {
	idx, err := v.Playlist(p, nil)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, len(idx))
	for i, x := range idx {
		ids[i] = v.Episodes[x].ID
	}
	return ids, nil
}

func (v *View) Done(i int) bool { return v.Plays[v.Episodes[i].ID].Played }

func (v *View) NotDone(idx []int) int {
	n := 0
	for _, i := range idx {
		if !v.Done(i) {
			n++
		}
	}
	return n
}

// StartOf picks what Play plays: the first unplayed episode in show order.
func (v *View) StartOf(idx []int) (int, bool) {
	for _, i := range idx {
		if !v.Done(i) {
			return i, true
		}
	}
	return 0, false
}

// NextAfter is the next unplayed episode after id in an ordered list.
func (v *View) NextAfter(idx []int, id int64) (int, bool) {
	seen := false
	for _, i := range idx {
		if seen && !v.Done(i) && v.Episodes[i].ID != id {
			return i, true
		}
		if v.Episodes[i].ID == id {
			seen = true
		}
	}
	return 0, false
}

func (v *View) Channel(id int64) store.Channel { return v.Channels[id] }

func (v *View) Unplayed(channelID int64) int {
	n := 0
	for i, e := range v.Episodes {
		if e.ChannelID == channelID && !v.Done(i) {
			n++
		}
	}
	return n
}
