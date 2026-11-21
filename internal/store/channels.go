package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

type Channel struct {
	ID           int64
	URL          string
	Kind         string
	Title        string
	Author       string
	Website      string
	Description  string
	Artwork      string
	Tags         []string
	Speed        int
	SkipStart    int
	SkipEnd      int
	Quality      string
	CheckEvery   int
	FetchCount   int
	Keep         int
	Username     string
	Password     string
	ETag         string
	LastModified string
	LastChecked  time.Time
	NextCheck    time.Time
	LastError    string
	Retrieving   bool
	Created      time.Time
	Seq          int64
}

func (c Channel) Name() string {
	if c.Title != "" {
		return c.Title
	}
	return c.URL
}

func (c Channel) HasTag(tag string) bool {
	for _, t := range c.Tags {
		if t == tag {
			return true
		}
	}
	return false
}

// Effective values fill the channel's empty settings from the server defaults.
type Effective struct {
	CheckEvery time.Duration
	FetchCount int
	Keep       int
	Quality    string
}

func (c Channel) Effective(d ServerSettings) Effective {
	e := Effective{CheckEvery: time.Duration(d.CheckEvery) * time.Minute, FetchCount: d.FetchCount, Keep: d.Keep, Quality: d.Quality}
	if c.Quality != "" {
		e.Quality = c.Quality
	}
	if c.CheckEvery > 0 {
		e.CheckEvery = time.Duration(c.CheckEvery) * time.Minute
	}
	if c.FetchCount > 0 {
		e.FetchCount = c.FetchCount
	}
	if c.Keep > 0 {
		e.Keep = c.Keep
	}
	if e.FetchCount == 0 && c.Kind == "video" {
		e.FetchCount = 400
	}
	return e
}

const channelCols = `id, url, kind, title, author, website, description, artwork, tags, speed, skip_start, skip_end, quality,
check_every, fetch_count, keep, username, password, etag, last_modified, last_checked, next_check, last_error, retrieving, created, seq`

func (s *Store) scanChannel(row interface{ Scan(...any) error }) (Channel, error) {
	var c Channel
	var tags string
	var pw []byte
	var lc, nc, created int64
	err := row.Scan(&c.ID, &c.URL, &c.Kind, &c.Title, &c.Author, &c.Website, &c.Description, &c.Artwork, &tags, &c.Speed,
		&c.SkipStart, &c.SkipEnd, &c.Quality, &c.CheckEvery, &c.FetchCount, &c.Keep, &c.Username, &pw, &c.ETag,
		&c.LastModified, &lc, &nc, &c.LastError, &c.Retrieving, &created, &c.Seq)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	c.Tags, c.Password = splitTags(tags), s.open(pw)
	c.LastChecked, c.NextCheck, c.Created = fromUnix(lc), fromUnix(nc), fromUnix(created)
	return c, err
}

func (s *Store) Channel(ctx context.Context, id int64) (Channel, error) {
	return s.scanChannel(s.db.QueryRowContext(ctx, `SELECT `+channelCols+` FROM channels WHERE id = ?`, id))
}

func (s *Store) ChannelByURL(ctx context.Context, url string) (Channel, error) {
	return s.scanChannel(s.db.QueryRowContext(ctx, `SELECT `+channelCols+` FROM channels WHERE url = ?`, url))
}

