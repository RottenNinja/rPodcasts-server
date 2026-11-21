package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"
)

type Device struct {
	ID       int64
	Name     string
	Created  time.Time
	LastSync time.Time
}

func hashToken(tok string) string {
	h := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(h[:])
}

// CreateDevice returns the new device and its token, which is shown once and stored only as a hash.
func (s *Store) CreateDevice(ctx context.Context, name string) (Device, string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return Device{}, "", err
	}
	tok := hex.EncodeToString(b)
	r, err := s.db.ExecContext(ctx, `INSERT INTO devices (name, token_hash, created) VALUES (?,?,?)`, name, hashToken(tok), time.Now().Unix())
	if err != nil {
		return Device{}, "", err
	}
	id, err := r.LastInsertId()
	if err != nil {
		return Device{}, "", err
	}
	d, err := s.Device(ctx, id)
	return d, tok, err
}

const deviceCols = `id, name, created, last_sync`

func scanDevice(row interface{ Scan(...any) error }) (Device, error) {
	var d Device
	var created, last int64
	err := row.Scan(&d.ID, &d.Name, &created, &last)
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrNotFound
	}
	if err != nil {
		return d, err
	}
	d.Created, d.LastSync = fromUnix(created), fromUnix(last)
	return d, nil
}

func (s *Store) Device(ctx context.Context, id int64) (Device, error) {
	return scanDevice(s.db.QueryRowContext(ctx, `SELECT `+deviceCols+` FROM devices WHERE id = ?`, id))
}

func (s *Store) DeviceByToken(ctx context.Context, tok string) (Device, error) {
	return scanDevice(s.db.QueryRowContext(ctx, `SELECT `+deviceCols+` FROM devices WHERE token_hash = ?`, hashToken(tok)))
}

func (s *Store) Devices(ctx context.Context) ([]Device, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+deviceCols+` FROM devices ORDER BY name, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Device
	for rows.Next() {
		d, err := scanDevice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (s *Store) TouchDevice(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE devices SET last_sync=? WHERE id=?`, time.Now().Unix(), id)
	return err
}

func (s *Store) DeleteDevice(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM devices WHERE id=?`, id)
	return err
}
