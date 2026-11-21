package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

type Play struct {
	EpisodeID  int64
	Position   int
	Played     bool
	Starred    bool
	PositionAt int64
	PlayedAt   int64
	StarredAt  int64
	Seq        int64
}

func (p Play) InProgress() bool { return p.Position > 0 && !p.Played }

const playCols = `episode_id, position, played, starred, position_at, played_at, starred_at, seq`

func scanPlay(row interface{ Scan(...any) error }) (Play, error) {
	var p Play
	err := row.Scan(&p.EpisodeID, &p.Position, &p.Played, &p.Starred, &p.PositionAt, &p.PlayedAt, &p.StarredAt, &p.Seq)
	return p, err
}

func (s *Store) Play(ctx context.Context, episodeID int64) (Play, error) {
	p, err := scanPlay(s.db.QueryRowContext(ctx, `SELECT `+playCols+` FROM plays WHERE episode_id = ?`, episodeID))
	if errors.Is(err, sql.ErrNoRows) {
		return Play{EpisodeID: episodeID}, nil
	}
	return p, err
}

func (t *Tx) play(id int64) (Play, error) {
	p, err := scanPlay(t.QueryRowContext(t.ctx, `SELECT `+playCols+` FROM plays WHERE episode_id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Play{EpisodeID: id}, nil
	}
	return p, err
}

func (t *Tx) savePlay(p Play) error {
	seq, err := t.Seq()
	if err != nil {
		return err
	}
	_, err = t.exec(`INSERT INTO plays (`+playCols+`) VALUES (?,?,?,?,?,?,?,?)
		ON CONFLICT (episode_id) DO UPDATE SET position=excluded.position, played=excluded.played, starred=excluded.starred,
		position_at=excluded.position_at, played_at=excluded.played_at, starred_at=excluded.starred_at, seq=excluded.seq`,
		p.EpisodeID, p.Position, b2i(p.Played), b2i(p.Starred), p.PositionAt, p.PlayedAt, p.StarredAt, seq)
	return err
}

// PlayChange is one field of one play, stamped with when it was made (unix ms).
type PlayChange struct {
	EpisodeID int64
	Field     string
	Position  int
	Value     bool
	At        int64
}

const (
	FieldPosition = "position"
	FieldPlayed   = "played"
	FieldStarred  = "starred"
)

// apply keeps the latest change for each field; a played or unplayed mark wipes the position.
func (p *Play) apply(c PlayChange) bool {
	switch c.Field {
	case FieldPosition:
		if c.At <= p.PositionAt || (p.Played && c.At <= p.PlayedAt) {
			return false
		}
		p.Position, p.PositionAt = c.Position, c.At
		if p.Played && c.Position > 0 {
			p.Played, p.PlayedAt = false, c.At
		}
	case FieldPlayed:
		if c.At <= p.PlayedAt {
			return false
		}
		p.Played, p.PlayedAt = c.Value, c.At
		p.Position, p.PositionAt = 0, c.At
	case FieldStarred:
		if c.At <= p.StarredAt {
			return false
		}
		p.Starred, p.StarredAt = c.Value, c.At
	default:
		return false
	}
	return true
}

func (s *Store) ApplyPlayChanges(ctx context.Context, changes []PlayChange) error {
	return s.tx(ctx, func(t *Tx) error {
		for _, c := range changes {
			var exists int
			if err := t.QueryRowContext(ctx, `SELECT count(*) FROM episodes WHERE id = ?`, c.EpisodeID).Scan(&exists); err != nil {
				return err
			}
			if exists == 0 {
				continue
			}
			p, err := t.play(c.EpisodeID)
			if err != nil {
				return err
			}
			if p.apply(c) {
				if err := t.savePlay(p); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func Now() int64 { return time.Now().UnixMilli() }

// MarkMany sets one yes/no field on several episodes at once.
func (s *Store) MarkMany(ctx context.Context, ids []int64, field string, value bool) error {
	at := Now()
	changes := make([]PlayChange, len(ids))
	for i, id := range ids {
		changes[i] = PlayChange{EpisodeID: id, Field: field, Value: value, At: at}
	}
	return s.ApplyPlayChanges(ctx, changes)
}

func (s *Store) SetPosition(ctx context.Context, id int64, pos int) error {
	return s.ApplyPlayChanges(ctx, []PlayChange{{EpisodeID: id, Field: FieldPosition, Position: pos, At: Now()}})
}

type posID struct {
	id  int64
	pos float64
}

// between returns a position for id next to target in an ordered list that excludes id.
func between(list []posID, id, target int64, after bool) (float64, bool) {
	var rest []posID
	for _, p := range list {
		if p.id != id {
			rest = append(rest, p)
		}
	}
	for i, p := range rest {
		if p.id != target {
			continue
		}
		if after {
			if i+1 < len(rest) {
				return (p.pos + rest[i+1].pos) / 2, true
			}
			return p.pos + 1, true
		}
		if i > 0 {
			return (rest[i-1].pos + p.pos) / 2, true
		}
		return p.pos - 1, true
	}
	return 0, false
}

// MarkOlderPlayed marks every episode of the channel published before the given one as played.
func (s *Store) MarkOlderPlayed(ctx context.Context, e Episode) (int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM episodes WHERE channel_id = ? AND published < ?`, e.ChannelID, unix(e.Published))
	if err != nil {
		return 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	return len(ids), s.MarkMany(ctx, ids, FieldPlayed, true)
}