func (s *Store) Channels(ctx context.Context) ([]Channel, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+channelCols+` FROM channels ORDER BY lower(CASE title WHEN '' THEN url ELSE title END)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Channel
	for rows.Next() {
		c, err := s.scanChannel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

type NewChannel struct {
	URL string
	// Title is the listener's own name for the channel, which reads don't replace; empty takes the address's own.
	Title    string
	Kind     string
	Tags     []string
	Username string
	Password string
}

// AddChannel follows an address, or returns the existing channel when it is already followed.
func (s *Store) AddChannel(ctx context.Context, n NewChannel) (Channel, bool, error) {
	if c, err := s.ChannelByURL(ctx, n.URL); err == nil {
		return c, false, nil
	}
	var id int64
	err := s.tx(ctx, func(t *Tx) error {
		seq, err := t.Seq()
		if err != nil {
			return err
		}
		kind := n.Kind
		if kind == "" {
			kind = "feed"
		}
		res, err := t.exec(`INSERT INTO channels (url, kind, title, title_own, tags, quality, username, password, created, seq) VALUES (?, ?, ?, ?, ?, '', ?, ?, ?, ?)`,
			n.URL, kind, n.Title, n.Title != "", tagText(n.Tags), n.Username, s.seal(n.Password), time.Now().Unix(), seq)
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	if err != nil {
		return Channel{}, false, err
	}
	c, err := s.Channel(ctx, id)
	return c, true, err
}

type ChannelSettings struct {
	Title      string
	Tags       []string
	Speed      int
	SkipStart  int
	SkipEnd    int
	Quality    string
	CheckEvery int
	FetchCount int
	Keep       int
	Username   string
	Password   string
	KeepPass   bool
}

func (s *Store) SetChannelSettings(ctx context.Context, id int64, v ChannelSettings) error {
	return s.tx(ctx, func(t *Tx) error {
		seq, err := t.Seq()
		if err != nil {
			return err
		}
		// A changed name is the listener's own; an emptied one, or the address an unnamed channel shows, is replaced at the next read.
		q := `UPDATE channels SET tags=?, speed=?, skip_start=?, skip_end=?, quality=?, check_every=?, fetch_count=?, keep=?, username=?, seq=?,
			title_own = CASE WHEN ?='' THEN 0 WHEN ? NOT IN (title, url) THEN 1 ELSE title_own END, title = CASE WHEN ?='' THEN title ELSE ? END`
		args := []any{tagText(v.Tags), v.Speed, v.SkipStart, v.SkipEnd, v.Quality, v.CheckEvery, v.FetchCount, v.Keep, v.Username, seq,
			v.Title, v.Title, v.Title, v.Title}
		if !v.KeepPass {
			q += `, password=?`
			args = append(args, s.seal(v.Password))
		}
		res, err := t.exec(q+` WHERE id=?`, append(args, id)...)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// SetChannelSpeed changes only the channel's playback speed, so devices pick it up on their next sync.
func (s *Store) SetChannelSpeed(ctx context.Context, id int64, speed int) error {
	return s.tx(ctx, func(t *Tx) error {
		seq, err := t.Seq()
		if err != nil {
			return err
		}
		res, err := t.exec(`UPDATE channels SET speed=?, seq=? WHERE id=?`, speed, seq, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// ChannelMeta is what a read learned about the channel itself.
type ChannelMeta struct {
	URL          string
	Kind         string
	Title        string
	Author       string
	Website      string
	Description  string
	Artwork      string
	ETag         string
	LastModified string
}

func (s *Store) SetChannelMeta(ctx context.Context, id int64, m ChannelMeta) error {
	return s.tx(ctx, func(t *Tx) error {
		seq, err := t.Seq()
		if err != nil {
			return err
		}
		_, err = t.exec(`UPDATE channels SET url=?, kind=?, title=CASE WHEN ?='' OR title_own THEN title ELSE ? END, author=?, website=?, description=?,
			artwork=CASE WHEN ?='' THEN artwork ELSE ? END, etag=?, last_modified=?, seq=? WHERE id=?`,
			m.URL, m.Kind, m.Title, m.Title, m.Author, m.Website, m.Description, m.Artwork, m.Artwork, m.ETag, m.LastModified, seq, id)
		return err
	})
}

// SetChecked records the end of a read and when the next one is due.
func (s *Store) SetChecked(ctx context.Context, id int64, at, next time.Time, errText string) error {
	return s.tx(ctx, func(t *Tx) error {
		seq, err := t.Seq()
		if err != nil {
			return err
		}
		_, err = t.exec(`UPDATE channels SET last_checked=?, next_check=?, last_error=?, retrieving=0, seq=? WHERE id=?`, at.Unix(), next.Unix(), errText, seq, id)
		return err
	})
}

func (s *Store) SetNextCheck(ctx context.Context, id int64, next time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE channels SET next_check=? WHERE id=?`, next.Unix(), id)
	return err
}

