// Package store SQLite tabanli kalici depolama saglar: gunluk skorlar
// (liderlik tablosu) ve push icin cihaz token'lari. modernc.org/sqlite ile
// cgo gerektirmez (Windows/Linux'ta tek binary).
package store

import (
	"database/sql"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct{ db *sql.DB }

// Entry liderlik tablosu satiri.
type Entry struct {
	Name  string `json:"name"`
	Score int    `json:"score"`
	Rank  int    `json:"rank"`
}

const schema = `
CREATE TABLE IF NOT EXISTS scores (
  device_id  TEXT    NOT NULL,
  name       TEXT    NOT NULL DEFAULT 'Anonim',
  puzzle     TEXT    NOT NULL,
  day        INTEGER NOT NULL,
  score      INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (device_id, puzzle, day)
);
CREATE INDEX IF NOT EXISTS idx_scores_board ON scores(puzzle, day, score DESC);
CREATE TABLE IF NOT EXISTS devices (
  device_id  TEXT PRIMARY KEY,
  token      TEXT NOT NULL,
  platform   TEXT NOT NULL,
  updated_at INTEGER NOT NULL
);
`

// Open veritabanini acar ve semayi olusturur.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// SubmitScore cihaz/bulmaca/gun icin skoru kaydeder; mevcut skordan iyiyse
// gunceller (gunde tek, en iyi skor).
func (s *Store) SubmitScore(deviceID, name, puzzle string, day, score int) error {
	_, err := s.db.Exec(`
INSERT INTO scores (device_id, name, puzzle, day, score, updated_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(device_id, puzzle, day) DO UPDATE SET
  score      = MAX(score, excluded.score),
  name       = excluded.name,
  updated_at = excluded.updated_at`,
		deviceID, name, puzzle, day, score, time.Now().Unix())
	return err
}

// Leaderboard belirli gun/bulmaca icin en yuksek [limit] skoru dondurur.
func (s *Store) Leaderboard(puzzle string, day, limit int) ([]Entry, error) {
	rows, err := s.db.Query(`
SELECT name, score FROM scores
WHERE puzzle = ? AND day = ?
ORDER BY score DESC, updated_at ASC
LIMIT ?`, puzzle, day, limit)
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
func (s *Store) MyRank(puzzle string, day int, deviceID string) (rank, score int, found bool, err error) {
	err = s.db.QueryRow(
		`SELECT score FROM scores WHERE device_id=? AND puzzle=? AND day=?`,
		deviceID, puzzle, day).Scan(&score)
	if err == sql.ErrNoRows {
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, err
	}
	err = s.db.QueryRow(
		`SELECT COUNT(*)+1 FROM scores WHERE puzzle=? AND day=? AND score > ?`,
		puzzle, day, score).Scan(&rank)
	if err != nil {
		return 0, 0, false, err
	}
	return rank, score, true, nil
}

// SaveDevice push icin cihaz token'ini kaydeder/gunceller.
func (s *Store) SaveDevice(deviceID, token, platform string) error {
	_, err := s.db.Exec(`
INSERT INTO devices (device_id, token, platform, updated_at)
VALUES (?, ?, ?, ?)
ON CONFLICT(device_id) DO UPDATE SET
  token      = excluded.token,
  platform   = excluded.platform,
  updated_at = excluded.updated_at`,
		deviceID, token, platform, time.Now().Unix())
	return err
}

// AllTokens kayitli tum push token'larini dondurur.
func (s *Store) AllTokens() ([]string, error) {
	rows, err := s.db.Query(`SELECT token FROM devices`)
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
