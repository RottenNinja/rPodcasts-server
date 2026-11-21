// Package store keeps the library, playlists, devices and run history in one SQLite database.
package store

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"

	_ "github.com/mattn/go-sqlite3"
)

var ErrNotFound = errors.New("not found")

type Store struct {
	db   *sql.DB
	aead cipher.AEAD

	libMu  sync.Mutex
	libSeq int64
	lib    *Library
}

func Open(path string, masterKey []byte) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite3", "file:"+path+"?_journal_mode=WAL&_busy_timeout=10000&_txlock=immediate&_synchronous=NORMAL")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("schema: %w", err)
	}
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		db.Close()
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{db: db, aead: aead}
	if err := s.foldersToTags(); err != nil {
		db.Close()
		return nil, fmt.Errorf("folders to tags: %w", err)
	}
	if err := s.dropDeviceSettings(); err != nil {
		db.Close()
		return nil, fmt.Errorf("device settings: %w", err)
	}
	if err := s.addColumn("channels", "title_own", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return nil, err
	}
	if err := s.addColumn("episodes", "chapters", "TEXT NOT NULL DEFAULT ''"); err != nil {
		db.Close()
		return nil, fmt.Errorf("episode chapters: %w", err)
	}
	if err := s.addColumn("playlists", "autoplay", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		db.Close()
		return nil, fmt.Errorf("playlist autoplay: %w", err)
	}
	if err := s.addColumn("playlists", "hide_played", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		db.Close()
		return nil, fmt.Errorf("playlist hide played: %w", err)
	}
	if err := s.addColumn("playlists", "seed", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		db.Close()
		return nil, fmt.Errorf("playlist seed: %w", err)
	}
	if err := s.dropQueueAndArchive(); err != nil {
		db.Close()
		return nil, fmt.Errorf("queue and archive: %w", err)
	}
	if err := s.defaultQuality(); err != nil {
		db.Close()
		return nil, fmt.Errorf("default quality: %w", err)
	}
	if err := s.flagsToKeys(); err != nil {
		db.Close()
		return nil, fmt.Errorf("filter flags: %w", err)
	}
	return s, nil
}

