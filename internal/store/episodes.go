package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"podcasts/internal/engine"
)

// Date quality: a better date replaces a worse one, never the other way round.
const (
	DateRough  = 0
	DateApprox = 1
	DateExact  = 2
)

type Chapter struct {
	Start int    `json:"start"`
	Title string `json:"title"`
}

type Episode struct {
	ID          int64
	ChannelID   int64
	GUID        string
	MediaURL    string
	PageURL     string
	Title       string
	Notes       string
	Published   time.Time
	DateQuality int
	Duration    int
	Media       string
	MIME        string
	Size        int64
	Season      int
	Number      int
	Type        string
	Artwork     string
	ChaptersURL string
	// Chapters are the ones the downloader engine found; a feed's are fetched from ChaptersURL.
	Chapters      []Chapter
	TranscriptURL string
	Created       time.Time
	Seq           int64
}

// Direct reports whether the episode plays straight from its enclosure rather than through the downloader engine.
func (e Episode) Direct() bool { return e.MediaURL != "" }

// Streamed episodes play through the server's media cache: videos, and anything without its own enclosure.
func (e Episode) Streamed() bool { return !e.Direct() || e.Media == "video" }

const episodeCols = `id, channel_id, guid, media_url, page_url, title, notes, published, date_quality, duration, media, mime, size, season, number, type,
artwork, chapters_url, chapters, transcript_url, created, seq`

func scanEpisode(row interface{ Scan(...any) error }) (Episode, error) {
	var e Episode
	var pub, created int64
	var chapters string
	err := row.Scan(&e.ID, &e.ChannelID, &e.GUID, &e.MediaURL, &e.PageURL, &e.Title, &e.Notes, &pub, &e.DateQuality, &e.Duration, &e.Media,
		&e.MIME, &e.Size, &e.Season, &e.Number, &e.Type, &e.Artwork, &e.ChaptersURL, &chapters, &e.TranscriptURL, &created, &e.Seq)
	if errors.Is(err, sql.ErrNoRows) {
		return e, ErrNotFound
	}
	if err == nil && chapters != "" {
		err = json.Unmarshal([]byte(chapters), &e.Chapters)
	}
	e.Published, e.Created = fromUnix(pub), fromUnix(created)
	return e, err
}

func (s *Store) Episode(ctx context.Context, id int64) (Episode, error) {
	return scanEpisode(s.db.QueryRowContext(ctx, `SELECT `+episodeCols+` FROM episodes WHERE id = ?`, id))
}

func (s *Store) Episodes(ctx context.Context, ids []int64) (map[int64]Episode, error) {
	out := make(map[int64]Episode, len(ids))
	for start := 0; start < len(ids); start += 500 {
		chunk := ids[start:min(start+500, len(ids))]
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		rows, err := s.db.QueryContext(ctx, `SELECT `+episodeCols+` FROM episodes WHERE id IN (`+placeholders(len(chunk))+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			e, err := scanEpisode(rows)
			if err != nil {
				rows.Close()
				return nil, err
			}
			out[e.ID] = e
		}
		rows.Close()
	}
	return out, nil
}

// EpisodeIn is one item read from a feed or video list.
type EpisodeIn struct {
	GUID        string
	MediaURL    string
	PageURL     string
	Title       string
	Notes       string
	Published   time.Time
	DateQuality int
	Duration    int
	Media       string
	// KeepMedia leaves an episode already held as audio or video as it is: a video list can't tell which, only opening one can.
	KeepMedia     bool
	MIME          string
	Size          int64
	Season        int
	Number        int
	Type          string
	Artwork       string
	ChaptersURL   string
	TranscriptURL string
}

type UpsertResult struct {
	Added   []int64
	Updated int
}

