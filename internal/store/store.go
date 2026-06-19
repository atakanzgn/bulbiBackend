// Package store PostgreSQL tabanli kalici depolama saglar: gunluk skorlar
// (liderlik tablosu) ve push icin cihaz token'lari. pgx/v5 (pgxpool) kullanir.
package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"bulbi-backend/internal/content"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
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
	`CREATE TABLE IF NOT EXISTS words (
		id         BIGSERIAL PRIMARY KEY,
		text       TEXT      NOT NULL UNIQUE,
		length     INT       NOT NULL,
		created_at BIGINT    NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS questions (
		id         BIGSERIAL PRIMARY KEY,
		q          TEXT      NOT NULL,
		options    TEXT[]    NOT NULL,
		answer     INT       NOT NULL,
		category   TEXT      NOT NULL DEFAULT 'genel',
		difficulty TEXT      NOT NULL DEFAULT 'orta',
		created_at BIGINT    NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS meta (
		key   TEXT   PRIMARY KEY,
		value BIGINT NOT NULL
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

// --- Icerik (kelime + soru) — admin panelinden yonetilir ---

type WordRow struct {
	ID     int64
	Text   string
	Length int
}

type QuestionRow struct {
	ID         int64
	Q          string
	Options    []string
	Answer     int
	Category   string
	Difficulty string
}

// ContentVersion her icerik degisikliginde artan surum numarasi.
func (s *Store) ContentVersion(ctx context.Context) (int, error) {
	var v int
	err := s.pool.QueryRow(ctx,
		`SELECT value FROM meta WHERE key='content_version'`).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return v, err
}

func (s *Store) bumpVersion(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `
INSERT INTO meta (key, value) VALUES ('content_version', 1)
ON CONFLICT (key) DO UPDATE SET value = meta.value + 1`)
	return err
}

// ContentBundle uygulamaya sunulacak paketi (surum + kelimeler + sorular) doner.
func (s *Store) ContentBundle(ctx context.Context) (int, []string, []content.Question, error) {
	version, err := s.ContentVersion(ctx)
	if err != nil {
		return 0, nil, nil, err
	}

	wrows, err := s.pool.Query(ctx, `SELECT text FROM words ORDER BY id`)
	if err != nil {
		return 0, nil, nil, err
	}
	words := []string{}
	for wrows.Next() {
		var t string
		if err := wrows.Scan(&t); err != nil {
			wrows.Close()
			return 0, nil, nil, err
		}
		words = append(words, t)
	}
	wrows.Close()
	if err := wrows.Err(); err != nil {
		return 0, nil, nil, err
	}

	qrows, err := s.pool.Query(ctx,
		`SELECT q, options, answer, category, difficulty FROM questions ORDER BY id`)
	if err != nil {
		return 0, nil, nil, err
	}
	defer qrows.Close()
	questions := []content.Question{}
	for qrows.Next() {
		var q content.Question
		if err := qrows.Scan(&q.Q, &q.Options, &q.Answer, &q.Category, &q.Difficulty); err != nil {
			return 0, nil, nil, err
		}
		questions = append(questions, q)
	}
	return version, words, questions, qrows.Err()
}

func (s *Store) AddWord(ctx context.Context, text string) error {
	t := upperTR(text)
	if _, err := s.pool.Exec(ctx, `
INSERT INTO words (text, length, created_at) VALUES ($1, $2, $3)
ON CONFLICT (text) DO NOTHING`, t, len([]rune(t)), time.Now().Unix()); err != nil {
		return err
	}
	return s.bumpVersion(ctx)
}

// upperTR Turkce kurallarina gore buyuk harfe cevirir (i->İ, ı->I).
func upperTR(s string) string {
	return cases.Upper(language.Turkish).String(strings.TrimSpace(s))
}

func (s *Store) DeleteWord(ctx context.Context, id int64) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM words WHERE id=$1`, id); err != nil {
		return err
	}
	return s.bumpVersion(ctx)
}

func (s *Store) ListWords(ctx context.Context) ([]WordRow, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, text, length FROM words ORDER BY length, text`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WordRow{}
	for rows.Next() {
		var r WordRow
		if err := rows.Scan(&r.ID, &r.Text, &r.Length); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) AddQuestion(ctx context.Context, q string, options []string, answer int, category, difficulty string) error {
	if _, err := s.pool.Exec(ctx, `
INSERT INTO questions (q, options, answer, category, difficulty, created_at)
VALUES ($1, $2, $3, $4, $5, $6)`,
		q, options, answer, category, difficulty, time.Now().Unix()); err != nil {
		return err
	}
	return s.bumpVersion(ctx)
}

func (s *Store) DeleteQuestion(ctx context.Context, id int64) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM questions WHERE id=$1`, id); err != nil {
		return err
	}
	return s.bumpVersion(ctx)
}