// foldersToTags moves each channel's old folder into its tags and drops the column.
func (s *Store) foldersToTags() error {
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM pragma_table_info('channels') WHERE name = 'folder'`).Scan(&n); err != nil || n == 0 {
		return err
	}
	ctx := context.Background()
	return s.tx(ctx, func(t *Tx) error {
		rows, err := t.QueryContext(ctx, `SELECT id, folder, tags FROM channels WHERE folder != ''`)
		if err != nil {
			return err
		}
		type moved struct {
			id   int64
			tags string
		}
		var all []moved
		for rows.Next() {
			var m moved
			var folder, tags string
			if err := rows.Scan(&m.id, &folder, &tags); err != nil {
				rows.Close()
				return err
			}
			m.tags = tagText(NormalizeTags(tags + " " + TagFromName(folder)))
			all = append(all, m)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(all) > 0 {
			seq, err := t.Seq()
			if err != nil {
				return err
			}
			for _, m := range all {
				if _, err := t.exec(`UPDATE channels SET tags = ?, seq = ? WHERE id = ?`, m.tags, seq, m.id); err != nil {
					return err
				}
			}
		}
		_, err = t.exec(`ALTER TABLE channels DROP COLUMN folder`)
		return err
	})
}

// dropDeviceSettings removes the columns from when the server kept each device's download settings.
func (s *Store) dropDeviceSettings() error {
	for _, col := range []string{"settings", "seq"} {
		var n int
		if err := s.db.QueryRow(`SELECT count(*) FROM pragma_table_info('devices') WHERE name = ?`, col).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			if _, err := s.db.Exec(`ALTER TABLE devices DROP COLUMN ` + col); err != nil {
				return err
			}
		}
	}
	return nil
}

var oldFlags = regexp.MustCompile(`(?i)#(archived|queued)([^\pL\pN_.\-]|$)`)

// dropQueueAndArchive marks archived episodes played and queued ones starred, then drops the queue and the archive.
func (s *Store) dropQueueAndArchive() error {
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM pragma_table_info('plays') WHERE name = 'archived'`).Scan(&n); err != nil || n == 0 {
		return err
	}
	ctx := context.Background()
	return s.tx(ctx, func(t *Tx) error {
		seq, err := t.Seq()
		if err != nil {
			return err
		}
		at := Now()
		if _, err := t.exec(`UPDATE plays SET played = 1, position = 0, played_at = ?, position_at = ?, seq = ? WHERE archived = 1 AND played = 0`, at, at, seq); err != nil {
			return err
		}
		if _, err := t.exec(`UPDATE plays SET starred = 1, starred_at = ?, seq = ? WHERE queued = 1 AND played = 0 AND starred = 0`, at, seq); err != nil {
			return err
		}
		rows, err := t.QueryContext(ctx, `SELECT id, filter FROM playlists WHERE filter LIKE '%#archived%' OR filter LIKE '%#queued%'`)
		if err != nil {
			return err
		}
		filters := map[int64]string{}
		for rows.Next() {
			var id int64
			var f string
			if err := rows.Scan(&id, &f); err != nil {
				rows.Close()
				return err
			}
			filters[id] = oldFlags.ReplaceAllStringFunc(f, func(m string) string {
				if len(m) >= 9 && strings.EqualFold(m[:9], "#archived") {
					return "#played" + m[9:]
				}
				return "#starred" + m[7:]
			})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for id, f := range filters {
			if _, err := t.exec(`UPDATE playlists SET filter = ?, seq = ? WHERE id = ?`, f, seq, id); err != nil {
				return err
			}
		}
		for _, col := range []string{"archived", "queued", "queue_pos", "archived_at", "queued_at"} {
			if _, err := t.exec(`ALTER TABLE plays DROP COLUMN ` + col); err != nil {
				return err
			}
		}
		_, err = t.exec(`ALTER TABLE channels DROP COLUMN auto_queue`)
		return err
	})
}

// addColumn adds a column to a database made before the schema had it.
func (s *Store) addColumn(table, col, decl string) error {
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM pragma_table_info(?) WHERE name = ?`, table, col).Scan(&n); err != nil || n > 0 {
		return err
	}
	_, err := s.db.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + col + ` ` + decl)
	return err
}

// defaultQuality clears every channel's 720p once, the old fixed default, so those channels follow the server's quality.
func (s *Store) defaultQuality() error {
	ctx := context.Background()
	var done bool
	if err := s.getJSON(ctx, "quality_default", &done); err != nil || done {
		return err
	}
	return s.tx(ctx, func(t *Tx) error {
		var n int
		if err := t.QueryRowContext(ctx, `SELECT count(*) FROM channels WHERE quality = '720'`).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			seq, err := t.Seq()
			if err != nil {
				return err
			}
			if _, err := t.exec(`UPDATE channels SET quality = '', seq = ? WHERE quality = '720'`, seq); err != nil {
				return err
			}
		}
		return t.putJSON("quality_default", true)
	})
}

