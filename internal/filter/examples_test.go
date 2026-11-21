package filter

import (
	"encoding/json"
	"os"
	"slices"
	"testing"
	"time"
)

// TestExamples checks the shared examples that the Dart implementation is also checked against.
func TestExamples(t *testing.T) {
	raw, err := os.ReadFile("testdata/examples.json")
	if err != nil {
		t.Fatal(err)
	}
	var ex struct {
		Env struct {
			Now      time.Time
			Zone     string
			Channels []struct {
				ID, Label string
				Tags      []string
			}
		}
		Episodes []struct {
			ID, Title, Channel, Media, Type  string
			Length, Season, Number, Position int64
			Published                        *time.Time
			InPlaylist                       bool
			Played, Starred                  bool
			Downloaded                       bool
		}
		Matches []struct {
			Filter string
			IDs    []string
		}
		Errors []struct {
			Filter  string
			Pos     int
			Message string
		}
		Stored []struct{ Display, Stored, Shown string }
	}
	if err := json.Unmarshal(raw, &ex); err != nil {
		t.Fatal(err)
	}
	loc, err := time.LoadLocation(ex.Env.Zone)
	if err != nil {
		t.Fatal(err)
	}
	env := Env{Now: ex.Env.Now, Loc: loc}
	names, tags := map[string]string{}, map[string][]string{}
	for _, c := range ex.Env.Channels {
		env.Channels = append(env.Channels, Option{ID: c.ID, Label: c.Label})
		names[c.ID], tags[c.ID] = c.Label, c.Tags
	}
	var items []Item
	var ids []string
	for _, e := range ex.Episodes {
		it := Item{Title: e.Title, Channel: e.Channel, ChannelName: names[e.Channel], Length: e.Length, Media: e.Media, Season: e.Season,
			Number: e.Number, Type: e.Type, InPlaylist: e.InPlaylist, Position: e.Position, Played: e.Played, Starred: e.Starred,
			Downloaded: e.Downloaded, Tags: tags[e.Channel]}
		if e.Published != nil {
			it.Published = e.Published.Unix()
		}
		items = append(items, it)
		ids = append(ids, e.ID)
	}
	for _, m := range ex.Matches {
		f, perr := Parse(m.Filter, env)
		if perr != nil {
			t.Errorf("%q: %v", m.Filter, perr)
			continue
		}
		got := []string{}
		for i := range items {
			if f.Match(&items[i]) {
				got = append(got, ids[i])
			}
		}
		if !slices.Equal(got, m.IDs) {
			t.Errorf("%q: got %v, want %v", m.Filter, got, m.IDs)
		}
	}
	for _, e := range ex.Errors {
		_, perr := Parse(e.Filter, env)
		if perr == nil || perr.Pos != e.Pos || perr.Msg != e.Message {
			t.Errorf("%q: got %v, want %q at %d", e.Filter, perr, e.Message, e.Pos)
		}
	}
	for _, s := range ex.Stored {
		got, perr := ToStored(s.Display, env)
		if perr != nil || got != s.Stored {
			t.Errorf("stored %q: got %q %v, want %q", s.Display, got, perr, s.Stored)
		}
		if shown := ToDisplay(s.Stored, env); shown != s.Shown {
			t.Errorf("shown %q: got %q, want %q", s.Stored, shown, s.Shown)
		}
	}
}
