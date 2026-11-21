package store

import (
	"context"
	"time"
)

// LibEpisode is the part of an episode that filters, lists and the player need.
type LibEpisode struct {
	ID        int64
	ChannelID int64
	Title     string
	Published time.Time
	Duration  int
	Media     string
	Season    int
	Number    int
	Type      string
	MediaURL  string
	Artwork   string
}

// Library is a read-only snapshot of everything, rebuilt only when something changed.
type Library struct {
	Seq       int64
	Channels  map[int64]Channel
	Episodes  []LibEpisode
	ByID      map[int64]int
	Plays     map[int64]Play
	Playlists []Playlist
}

func (l *Library) Episode(id int64) (LibEpisode, bool) {
	i, ok := l.ByID[id]
	if !ok {
		return LibEpisode{}, false
	}
	return l.Episodes[i], true
}

func (s *Store) Library(ctx context.Context) (*Library, error) {
	seq, err := s.CurrentSeq(ctx)
	if err != nil {
		return nil, err
	}
	s.libMu.Lock()
	defer s.libMu.Unlock()
	if s.lib != nil && s.libSeq == seq {
		return s.lib, nil
	}
	lib := &Library{Seq: seq, Channels: map[int64]Channel{}, ByID: map[int64]int{}, Plays: map[int64]Play{}}
	chs, err := s.Channels(ctx)
	if err != nil {
		return nil, err
	}
	for _, c := range chs {
		lib.Channels[c.ID] = c
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, channel_id, title, published, duration, media, season, number, type, media_url, artwork FROM episodes ORDER BY published DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var e LibEpisode
		var pub int64
		if err := rows.Scan(&e.ID, &e.ChannelID, &e.Title, &pub, &e.Duration, &e.Media, &e.Season, &e.Number, &e.Type, &e.MediaURL, &e.Artwork); err != nil {
			rows.Close()
			return nil, err
		}
		e.Published = fromUnix(pub)
		lib.ByID[e.ID] = len(lib.Episodes)
		lib.Episodes = append(lib.Episodes, e)
	}
	rows.Close()
	prow, err := s.db.QueryContext(ctx, `SELECT `+playCols+` FROM plays`)
	if err != nil {
		return nil, err
	}
	for prow.Next() {
		p, err := scanPlay(prow)
		if err != nil {
			prow.Close()
			return nil, err
		}
		lib.Plays[p.EpisodeID] = p
	}
	prow.Close()
	if lib.Playlists, err = s.Playlists(ctx); err != nil {
		return nil, err
	}
	s.lib, s.libSeq = lib, seq
	return lib, nil
}

// Changes is everything a device needs to catch up from its cursor.
type Changes struct {
	Cursor        int64
	More          bool
	Channels      []Channel
	Episodes      []Episode
	Plays         []Play
	Playlists     []Playlist
	PlaylistItems []PlaylistItem
	Deleted       []Deletion
	Prefs         *Prefs
}

type PlaylistItem struct {
	PlaylistID int64
	EpisodeID  int64
	Pos        float64
}

type Deletion struct {
	Kind string
	Ref  string
}

// Changes pages by change number and never splits one change across pages.
func (s *Store) Changes(ctx context.Context, cursor int64, limit int) (Changes, error) {
	cur, err := s.CurrentSeq(ctx)
	if err != nil {
		return Changes{}, err
	}
	upper := cur
	var edge int64
	err = s.db.QueryRowContext(ctx, `SELECT seq FROM episodes WHERE seq > ? ORDER BY seq LIMIT 1 OFFSET ?`, cursor, limit-1).Scan(&edge)
	if err == nil && edge < cur {
		upper = edge
	}
	ch := Changes{Cursor: upper, More: upper < cur}
	if cursor >= upper {
		return ch, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+channelCols+` FROM channels WHERE seq > ? AND seq <= ?`, cursor, upper)
	if err != nil {
		return ch, err
	}
	for rows.Next() {
		c, err := s.scanChannel(rows)
		if err != nil {
			rows.Close()
			return ch, err
		}
		ch.Channels = append(ch.Channels, c)
	}
	rows.Close()
	rows, err = s.db.QueryContext(ctx, `SELECT `+episodeCols+` FROM episodes WHERE seq > ? AND seq <= ? ORDER BY seq`, cursor, upper)
	if err != nil {
		return ch, err
	}
	for rows.Next() {
		e, err := scanEpisode(rows)
		if err != nil {
			rows.Close()
			return ch, err
		}
		ch.Episodes = append(ch.Episodes, e)
	}
	rows.Close()
	rows, err = s.db.QueryContext(ctx, `SELECT `+playCols+` FROM plays WHERE seq > ? AND seq <= ?`, cursor, upper)
	if err != nil {
		return ch, err
	}
	for rows.Next() {
		p, err := scanPlay(rows)
		if err != nil {
			rows.Close()
			return ch, err
		}
		ch.Plays = append(ch.Plays, p)
	}
	rows.Close()
	rows, err = s.db.QueryContext(ctx, `SELECT `+playlistCols+` FROM playlists WHERE seq > ? AND seq <= ?`, cursor, upper)
	if err != nil {
		return ch, err
	}
	for rows.Next() {
		p, err := scanPlaylist(rows)
		if err != nil {
			rows.Close()
			return ch, err
		}
		ch.Playlists = append(ch.Playlists, p)
	}
	rows.Close()
	rows, err = s.db.QueryContext(ctx, `SELECT playlist_id, episode_id, pos FROM playlist_items WHERE seq > ? AND seq <= ?`, cursor, upper)
	if err != nil {
		return ch, err
	}
	for rows.Next() {
		var it PlaylistItem
		if err := rows.Scan(&it.PlaylistID, &it.EpisodeID, &it.Pos); err != nil {
			rows.Close()
			return ch, err
		}
		ch.PlaylistItems = append(ch.PlaylistItems, it)
	}
	rows.Close()
	rows, err = s.db.QueryContext(ctx, `SELECT kind, ref FROM tombstones WHERE seq > ? AND seq <= ?`, cursor, upper)
	if err != nil {
		return ch, err
	}
	for rows.Next() {
		var d Deletion
		if err := rows.Scan(&d.Kind, &d.Ref); err != nil {
			rows.Close()
			return ch, err
		}
		ch.Deleted = append(ch.Deleted, d)
	}
	rows.Close()
	if ps := s.prefsSeq(ctx); ps > cursor && ps <= upper || cursor == 0 {
		p, err := s.Prefs(ctx)
		if err != nil {
			return ch, err
		}
		ch.Prefs = &p
	}
	return ch, nil
}