// flagsToKeys rewrites every saved filter once, from when flags and keys were written #name; # is now tags only.
func (s *Store) flagsToKeys() error {
	ctx := context.Background()
	var done bool
	if err := s.getJSON(ctx, "flags_are_keys", &done); err != nil || done {
		return err
	}
	return s.tx(ctx, func(t *Tx) error {
		rows, err := t.QueryContext(ctx, `SELECT id, filter FROM playlists WHERE filter LIKE '%#%'`)
		if err != nil {
			return err
		}
		filters := map[int64]string{}
		for rows.Next() {
			var id int64
			var f string
			if err := rows.Scan(&id, &f); err != nil {
				rows.Close()
				return err
			}
			if to := hashToDollar(f); to != f {
				filters[id] = to
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(filters) > 0 {
			seq, err := t.Seq()
			if err != nil {
				return err
			}
			for id, f := range filters {
				if _, err := t.exec(`UPDATE playlists SET filter = ?, seq = ? WHERE id = ?`, f, seq, id); err != nil {
					return err
				}
			}
		}
		return t.putJSON("flags_are_keys", true)
	})
}

// hashToDollar turns #name into $name for the names that were flags or keys, leaving tags and quoted text alone.
func hashToDollar(f string) string {
	old := map[string]bool{"unplayed": true, "played": true, "inprogress": true, "starred": true, "downloaded": true, "playlist": true,
		"title": true, "title.length": true, "channel": true, "length": true, "left": true, "published": true, "media": true, "type": true,
		"season": true, "number": true}
	r := []rune(f)
	quoted := false
	for i, c := range r {
		if c == '"' {
			quoted = !quoted
		}
		if c != '#' || quoted || (i > 0 && !unicode.IsSpace(r[i-1]) && r[i-1] != '(' && r[i-1] != '!') {
			continue
		}
		j := i + 1
		for j < len(r) && (unicode.IsLetter(r[j]) || unicode.IsDigit(r[j]) || strings.ContainsRune("_-.", r[j])) {
			j++
		}
		if old[strings.ToLower(string(r[i+1:j]))] {
			r[i] = '$'
		}
	}
	return string(r)
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) seal(plain string) []byte {
	if plain == "" {
		return nil
	}
	nonce := make([]byte, s.aead.NonceSize())
	_, _ = rand.Read(nonce)
	return s.aead.Seal(nonce, nonce, []byte(plain), nil)
}

func (s *Store) open(b []byte) string {
	n := s.aead.NonceSize()
	if len(b) <= n {
		return ""
	}
	p, err := s.aead.Open(nil, b[:n], b[n:], nil)
	if err != nil {
		return ""
	}
	return string(p)
}

// tx runs fn in one write transaction; seq hands out the change number every written row carries.
func (s *Store) tx(ctx context.Context, fn func(t *Tx) error) error {
	sqlTx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	t := &Tx{Tx: sqlTx, ctx: ctx}
	if err := fn(t); err != nil {
		_ = sqlTx.Rollback()
		return err
	}
	if t.seq != 0 {
		if _, err := sqlTx.ExecContext(ctx, `UPDATE meta SET v = ? WHERE k = 'seq'`, t.seq); err != nil {
			_ = sqlTx.Rollback()
			return err
		}
	}
	return sqlTx.Commit()
}

type Tx struct {
	*sql.Tx
	ctx context.Context
	seq int64
}

// Seq is the change number for this transaction, the same for every row it writes.
func (t *Tx) Seq() (int64, error) {
	if t.seq != 0 {
		return t.seq, nil
	}
	var v int64
	if err := t.QueryRowContext(t.ctx, `SELECT CAST(v AS INTEGER) FROM meta WHERE k = 'seq'`).Scan(&v); err != nil {
		return 0, err
	}
	t.seq = v + 1
	return t.seq, nil
}

func (t *Tx) exec(q string, args ...any) (sql.Result, error) { return t.ExecContext(t.ctx, q, args...) }

func (t *Tx) tombstone(kind string, ref any) error {
	seq, err := t.Seq()
	if err != nil {
		return err
	}
	_, err = t.exec(`INSERT INTO tombstones (kind, ref, seq) VALUES (?, ?, ?) ON CONFLICT (kind, ref) DO UPDATE SET seq = excluded.seq`, kind, fmt.Sprint(ref), seq)
	return err
}

func (s *Store) CurrentSeq(ctx context.Context) (int64, error) {
	var v int64
	err := s.db.QueryRowContext(ctx, `SELECT CAST(v AS INTEGER) FROM meta WHERE k = 'seq'`).Scan(&v)
	return v, err
}

// ServerSettings are the defaults every channel falls back to, set under Settings → Server.
type ServerSettings struct {
	CheckEvery  int `json:"check_every"`
	FetchCount  int `json:"fetch_count"`
	Keep        int `json:"keep"`
	DeepDepth   int `json:"deep_depth"`
	Refreshes   int `json:"refreshes"`
	Streams     int `json:"streams"`
	HistoryDays int `json:"history_days"`
	// CacheGB and CacheHours bound the media cache: whichever is reached first drops the files used longest ago.
	CacheGB    int `json:"cache_gb"`
	CacheHours int `json:"cache_hours"`
	// Quality is the video quality of every channel that sets none of its own.
	Quality string `json:"quality"`
}

func DefaultServerSettings() ServerSettings {
	return ServerSettings{CheckEvery: 1440, FetchCount: 20, DeepDepth: 1000, Refreshes: 2, Streams: 4, HistoryDays: 7, CacheGB: 5, CacheHours: 6, Quality: "720"}
}

// Prefs are the listener's own settings, shared by the browser and the app.
type Prefs struct {
	TimeZone    string `json:"time_zone"`
	SkipBack    int    `json:"skip_back"`
	SkipForward int    `json:"skip_forward"`
	NextAt      int    `json:"next_at"`
	Theme       string `json:"theme"`
	// RememberSpeed saves a speed changed while playing as the podcast's own speed.
	RememberSpeed bool `json:"remember_speed"`
}

func DefaultPrefs() Prefs {
	return Prefs{TimeZone: "UTC", SkipBack: 10, SkipForward: 30, NextAt: 5, Theme: "system"}
}

func (p Prefs) Location() *time.Location {
	if l, err := time.LoadLocation(p.TimeZone); err == nil {
		return l
	}
	return time.UTC
}

func (s *Store) getJSON(ctx context.Context, key string, v any) error {
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT v FROM settings WHERE k = ?`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(raw), v)
}

func (t *Tx) putJSON(key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = t.exec(`INSERT INTO settings (k, v) VALUES (?, ?) ON CONFLICT (k) DO UPDATE SET v = excluded.v`, key, string(b))
	return err
}

func (s *Store) ServerSettings(ctx context.Context) (ServerSettings, error) {
	v := DefaultServerSettings()
	return v, s.getJSON(ctx, "server", &v)
}

func (s *Store) SetServerSettings(ctx context.Context, v ServerSettings) error {
	return s.tx(ctx, func(t *Tx) error { return t.putJSON("server", v) })
}

func (s *Store) Prefs(ctx context.Context) (Prefs, error) {
	v := DefaultPrefs()
	return v, s.getJSON(ctx, "prefs", &v)
}

// SetPrefs bumps the change number so devices pick the new prefs up on their next sync.
func (s *Store) SetPrefs(ctx context.Context, v Prefs) error {
	return s.tx(ctx, func(t *Tx) error {
		seq, err := t.Seq()
		if err != nil {
			return err
		}
		if err := t.putJSON("prefs_seq", seq); err != nil {
			return err
		}
		return t.putJSON("prefs", v)
	})
}

func (s *Store) prefsSeq(ctx context.Context) int64 {
	var v int64
	_ = s.getJSON(ctx, "prefs_seq", &v)
	return v
}

type EngineInfo struct {
	Version     string    `json:"version"`
	LastUpdate  time.Time `json:"last_update"`
	LastJobID   int64     `json:"last_job_id"`
	LastOutcome string    `json:"last_outcome"`
}

func (s *Store) EngineInfo(ctx context.Context) (EngineInfo, error) {
	var v EngineInfo
	return v, s.getJSON(ctx, "engine", &v)
}

func (s *Store) SetEngineInfo(ctx context.Context, v EngineInfo) error {
	return s.tx(ctx, func(t *Tx) error { return t.putJSON("engine", v) })
}

func unix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func fromUnix(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.Unix(v, 0)
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// TagText stores tags space-padded so one tag can be matched with LIKE '% tag %'.
func tagText(tags []string) string {
	if len(tags) == 0 {
		return ""
	}
	return " " + strings.Join(tags, " ") + " "
}

func splitTags(v string) []string { return strings.Fields(v) }

func placeholders(n int) string {
	if n <= 0 {
		return "NULL"
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
