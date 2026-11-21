package api

import (
	"strconv"

	"podcasts/internal/store"
)

func unix(t interface {
	Unix() int64
	IsZero() bool
}) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func channelJSON(c store.Channel) map[string]any {
	return map[string]any{"id": c.ID, "url": c.URL, "kind": c.Kind, "title": c.Name(), "author": c.Author, "website": c.Website,
		"description": c.Description, "artwork": c.Artwork, "tags": nonNil(c.Tags), "speed": c.Speed, "skip_start": c.SkipStart,
		"skip_end": c.SkipEnd, "quality": c.Quality, "check_every": c.CheckEvery, "fetch_count": c.FetchCount,
		"keep": c.Keep, "username": c.Username, "private": c.Password != "", "last_checked": unix(c.LastChecked), "next_check": unix(c.NextCheck),
		"last_error": c.LastError, "retrieving": c.Retrieving}
}

func nonNil[T any](v []T) []T {
	if v == nil {
		return []T{}
	}
	return v
}

func episodeJSON(e store.Episode) map[string]any {
	return map[string]any{"id": e.ID, "channel": e.ChannelID, "guid": e.GUID, "media_url": e.MediaURL, "page_url": e.PageURL, "title": e.Title,
		"notes": e.Notes, "published": unix(e.Published), "date_quality": e.DateQuality, "duration": e.Duration, "media": e.Media, "mime": e.MIME,
		"size": e.Size, "season": e.Season, "number": e.Number, "type": e.Type, "artwork": e.Artwork, "chapters_url": e.ChaptersURL, "chapters": nonNil(e.Chapters),
		"transcript_url": e.TranscriptURL}
}

func playJSON(p store.Play) map[string]any {
	return map[string]any{"episode": p.EpisodeID, "position": p.Position, "played": p.Played, "starred": p.Starred,
		"position_at": p.PositionAt, "played_at": p.PlayedAt, "starred_at": p.StarredAt}
}

func playlistJSON(p store.Playlist) map[string]any {
	return map[string]any{"id": p.ID, "name": p.Name, "filter": p.Filter, "sort": p.Sort, "pos": p.Pos, "builtin": p.Builtin, "autoplay": p.Autoplay, "hide_played": p.HidePlayed, "seed": p.ShuffleSeed()}
}

func deviceJSON(d store.Device) map[string]any {
	return map[string]any{"id": d.ID, "name": d.Name, "last_sync": unix(d.LastSync)}
}

func jobJSON(j store.Job) map[string]any {
	return map[string]any{"id": j.ID, "kind": j.Kind, "channel": j.ChannelID, "label": j.Label, "reason": j.Reason, "state": j.State,
		"queued": unix(j.Queued), "started": unix(j.Started), "finished": unix(j.Finished), "added": j.Added, "updated": j.Updated,
		"pruned": j.Pruned, "not_modified": j.NotModified, "summary": j.Summary, "error": j.Error}
}

func changesJSON(c store.Changes) map[string]any {
	chs, eps, plays, pls, items, del := []any{}, []any{}, []any{}, []any{}, []any{}, []any{}
	for _, x := range c.Channels {
		chs = append(chs, channelJSON(x))
	}
	for _, x := range c.Episodes {
		eps = append(eps, episodeJSON(x))
	}
	for _, x := range c.Plays {
		plays = append(plays, playJSON(x))
	}
	for _, x := range c.Playlists {
		pls = append(pls, playlistJSON(x))
	}
	for _, x := range c.PlaylistItems {
		items = append(items, map[string]any{"playlist": x.PlaylistID, "episode": x.EpisodeID, "pos": x.Pos})
	}
	for _, x := range c.Deleted {
		id, _ := strconv.ParseInt(x.Ref, 10, 64)
		del = append(del, map[string]any{"kind": x.Kind, "id": id})
	}
	out := map[string]any{"cursor": c.Cursor, "more": c.More, "channels": chs, "episodes": eps, "plays": plays, "playlists": pls,
		"playlist_items": items, "deleted": del}
	if c.Prefs != nil {
		out["prefs"] = c.Prefs
	}
	return out
}
