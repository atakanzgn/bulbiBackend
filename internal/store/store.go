// Package store PostgreSQL tabanli kalici depolama saglar: gunluk skorlar
// (liderlik tablosu) ve push icin cihaz token'lari. pgx/v5 (pgxpool) kullanir.
package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct{ pool *pgxpool.Pool }

// Entry liderlik tablosu satiri.
type Entry struct {
	Name  string `json:"name"`
	Score int    `json:"score"`
	Rank  int    `json:"rank"`
}

var migrations = []string{
	`CREATE TABLE IF NOT EXISTS scores (
		device_id  TEXT    NOT NULL,
		name       TEXT    NOT NULL DEFAULT 'Anonim',
		puzzle     TEXT    NOT NULL,
		day        INTEGER NOT NULL,
		score      INTEGER NOT NULL,
		updated_at BIGINT  NOT NULL,
		PRIMARY KEY (device_id, puzzle, day)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_scores_board ON scores (puzzle, day, score DESC)`,
	`CREATE TABLE IF NOT EXISTS devices (
		device_id  TEXT   PRIMARY KEY,
		token      TEXT   NOT NULL,
		platform   TEXT   NOT NULL,
		updated_at BIGINT NOT NULL
	)`,
}

// Open havuzu acar, baglantiyi dogrular ve semayi olusturur.
// dsn ornegi: postgres://bulbi:sifre@db:5432/bulbi?sslmode=disable
func Open(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	for _, m := range migrations {
		if _, err := pool.Exec(ctx, m); err != nil {
			pool.Close()
			return nil, err
		}
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close() { s.pool.Close() }

// SubmitScore cihaz/bulmaca/gun icin skoru kaydeder; mevcut skordan iyiyse
// gunceller (gunde tek, en iyi skor).
func (s *Store) SubmitScore(ctx context.Context, deviceID, name, puzzle string, day, score int) error {
	_, err := s.pool.Exec(ctx, `
INSERT INTO scores (device_id, name, puzzle, day, score, updated_at)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (device_id, puzzle, day) DO UPDATE SET
  score      = GREATEST(scores.score, EXCLUDED.score),
  name       = EXCLUDED.name,
  updated_at = EXCLUDED.updated_at`,
		deviceID, name, puzzle, day, score, time.Now().Unix())
	return err
}

// Leaderboard belirli gun/bulmaca icin en yuksek [limit] skoru dondurur.
func (s *Store) Leaderboard(ctx context.Context, puzzle string, day, limit int) ([]Entry, error) {
	rows, err := s.pool.Query(ctx, `
SELECT name, score FROM scores
WHERE puzzle = $1 AND day = $2
ORDER BY score DESC, updated_at ASC
LIMIT $3`, puzzle, day, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Entry{}
	rank := 0
	for rows.Next() {
		rank++
		var e Entry
		if err := rows.Scan(&e.Name, &e.Score); err != nil {
			return nil, err
		}
		e.Rank = rank
		out = append(out, e)
	}
	return out, rows.Err()
}

// MyRank bir cihazin belirli gun/bulmacadaki sirasini ve skorunu dondurur.
func (s *Store) MyRank(ctx context.Context, puzzle string, day int, deviceID string) (rank, score int, found bool, err error) {
	err = s.pool.QueryRow(ctx,
		`SELECT score FROM scores WHERE device_id=$1 AND puzzle=$2 AND day=$3`,
		deviceID, puzzle, day).Scan(&score)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, err
	}
	err = s.pool.QueryRow(ctx,
		`SELECT COUNT(*)+1 FROM scores WHERE puzzle=$1 AND day=$2 AND score > $3`,
		puzzle, day, score).Scan(&rank)
	if err != nil {
		return 0, 0, false, err
	}
	return rank, score, true, nil
}

// SaveDevice push icin cihaz token'ini kaydeder/gunceller.
func (s *Store) SaveDevice(ctx context.Context, deviceID, token, platform string) error {
	_, err := s.pool.Exec(ctx, `
INSERT INTO devices (device_id, token, platform, updated_at)
VALUES ($1, $2, $3, $4)
ON CONFLICT (device_id) DO UPDATE SET
  token      = EXCLUDED.token,
  platform   = EXCLUDED.platform,
  updated_at = EXCLUDED.updated_at`,
		deviceID, token, platform, time.Now().Unix())
	return err
}

// AllTokens kayitli tum push token'larini dondurur.
func (s *Store) AllTokens(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT token FROM devices`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	tokens := []string{}
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		tokens = append(tokens, t)
	}
	return tokens, rows.Err()
}
