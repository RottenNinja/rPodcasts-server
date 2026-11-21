package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Job kinds.
const (
	JobRefresh = "refresh"
	JobDeep    = "deep"
	JobEngine  = "engine-update"
	JobDates   = "fix-dates"
)

// Job states.
const (
	JobQueued    = "queued"
	JobRunning   = "running"
	JobDone      = "done"
	JobFailed    = "failed"
	JobCancelled = "cancelled"
)

type Job struct {
	ID          int64
	Kind        string
	ChannelID   int64
	Label       string
	Reason      string
	Depth       int
	State       string
	Queued      time.Time
	Started     time.Time
	Finished    time.Time
	Added       int
	Updated     int
	Pruned      int
	NotModified bool
	Summary     string
	Error       string
}

func (j Job) Took() time.Duration {
	if j.Started.IsZero() || j.Finished.IsZero() {
		return 0
	}
	return j.Finished.Sub(j.Started)
}

const jobCols = `id, kind, channel_id, label, reason, depth, state, queued, started, finished, added, updated, pruned, not_modified, summary, error`

func scanJob(row interface{ Scan(...any) error }) (Job, error) {
	var j Job
	var q, s, f int64
	err := row.Scan(&j.ID, &j.Kind, &j.ChannelID, &j.Label, &j.Reason, &j.Depth, &j.State, &q, &s, &f, &j.Added, &j.Updated, &j.Pruned, &j.NotModified, &j.Summary, &j.Error)
	if errors.Is(err, sql.ErrNoRows) {
		return j, ErrNotFound
	}
	j.Queued, j.Started, j.Finished = fromUnix(q), fromUnix(s), fromUnix(f)
	return j, err
}

func (s *Store) InsertJob(ctx context.Context, j Job) (int64, error) {
	r, err := s.db.ExecContext(ctx, `INSERT INTO jobs (kind, channel_id, label, reason, depth, state, queued, started) VALUES (?,?,?,?,?,?,?,?)`,
		j.Kind, j.ChannelID, j.Label, j.Reason, j.Depth, j.State, unix(j.Queued), unix(j.Started))
	if err != nil {
		return 0, err
	}
	return r.LastInsertId()
}

func (s *Store) SaveJob(ctx context.Context, j Job) error {
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET label=?, state=?, started=?, finished=?, added=?, updated=?, pruned=?, not_modified=?, summary=?, error=? WHERE id=?`,
		j.Label, j.State, unix(j.Started), unix(j.Finished), j.Added, j.Updated, j.Pruned, b2i(j.NotModified), j.Summary, j.Error, j.ID)
	return err
}

func (s *Store) Job(ctx context.Context, id int64) (Job, error) {
	return scanJob(s.db.QueryRowContext(ctx, `SELECT `+jobCols+` FROM jobs WHERE id = ?`, id))
}

// History lists finished runs, newest first.
func (s *Store) History(ctx context.Context, failedOnly bool, limit int) ([]Job, error) {
	q := `SELECT ` + jobCols + ` FROM jobs WHERE state IN ('done','failed','cancelled')`
	if failedOnly {
		q += ` AND state = 'failed'`
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY finished DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// LastFailed reports whether the most recent run of anything failed.
func (s *Store) LastFailed(ctx context.Context) bool {
	var state string
	_ = s.db.QueryRowContext(ctx, `SELECT state FROM jobs WHERE state IN ('done','failed') ORDER BY finished DESC, id DESC LIMIT 1`).Scan(&state)
	return state == JobFailed
}

func (s *Store) PruneHistory(ctx context.Context, keep time.Duration) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM jobs WHERE state IN ('done','failed','cancelled') AND finished < ?`, time.Now().Add(-keep).Unix())
	return err
}

// AbandonRunning marks runs left over from a previous process as failed.
func (s *Store) AbandonRunning(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET state='failed', error='server restarted', finished=? WHERE state IN ('queued','running')`, time.Now().Unix())
	return err
}
