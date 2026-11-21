package store

import (
	"context"
	"database/sql"
	"errors"
)

type Playlist struct {
	ID      int64
	Name    string
	Filter  string
	Sort    string
	Pos     float64
	Builtin string
	// Autoplay carries on to the next episode when one ends.
	Autoplay bool
	// HidePlayed lists only unplayed episodes; played ones are still members.
	HidePlayed bool
	// Seed is the seq of the last reshuffle; 0 until the first one.
	Seed int64
	Seq  int64
}

// ShuffleSeed fixes the shuffled order: the playlist's own until it is reshuffled.
func (p Playlist) ShuffleSeed() int64 {
	if p.Seed == 0 {
		return p.ID
	}
	return p.Seed
}

const playlistCols = `id, name, filter, sort, pos, builtin, autoplay, hide_played, seed, seq`

func scanPlaylist(row interface{ Scan(...any) error }) (Playlist, error) {
	var p Playlist
	err := row.Scan(&p.ID, &p.Name, &p.Filter, &p.Sort, &p.Pos, &p.Builtin, &p.Autoplay, &p.HidePlayed, &p.Seed, &p.Seq)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrNotFound
	}
	return p, err
}

func (s *Store) Playlist(ctx context.Context, id int64) (Playlist, error) {
	return scanPlaylist(s.db.QueryRowContext(ctx, `SELECT `+playlistCols+` FROM playlists WHERE id = ?`, id))
}

func (s *Store) Playlists(ctx context.Context) ([]Playlist, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+playlistCols+` FROM playlists ORDER BY pos, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Playlist
	for rows.Next() {
		p, err := scanPlaylist(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

type PlaylistIn struct {
	Name       string
	Filter     string
	Sort       string
	Autoplay   bool
	HidePlayed bool
}

func (s *Store) CreatePlaylist(ctx context.Context, in PlaylistIn) (int64, error) {
	var id int64
	err := s.tx(ctx, func(t *Tx) error {
		var err error
		id, err = t.createPlaylist(in, "")
		return err
	})
	return id, err
}

func (t *Tx) createPlaylist(in PlaylistIn, builtin string) (int64, error) {
	seq, err := t.Seq()
	if err != nil {
		return 0, err
	}
	var hi sql.NullFloat64
	if err := t.QueryRowContext(t.ctx, `SELECT max(pos) FROM playlists`).Scan(&hi); err != nil {
		return 0, err
	}
	r, err := t.exec(`INSERT INTO playlists (name, filter, sort, pos, builtin, autoplay, hide_played, seq) VALUES (?,?,?,?,?,?,?,?)`,
		in.Name, in.Filter, in.Sort, hi.Float64+1, builtin, b2i(in.Autoplay), b2i(in.HidePlayed), seq)
	if err != nil {
		return 0, err
	}
	return r.LastInsertId()
}

func (s *Store) UpdatePlaylist(ctx context.Context, id int64, in PlaylistIn) error {
	return s.tx(ctx, func(t *Tx) error {
		seq, err := t.Seq()
		if err != nil {
			return err
		}
		r, err := t.exec(`UPDATE playlists SET name=?, filter=?, sort=?, autoplay=?, hide_played=?, seq=? WHERE id=?`,
			in.Name, in.Filter, in.Sort, b2i(in.Autoplay), b2i(in.HidePlayed), seq, id)
		if err != nil {
			return err
		}
		if n, _ := r.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// ReshufflePlaylist gives a playlist a new shuffled order.
func (s *Store) ReshufflePlaylist(ctx context.Context, id int64) error {
	return s.tx(ctx, func(t *Tx) error {
		seq, err := t.Seq()
		if err != nil {
			return err
		}
		r, err := t.exec(`UPDATE playlists SET seed=?, seq=? WHERE id=?`, seq, seq, id)
		if err != nil {
			return err
		}
		if n, _ := r.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// DeletePlaylist remembers a deleted built-in so it is never recreated.
func (s *Store) DeletePlaylist(ctx context.Context, id int64) error {
	return s.tx(ctx, func(t *Tx) error {
		if _, err := t.exec(`DELETE FROM playlists WHERE id=?`, id); err != nil {
			return err
		}
		if _, err := t.exec(`DELETE FROM playlist_items WHERE playlist_id=?`, id); err != nil {
			return err
		}
		return t.tombstone("playlist", id)
	})
}

func (s *Store) MovePlaylist(ctx context.Context, id, target int64, after bool) error {
	return s.tx(ctx, func(t *Tx) error {
		rows, err := t.QueryContext(ctx, `SELECT id, pos FROM playlists ORDER BY pos, id`)
		if err != nil {
			return err
		}
		var list []posID
		for rows.Next() {
			var p posID
			if err := rows.Scan(&p.id, &p.pos); err != nil {
				rows.Close()
				return err
			}
			list = append(list, p)
		}
		rows.Close()
		pos, ok := between(list, id, target, after)
		if !ok {
			return ErrNotFound
		}
		seq, err := t.Seq()
		if err != nil {
			return err
		}
		_, err = t.exec(`UPDATE playlists SET pos=?, seq=? WHERE id=?`, pos, seq, id)
		return err
	})
}

// ManualOrder maps episode id to its place in a playlist's own order.
func (s *Store) ManualOrder(ctx context.Context, playlistID int64) (map[int64]float64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT episode_id, pos FROM playlist_items WHERE playlist_id = ?`, playlistID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]float64{}
	for rows.Next() {
		var id int64
		var pos float64
		if err := rows.Scan(&id, &pos); err != nil {
			return nil, err
		}
		out[id] = pos
	}
	return out, rows.Err()
}