func (s *Store) ListQuestions(ctx context.Context) ([]QuestionRow, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, q, options, answer, category, difficulty FROM questions ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []QuestionRow{}
	for rows.Next() {
		var r QuestionRow
		if err := rows.Scan(&r.ID, &r.Q, &r.Options, &r.Answer, &r.Category, &r.Difficulty); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SeedIfEmpty kelime/soru tablolari tamamen bossa verilen icerikten doldurur.
func (s *Store) SeedIfEmpty(ctx context.Context, c content.Content) (bool, error) {
	var n int
	if err := s.pool.QueryRow(ctx,
		`SELECT (SELECT COUNT(*) FROM words) + (SELECT COUNT(*) FROM questions)`).Scan(&n); err != nil {
		return false, err
	}
	if n > 0 {
		return false, nil
	}

	now := time.Now().Unix()
	for _, w := range c.Words {
		if _, err := s.pool.Exec(ctx, `
INSERT INTO words (text, length, created_at) VALUES ($1, $2, $3)
ON CONFLICT (text) DO NOTHING`, w, len([]rune(w)), now); err != nil {
			return false, err
		}
	}
	for _, q := range c.Questions {
		cat := q.Category
		if cat == "" {
			cat = "genel"
		}
		diff := q.Difficulty
		if diff == "" {
			diff = "orta"
		}
		if _, err := s.pool.Exec(ctx, `
INSERT INTO questions (q, options, answer, category, difficulty, created_at)
VALUES ($1, $2, $3, $4, $5, $6)`, q.Q, q.Options, q.Answer, cat, diff, now); err != nil {
			return false, err
		}
	}
	if _, err := s.pool.Exec(ctx, `
INSERT INTO meta (key, value) VALUES ('content_version', $1)
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`, max(c.Version, 1)); err != nil {
		return false, err
	}
	return true, nil
}

// --- Sayfalama ---

func (s *Store) CountWords(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM words`).Scan(&n)
	return n, err
}

func (s *Store) CountQuestions(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM questions`).Scan(&n)
	return n, err
}

func (s *Store) ListWordsPaged(ctx context.Context, limit, offset int) ([]WordRow, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, text, length FROM words ORDER BY length, text LIMIT $1 OFFSET $2`,
		limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WordRow{}
	for rows.Next() {
		var r WordRow
		if err := rows.Scan(&r.ID, &r.Text, &r.Length); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) ListQuestionsPaged(ctx context.Context, limit, offset int) ([]QuestionRow, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, q, options, answer, category, difficulty FROM questions ORDER BY id DESC LIMIT $1 OFFSET $2`,
		limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []QuestionRow{}
	for rows.Next() {
		var r QuestionRow
		if err := rows.Scan(&r.ID, &r.Q, &r.Options, &r.Answer, &r.Category, &r.Difficulty); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// --- Toplu ice aktarma (Excel) ---

// ImportWords verilen kelimeleri (Turkce buyuk harfe cevirip) ekler; eklenen
// yeni kelime sayisini doner.
func (s *Store) ImportWords(ctx context.Context, words []string) (int, error) {
	now := time.Now().Unix()
	added := 0
	for _, w := range words {
		t := upperTR(w)
		if t == "" {
			continue
		}
		tag, err := s.pool.Exec(ctx, `
INSERT INTO words (text, length, created_at) VALUES ($1, $2, $3)
ON CONFLICT (text) DO NOTHING`, t, len([]rune(t)), now)
		if err != nil {
			return added, err
		}
		added += int(tag.RowsAffected())
	}
	if len(words) > 0 {
		if err := s.bumpVersion(ctx); err != nil {
			return added, err
		}
	}
	return added, nil
}

// ImportQuestions verilen sorulari ekler; eklenen soru sayisini doner.
func (s *Store) ImportQuestions(ctx context.Context, qs []content.Question) (int, error) {
	now := time.Now().Unix()
	added := 0
	for _, q := range qs {
		if q.Q == "" || len(q.Options) < 2 || q.Answer < 0 || q.Answer >= len(q.Options) {
			continue
		}
		cat := q.Category
		if cat == "" {
			cat = "genel"
		}
		diff := q.Difficulty
		if diff == "" {
			diff = "orta"
		}
		if _, err := s.pool.Exec(ctx, `
INSERT INTO questions (q, options, answer, category, difficulty, created_at)
VALUES ($1, $2, $3, $4, $5, $6)`, q.Q, q.Options, q.Answer, cat, diff, now); err != nil {
			return added, err
		}
		added++
	}
	if added > 0 {
		if err := s.bumpVersion(ctx); err != nil {
			return added, err
		}
	}
	return added, nil
}

// DeleteDevice bir cihazin push token kaydini siler (bildirim kapatildiginda).
func (s *Store) DeleteDevice(ctx context.Context, deviceID string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM devices WHERE device_id=$1`, deviceID)
	return err
}