func (s *Store) DueChannels(ctx context.Context, now time.Time) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM channels WHERE next_check <= ? ORDER BY next_check`, now.Unix())
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

// DeleteChannel removes a channel with its episodes, plays and manual-order entries; playlists stay.
func (s *Store) DeleteChannel(ctx context.Context, id int64) error {
	return s.tx(ctx, func(t *Tx) error {
		ids, err := t.episodeIDs(`channel_id = ?`, id)
		if err != nil {
			return err
		}
		if err := t.deleteEpisodes(ids); err != nil {
			return err
		}
		if _, err := t.exec(`DELETE FROM channels WHERE id=?`, id); err != nil {
			return err
		}
		return t.tombstone("channel", id)
	})
}

// MergeChannel folds channel from into channel into: plays move to the matching episodes, then from is removed.
func (s *Store) MergeChannel(ctx context.Context, from, into int64) error {
	return s.tx(ctx, func(t *Tx) error {
		seq, err := t.Seq()
		if err != nil {
			return err
		}
		_, err = t.exec(`UPDATE plays SET episode_id = (
				SELECT n.id FROM episodes o JOIN episodes n ON n.channel_id = ? AND ((o.guid != '' AND n.guid = o.guid) OR (o.media_url != '' AND n.media_url = o.media_url))
				WHERE o.id = plays.episode_id LIMIT 1), seq = ?
			WHERE episode_id IN (SELECT id FROM episodes WHERE channel_id = ?)
			AND EXISTS (SELECT 1 FROM episodes o JOIN episodes n ON n.channel_id = ? AND ((o.guid != '' AND n.guid = o.guid) OR (o.media_url != '' AND n.media_url = o.media_url))
				WHERE o.id = plays.episode_id)
			AND NOT EXISTS (SELECT 1 FROM plays p2 JOIN episodes o ON o.id = plays.episode_id JOIN episodes n ON n.id = p2.episode_id AND n.channel_id = ?
				AND ((o.guid != '' AND n.guid = o.guid) OR (o.media_url != '' AND n.media_url = o.media_url)))`, into, seq, from, into, into)
		if err != nil {
			return err
		}
		ids, err := t.episodeIDs(`channel_id = ?`, from)
		if err != nil {
			return err
		}
		if err := t.deleteEpisodes(ids); err != nil {
			return err
		}
		if _, err := t.exec(`DELETE FROM channels WHERE id=?`, from); err != nil {
			return err
		}
		return t.tombstone("channel", from)
	})
}

type TagInfo struct {
	Name     string
	Channels []Channel
}

func (s *Store) Tags(ctx context.Context) ([]TagInfo, error) {
	chs, err := s.Channels(ctx)
	if err != nil {
		return nil, err
	}
	idx := map[string]int{}
	var out []TagInfo
	for _, c := range chs {
		for _, t := range c.Tags {
			i, ok := idx[t]
			if !ok {
				i = len(out)
				idx[t] = i
				out = append(out, TagInfo{Name: t})
			}
			out[i].Channels = append(out[i].Channels, c)
		}
	}
	sortTags(out)
	return out, nil
}

func sortTags(t []TagInfo) {
	for i := 1; i < len(t); i++ {
		for j := i; j > 0 && t[j].Name < t[j-1].Name; j-- {
			t[j], t[j-1] = t[j-1], t[j]
		}
	}
}

// RenameTag rewrites the tag on every channel; the caller rewrites playlist filters in the same call through rewrite.
func (s *Store) RenameTag(ctx context.Context, from, to string, rewrite func(filter string) string) error {
	return s.tx(ctx, func(t *Tx) error {
		seq, err := t.Seq()
		if err != nil {
			return err
		}
		rows, err := t.QueryContext(ctx, `SELECT id, tags FROM channels WHERE tags LIKE ?`, "% "+from+" %")
		if err != nil {
			return err
		}
		type upd struct {
			id   int64
			tags string
		}
		var ups []upd
		for rows.Next() {
			var u upd
			if err := rows.Scan(&u.id, &u.tags); err != nil {
				rows.Close()
				return err
			}
			var out []string
			seen := map[string]bool{}
			for _, tg := range splitTags(u.tags) {
				if tg == from {
					tg = to
				}
				if !seen[tg] {
					seen[tg] = true
					out = append(out, tg)
				}
			}
			u.tags = tagText(out)
			ups = append(ups, u)
		}
		rows.Close()
		for _, u := range ups {
			if _, err := t.exec(`UPDATE channels SET tags=?, seq=? WHERE id=?`, u.tags, seq, u.id); err != nil {
				return err
			}
		}
		prow, err := t.QueryContext(ctx, `SELECT id, filter FROM playlists`)
		if err != nil {
			return err
		}
		type pf struct {
			id     int64
			filter string
		}
		var pfs []pf
		for prow.Next() {
			var p pf
			if err := prow.Scan(&p.id, &p.filter); err != nil {
				prow.Close()
				return err
			}
			if nf := rewrite(p.filter); nf != p.filter {
				p.filter = nf
				pfs = append(pfs, p)
			}
		}
		prow.Close()
		for _, p := range pfs {
			if _, err := t.exec(`UPDATE playlists SET filter=?, seq=? WHERE id=?`, p.filter, seq, p.id); err != nil {
				return err
			}
		}
		return nil
	})
}

// TagFromName turns a free-text name such as an OPML folder into one tag.
func TagFromName(name string) string {
	tag := strings.Join(strings.FieldsFunc(strings.ToLower(name), func(r rune) bool {
		return !(r == '-' || r == '_' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r > 127)
	}), "-")
	if r := []rune(tag); len(r) > 40 {
		tag = strings.TrimRight(string(r[:40]), "-")
	}
	return tag
}

func NormalizeTags(v string) []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' }) {
		f = strings.ToLower(strings.TrimLeft(f, "#"))
		if f != "" && !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	return out
}