// UpsertEpisodes matches items by guid, then by media address, so nothing is duplicated.
func (s *Store) UpsertEpisodes(ctx context.Context, channelID int64, items []EpisodeIn) (UpsertResult, error) {
	var res UpsertResult
	err := s.tx(ctx, func(t *Tx) error {
		res = UpsertResult{}
		seq, err := t.Seq()
		if err != nil {
			return err
		}
		now := time.Now().Unix()
		for _, it := range items {
			var id, pub int64
			var dq int
			var title, mediaURL, notes, pageURL, art string
			var dur int
			err := t.QueryRowContext(ctx, `SELECT id, published, date_quality, title, media_url, notes, page_url, duration, artwork FROM episodes
				WHERE channel_id = ? AND ((? != '' AND guid = ?) OR (? != '' AND media_url = ?)) ORDER BY (guid = ?) DESC LIMIT 1`,
				channelID, it.GUID, it.GUID, it.MediaURL, it.MediaURL, it.GUID).Scan(&id, &pub, &dq, &title, &mediaURL, &notes, &pageURL, &dur, &art)
			if errors.Is(err, sql.ErrNoRows) {
				r, err := t.exec(`INSERT INTO episodes (channel_id, guid, media_url, page_url, title, notes, published, date_quality, duration, media, mime, size,
					season, number, type, artwork, chapters_url, transcript_url, created, seq) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
					channelID, it.GUID, it.MediaURL, it.PageURL, it.Title, it.Notes, unix(it.Published), it.DateQuality, it.Duration, it.Media, it.MIME,
					it.Size, it.Season, it.Number, it.Type, it.Artwork, it.ChaptersURL, it.TranscriptURL, now, seq)
				if err != nil {
					return err
				}
				nid, _ := r.LastInsertId()
				res.Added = append(res.Added, nid)
				continue
			}
			if err != nil {
				return err
			}
			// An approximate date is worked out from today, so a later listing's is different but no better.
			newPub, newDQ := pub, dq
			if !it.Published.IsZero() && (it.DateQuality > dq || (it.DateQuality == dq && dq == DateExact && unix(it.Published) != pub) || pub == 0) {
				newPub, newDQ = unix(it.Published), it.DateQuality
			}
			if it.Duration == 0 {
				it.Duration = dur
			}
			if it.Notes == "" {
				it.Notes = notes
			}
			if it.PageURL == "" {
				it.PageURL = pageURL
			}
			if it.Artwork == "" {
				it.Artwork = art
			}
			// A listing's picture is known to load, so it replaces one an opened video gave, which can be missing.
			changed := newPub != pub || newDQ != dq || it.Title != title || it.MediaURL != mediaURL || it.Notes != notes || it.Duration != dur || it.Artwork != art
			if !changed {
				continue
			}
			if _, err := t.exec(`UPDATE episodes SET media_url=?, page_url=?, title=?, notes=?, published=?, date_quality=?, duration=?, media=CASE WHEN ? THEN media ELSE ? END, mime=?, size=?,
				season=?, number=?, type=?, artwork=?, chapters_url=?, transcript_url=?, seq=? WHERE id=?`,
				it.MediaURL, it.PageURL, it.Title, it.Notes, newPub, newDQ, it.Duration, it.KeepMedia, it.Media, it.MIME, it.Size, it.Season, it.Number, it.Type,
				it.Artwork, it.ChaptersURL, it.TranscriptURL, seq, id); err != nil {
				return err
			}
			res.Updated++
		}
		return nil
	})
	return res, err
}

// ApproxDatedVideos are the channel's newest episodes whose date is only approximate, so were never opened.
func (s *Store) ApproxDatedVideos(ctx context.Context, channelID int64, limit int) ([]Episode, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+episodeCols+` FROM episodes WHERE channel_id = ? AND date_quality = ? AND page_url != ''
		ORDER BY published DESC, id LIMIT ?`, channelID, DateApprox, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Episode
	for rows.Next() {
		e, err := scanEpisode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// VideosWithoutExactDates are the given episodes that are videos with only a rough or approximate date, newest first.
func (s *Store) VideosWithoutExactDates(ctx context.Context, ids []int64) ([]Episode, error) {
	want := make(map[int64]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+episodeCols+` FROM episodes WHERE date_quality < ? AND page_url != '' AND media = 'video' ORDER BY published DESC, id`, DateExact)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Episode
	for rows.Next() {
		e, err := scanEpisode(rows)
		if err != nil {
			return nil, err
		}
		if want[e.ID] {
			out = append(out, e)
		}
	}
	return out, rows.Err()
}

// HasVideo reports whether any of the channel's episodes is a video, so its video quality matters.
func (s *Store) HasVideo(ctx context.Context, channelID int64) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM episodes WHERE channel_id = ? AND media = 'video')`, channelID).Scan(&n)
	return n == 1, err
}

// NewestMedia is "audio" or "video" as the channel's newest episode is, and empty for a channel with none.
func (s *Store) NewestMedia(ctx context.Context, channelID int64) (string, error) {
	var m string
	err := s.db.QueryRowContext(ctx, `SELECT media FROM episodes WHERE channel_id = ? ORDER BY published DESC, id DESC LIMIT 1`, channelID).Scan(&m)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return m, err
}

type EpisodeDetails struct {
	Published time.Time
	Duration  int
	Notes     string
	Artwork   string
	Chapters  []Chapter
	// Media is "audio" or "video"; empty keeps what the episode has.
	Media string
}

// DetailsOf is what opening a video with the downloader engine says about its episode.
func DetailsOf(v engine.Video) EpisodeDetails {
	d := EpisodeDetails{Published: v.Published, Duration: v.Duration, Notes: v.Description, Artwork: v.Thumbnail, Media: v.Media}
	for _, c := range v.Chapters {
		d.Chapters = append(d.Chapters, Chapter{Start: c.Start, Title: c.Title})
	}
	return d
}

func (s *Store) SetExactDetails(ctx context.Context, id int64, d EpisodeDetails) error {
	chapters := ""
	if len(d.Chapters) > 0 {
		b, err := json.Marshal(d.Chapters)
		if err != nil {
			return err
		}
		chapters = string(b)
	}
	return s.tx(ctx, func(t *Tx) error {
		seq, err := t.Seq()
		if err != nil {
			return err
		}
		_, err = t.exec(`UPDATE episodes SET published = CASE WHEN ? > 0 THEN ? ELSE published END, date_quality = CASE WHEN ? > 0 THEN ? ELSE date_quality END,
			duration = CASE WHEN ? > 0 THEN ? ELSE duration END, notes = CASE WHEN ? != '' THEN ? ELSE notes END,
			artwork = CASE WHEN artwork = '' AND ? != '' THEN ? ELSE artwork END, chapters = CASE WHEN ? != '' THEN ? ELSE chapters END,
			media = CASE WHEN ? != '' THEN ? ELSE media END, seq = ? WHERE id = ?`,
			unix(d.Published), unix(d.Published), unix(d.Published), DateExact, d.Duration, d.Duration, d.Notes, d.Notes, d.Artwork, d.Artwork, chapters, chapters, d.Media, d.Media, seq, id)
		return err
	})
}

// Prune deletes a channel's episodes beyond keep, oldest first, skipping starred and part-played ones.
func (s *Store) Prune(ctx context.Context, channelID int64, keep int) (int, error) {
	if keep <= 0 {
		return 0, nil
	}
	var n int
	err := s.tx(ctx, func(t *Tx) error {
		ids, err := t.episodeIDs(`channel_id = ? AND id NOT IN (SELECT id FROM episodes WHERE channel_id = ? ORDER BY published DESC, id DESC LIMIT ?)
			AND id NOT IN (SELECT episode_id FROM plays WHERE starred = 1 OR (position > 0 AND played = 0))`, channelID, channelID, keep)
		if err != nil {
			return err
		}
		n = len(ids)
		return t.deleteEpisodes(ids)
	})
	return n, err
}

// RemoveEpisodes deletes the channel's episodes with these GUIDs, whatever their play state.
func (s *Store) RemoveEpisodes(ctx context.Context, channelID int64, guids []string) error {
	if len(guids) == 0 {
		return nil
	}
	return s.tx(ctx, func(t *Tx) error {
		args := []any{channelID}
		for _, g := range guids {
			args = append(args, g)
		}
		ids, err := t.episodeIDs(`channel_id = ? AND guid IN (`+placeholders(len(guids))+`)`, args...)
		if err != nil {
			return err
		}
		return t.deleteEpisodes(ids)
	})
}

func (t *Tx) episodeIDs(where string, args ...any) ([]int64, error) {
	rows, err := t.QueryContext(t.ctx, `SELECT id FROM episodes WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (t *Tx) deleteEpisodes(ids []int64) error {
	for start := 0; start < len(ids); start += 500 {
		chunk := ids[start:min(start+500, len(ids))]
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		in := `(` + placeholders(len(chunk)) + `)`
		for _, q := range []string{`DELETE FROM episodes WHERE id IN ` + in, `DELETE FROM plays WHERE episode_id IN ` + in, `DELETE FROM playlist_items WHERE episode_id IN ` + in} {
			if _, err := t.exec(q, args...); err != nil {
				return err
			}
		}
		for _, id := range chunk {
			if err := t.tombstone("episode", id); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) EpisodeCounts(ctx context.Context, channelID int64) (total, unplayed int, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT count(*), coalesce(sum(CASE WHEN coalesce(p.played,0)=0 THEN 1 ELSE 0 END),0)
		FROM episodes e LEFT JOIN plays p ON p.episode_id = e.id WHERE e.channel_id = ?`, channelID).Scan(&total, &unplayed)
	return
}