// PlaceInPlaylist stores the full current order (the caller's list of episode ids) with id moved next to target.
func (s *Store) PlaceInPlaylist(ctx context.Context, playlistID int64, order []int64, id, target int64, after bool) error {
	return s.tx(ctx, func(t *Tx) error {
		list := make([]posID, len(order))
		for i, e := range order {
			list[i] = posID{id: e, pos: float64(i + 1)}
		}
		pos, ok := between(list, id, target, after)
		if !ok {
			return ErrNotFound
		}
		seq, err := t.Seq()
		if err != nil {
			return err
		}
		for _, p := range list {
			if p.id == id {
				p.pos = pos
			}
			if _, err := t.exec(`INSERT INTO playlist_items (playlist_id, episode_id, pos, seq) VALUES (?,?,?,?)
				ON CONFLICT (playlist_id, episode_id) DO UPDATE SET pos=excluded.pos, seq=excluded.seq`, playlistID, p.id, p.pos, seq); err != nil {
				return err
			}
		}
		return nil
	})
}

type BuiltinPlaylist struct {
	Key    string
	Name   string
	Filter string
}

var Builtins = []BuiltinPlaylist{
	{"all", "All", ""},
	{"new", "New", "$unplayed AND !$inprogress AND $published >= -7d"},
	{"inprogress", "In progress", "$inprogress"},
	{"audio", "All audio", "$media = audio"},
	{"video", "All video", "$media = video"},
	{"starred", "Starred", "$starred"},
}

// EnsureBuiltins creates the built-in playlists once, on the first follow; deleted ones stay deleted.
func (s *Store) EnsureBuiltins(ctx context.Context) error {
	var done bool
	if err := s.getJSON(ctx, "builtins_created", &done); err != nil || done {
		return err
	}
	return s.tx(ctx, func(t *Tx) error {
		for _, b := range Builtins {
			if _, err := t.createPlaylist(PlaylistIn{Name: b.Name, Filter: b.Filter, Sort: "newest", HidePlayed: true}, b.Key); err != nil {
				return err
			}
		}
		return t.putJSON("builtins_created", true)
	})
}
