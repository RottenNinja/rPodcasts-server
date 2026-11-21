package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"

	"podcasts/internal/intake"
	"podcasts/internal/jobs"
	"podcasts/internal/library"
	"podcasts/internal/store"
)

//go:embed demo.json
var demoLibrary []byte

// seedDemo makes the library match demo.json: it follows its channels, creates its playlists and removes anything else.
func seedDemo(ctx context.Context, st *store.Store, jm *jobs.Manager, lib *library.Loader) error {
	var seed struct {
		Channels  []struct{ URL, Tags string }
		Playlists []struct{ Name, Filter, Sort string }
	}
	if err := json.Unmarshal(demoLibrary, &seed); err != nil {
		return fmt.Errorf("demo.json: %w", err)
	}
	keep := map[int64]bool{}
	for _, c := range seed.Channels {
		f, err := intake.ParseFollow(url.Values{"url": {c.URL}, "tags": {c.Tags}})
		if err != nil {
			return fmt.Errorf("demo.json %s: %w", c.URL, err)
		}
		ch, created, err := jm.FollowUnchecked(ctx, store.NewChannel{URL: f.URL, Tags: f.Tags})
		if err != nil {
			return err
		}
		keep[ch.ID] = true
		if !created && !slices.Equal(ch.Tags, f.Tags) {
			err := st.SetChannelSettings(ctx, ch.ID, store.ChannelSettings{Tags: f.Tags, Speed: ch.Speed, SkipStart: ch.SkipStart, SkipEnd: ch.SkipEnd,
				Quality: ch.Quality, CheckEvery: ch.CheckEvery, FetchCount: ch.FetchCount, Keep: ch.Keep, Username: ch.Username, KeepPass: true})
			if err != nil {
				return err
			}
		}
	}
	chs, err := st.Channels(ctx)
	if err != nil {
		return err
	}
	for _, ch := range chs {
		if !keep[ch.ID] {
			if err := st.DeleteChannel(ctx, ch.ID); err != nil {
				return err
			}
		}
	}
	have, err := st.Playlists(ctx)
	if err != nil {
		return err
	}
	v, err := lib.Load(ctx)
	if err != nil {
		return err
	}
	var order []store.PlaylistIn
	want := map[store.PlaylistIn]bool{}
	for _, in := range seed.Playlists {
		p, err := intake.ParsePlaylist(url.Values{"name": {in.Name}, "filter": {in.Filter}, "sort": {in.Sort}})
		if err != nil {
			return fmt.Errorf("demo.json playlist %q: %w", in.Name, err)
		}
		if _, ferr := v.Parse(p.Filter); ferr != nil {
			return fmt.Errorf("demo.json playlist %q: %s", in.Name, ferr.Msg)
		}
		in := store.PlaylistIn{Name: p.Name, Filter: p.Filter, Sort: p.Sort}
		order = append(order, in)
		want[in] = true
	}
	for _, p := range have {
		in := store.PlaylistIn{Name: p.Name, Filter: p.Filter, Sort: p.Sort}
		switch {
		case p.Builtin != "":
		case want[in]:
			delete(want, in)
		default:
			if err := st.DeletePlaylist(ctx, p.ID); err != nil {
				return err
			}
		}
	}
	for _, p := range order {
		if !want[p] {
			continue
		}
		if _, err := st.CreatePlaylist(ctx, p); err != nil {
			return err
		}
	}
	return nil
}
