// Package store PostgreSQL tabanli kalici depolama saglar: gunluk skorlar
// (liderlik tablosu) ve push icin cihaz token'lari. pgx/v5 (pgxpool) kullanir.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand"
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

// League bir arkadas ligi (davet kodlu ozel liderlik).
type League struct {
	ID   int64  `json:"id"`
	Code string `json:"code"`
	Name string `json:"name"`
}

// ErrLeagueNotFound gecersiz davet kodu icin doner.
var ErrLeagueNotFound = errors.New("lig bulunamadi")

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
	`CREATE TABLE IF NOT EXISTS connections (
		id         BIGSERIAL PRIMARY KEY,
		data       JSONB     NOT NULL,
		created_at BIGINT    NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS leagues (
		id           BIGSERIAL PRIMARY KEY,
		code         TEXT      NOT NULL UNIQUE,
		name         TEXT      NOT NULL,
		owner_device TEXT      NOT NULL,
		created_at   BIGINT    NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS league_members (
		league_id BIGINT NOT NULL REFERENCES leagues(id) ON DELETE CASCADE,
		device_id TEXT   NOT NULL,
		name      TEXT   NOT NULL DEFAULT 'Anonim',
		joined_at BIGINT NOT NULL,
		PRIMARY KEY (league_id, device_id)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_league_members_device ON league_members (device_id)`,
	`CREATE TABLE IF NOT EXISTS purchases (
		transaction_id TEXT    PRIMARY KEY,
		device_id      TEXT    NOT NULL,
		product_id     TEXT    NOT NULL,
		platform       TEXT    NOT NULL,
		coins          INTEGER NOT NULL,
		created_at     BIGINT  NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS announcements (
		id         BIGSERIAL PRIMARY KEY,
		title      TEXT    NOT NULL,
		body       TEXT    NOT NULL DEFAULT '',
		code       TEXT    NOT NULL DEFAULT '',
		active     BOOLEAN NOT NULL DEFAULT TRUE,
		created_at BIGINT  NOT NULL
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

// LeaderboardWeekly [dayStart..dayEnd] penceresinde cihaz basina toplam skoru
// (gunluk skorlarin toplami) en yuksekten dusuge dondurur. Ad, penceredeki en
// guncel gunden alinir.
func (s *Store) LeaderboardWeekly(ctx context.Context, puzzle string, dayStart, dayEnd, limit int) ([]Entry, error) {
	rows, err := s.pool.Query(ctx, `
SELECT (array_agg(name ORDER BY day DESC))[1] AS name, SUM(score)::int AS total
FROM scores
WHERE puzzle = $1 AND day BETWEEN $2 AND $3
GROUP BY device_id
ORDER BY total DESC, MAX(updated_at) ASC
LIMIT $4`, puzzle, dayStart, dayEnd, limit)
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

// MyRankWeekly bir cihazin [dayStart..dayEnd] penceresindeki toplam skorunu ve
// sirasini dondurur. Pencerede hic skoru yoksa found=false.
func (s *Store) MyRankWeekly(ctx context.Context, puzzle string, dayStart, dayEnd int, deviceID string) (rank, score int, found bool, err error) {
	var cnt int
	err = s.pool.QueryRow(ctx,
		`SELECT COUNT(*), COALESCE(SUM(score), 0)::int FROM scores
		 WHERE device_id=$1 AND puzzle=$2 AND day BETWEEN $3 AND $4`,
		deviceID, puzzle, dayStart, dayEnd).Scan(&cnt, &score)
	if err != nil {
		return 0, 0, false, err
	}
	if cnt == 0 {
		return 0, 0, false, nil
	}
	err = s.pool.QueryRow(ctx, `
SELECT COUNT(*)+1 FROM (
  SELECT device_id, SUM(score) AS total
  FROM scores WHERE puzzle=$1 AND day BETWEEN $2 AND $3
  GROUP BY device_id
) t WHERE t.total > $4`,
		puzzle, dayStart, dayEnd, score).Scan(&rank)
	if err != nil {
		return 0, 0, false, err
	}
	return rank, score, true, nil
}

const _codeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789" // O/0/I/1 yok

func genLeagueCode() string {
	b := make([]byte, 4)
	for i := range b {
		b[i] = _codeAlphabet[rand.Intn(len(_codeAlphabet))]
	}
	return "BULBI-" + string(b)
}

// CreateLeague benzersiz kodlu bir lig olusturur ve sahibini ([playerName] ile)
// uye yapar. [leagueName] ligin adi, [playerName] sahibin gorunen adidir.
func (s *Store) CreateLeague(ctx context.Context, deviceID, playerName, leagueName string) (League, error) {
	now := time.Now().Unix()
	for attempt := 0; attempt < 8; attempt++ {
		code := genLeagueCode()
		var id int64
		err := s.pool.QueryRow(ctx,
			`INSERT INTO leagues (code, name, owner_device, created_at)
			 VALUES ($1,$2,$3,$4) ON CONFLICT (code) DO NOTHING RETURNING id`,
			code, leagueName, deviceID, now).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			continue // kod carpismasi -> yeniden dene
		}
		if err != nil {
			return League{}, err
		}
		if _, err := s.pool.Exec(ctx,
			`INSERT INTO league_members (league_id, device_id, name, joined_at)
			 VALUES ($1,$2,$3,$4)
			 ON CONFLICT (league_id, device_id) DO UPDATE SET name=EXCLUDED.name`,
			id, deviceID, playerName, now); err != nil {
			return League{}, err
		}
		return League{ID: id, Code: code, Name: leagueName}, nil
	}
	return League{}, errors.New("benzersiz kod uretilemedi")
}

// JoinLeague koda gore cihazi lige katar (varsa adini gunceller).
func (s *Store) JoinLeague(ctx context.Context, deviceID, name, code string) (League, error) {
	var lg League
	err := s.pool.QueryRow(ctx,
		`SELECT id, code, name FROM leagues WHERE code=$1`, code).
		Scan(&lg.ID, &lg.Code, &lg.Name)
	if errors.Is(err, pgx.ErrNoRows) {
		return League{}, ErrLeagueNotFound
	}
	if err != nil {
		return League{}, err
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO league_members (league_id, device_id, name, joined_at)
		 VALUES ($1,$2,$3,$4)
		 ON CONFLICT (league_id, device_id) DO UPDATE SET name=EXCLUDED.name`,
		lg.ID, deviceID, name, time.Now().Unix()); err != nil {
		return League{}, err
	}
	return lg, nil
}

// MyLeagues cihazin uye oldugu ligleri dondurur.
func (s *Store) MyLeagues(ctx context.Context, deviceID string) ([]League, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT l.id, l.code, l.name FROM leagues l
		 JOIN league_members m ON m.league_id = l.id
		 WHERE m.device_id = $1 ORDER BY m.joined_at DESC`, deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []League{}
	for rows.Next() {
		var l League
		if err := rows.Scan(&l.ID, &l.Code, &l.Name); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// LeagueBoard lig uyelerini [dayStart..dayEnd] penceresindeki haftalik 'daily'
// skor toplamina gore siralar. Lig yoksa bos liste doner.
func (s *Store) LeagueBoard(ctx context.Context, code string, dayStart, dayEnd int) ([]Entry, error) {
	rows, err := s.pool.Query(ctx, `
SELECT m.name, COALESCE(SUM(s.score), 0)::int AS total
FROM league_members m
JOIN leagues l ON l.id = m.league_id
LEFT JOIN scores s ON s.device_id = m.device_id
  AND s.puzzle = 'daily' AND s.day BETWEEN $2 AND $3
WHERE l.code = $1
GROUP BY m.device_id, m.name
ORDER BY total DESC, m.name ASC`, code, dayStart, dayEnd)
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

// RecordPurchase satin almayi idempotent kaydeder: ilk kez isleniyorsa true,
// daha once islenmisse false doner (coin TEKRAR verilmemeli).
func (s *Store) RecordPurchase(ctx context.Context, txnID, deviceID, productID, platform string, coins int) (bool, error) {
	ct, err := s.pool.Exec(ctx,
		`INSERT INTO purchases (transaction_id, device_id, product_id, platform, coins, created_at)
		 VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT (transaction_id) DO NOTHING`,
		txnID, deviceID, productID, platform, coins, time.Now().Unix())
	if err != nil {
		return false, err
	}
	return ct.RowsAffected() > 0, nil
}

// Announcement ana ekranda gosterilen kampanya/duyuru satiri.
type Announcement struct {
	ID        int64
	Title     string
	Body      string
	Code      string
	Active    bool
	CreatedAt int64
}

// AddAnnouncement yeni duyuru ekler (ayni anda birden fazla duyuru olabilir).
func (s *Store) AddAnnouncement(ctx context.Context, a Announcement) error {
	_, err := s.pool.Exec(ctx, `
INSERT INTO announcements (title, body, code, active, created_at)
VALUES ($1, $2, $3, $4, $5)`,
		a.Title, a.Body, a.Code, a.Active, time.Now().Unix())
	return err
}

// DeleteAnnouncement duyuruyu tamamen siler.
func (s *Store) DeleteAnnouncement(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM announcements WHERE id = $1`, id)
	return err
}

// ToggleAnnouncement aktif/pasif durumunu tersine cevirir.
func (s *Store) ToggleAnnouncement(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE announcements SET active = NOT active WHERE id = $1`, id)
	return err
}

// ListAnnouncements tum duyurulari (admin icin) yeniden eskiye dogru doner.
// activeOnly true ise yalnizca aktif olanlar (uygulama API'si) doner.
func (s *Store) ListAnnouncements(ctx context.Context, activeOnly bool) ([]Announcement, error) {
	q := `SELECT id, title, body, code, active, created_at FROM announcements`
	if activeOnly {
		q += ` WHERE active`
	}
	q += ` ORDER BY id DESC`
	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Announcement
	for rows.Next() {
		var a Announcement
		if err := rows.Scan(&a.ID, &a.Title, &a.Body, &a.Code, &a.Active, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
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

// DeleteTokens verilen push token'larini siler (olu/kayitsiz token temizligi).
func (s *Store) DeleteTokens(ctx context.Context, tokens []string) error {
	if len(tokens) == 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx, `DELETE FROM devices WHERE token = ANY($1)`, tokens)
	return err
}

// StreakReminderTokens, [yesterday] gunu 'daily' oynamis ama [today] gunu
// oynamamis cihazlarin push token'larini doner (seri-koruma hatirlatmasi icin).
func (s *Store) StreakReminderTokens(ctx context.Context, yesterday, today int) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
SELECT d.token FROM devices d
WHERE EXISTS (
    SELECT 1 FROM scores s
    WHERE s.device_id = d.device_id AND s.puzzle = 'daily' AND s.day = $1)
  AND NOT EXISTS (
    SELECT 1 FROM scores s
    WHERE s.device_id = d.device_id AND s.puzzle = 'daily' AND s.day = $2)`,
		yesterday, today)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
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

// ContentBundle uygulamaya sunulacak paketi (surum + kelimeler + sorular +
// baglantilar) doner.
func (s *Store) ContentBundle(ctx context.Context) (int, []string, []content.Question, []content.ConnectionPuzzle, error) {
	version, err := s.ContentVersion(ctx)
	if err != nil {
		return 0, nil, nil, nil, err
	}

	wrows, err := s.pool.Query(ctx, `SELECT text FROM words ORDER BY id`)
	if err != nil {
		return 0, nil, nil, nil, err
	}
	words := []string{}
	for wrows.Next() {
		var t string
		if err := wrows.Scan(&t); err != nil {
			wrows.Close()
			return 0, nil, nil, nil, err
		}
		words = append(words, t)
	}
	wrows.Close()
	if err := wrows.Err(); err != nil {
		return 0, nil, nil, nil, err
	}

	qrows, err := s.pool.Query(ctx,
		`SELECT q, options, answer, category, difficulty FROM questions ORDER BY id`)
	if err != nil {
		return 0, nil, nil, nil, err
	}
	questions := []content.Question{}
	for qrows.Next() {
		var q content.Question
		if err := qrows.Scan(&q.Q, &q.Options, &q.Answer, &q.Category, &q.Difficulty); err != nil {
			qrows.Close()
			return 0, nil, nil, nil, err
		}
		questions = append(questions, q)
	}
	qrows.Close()
	if err := qrows.Err(); err != nil {
		return 0, nil, nil, nil, err
	}

	crows, err := s.pool.Query(ctx, `SELECT data FROM connections ORDER BY id`)
	if err != nil {
		return 0, nil, nil, nil, err
	}
	conns := []content.ConnectionPuzzle{}
	for crows.Next() {
		var raw []byte
		if err := crows.Scan(&raw); err != nil {
			crows.Close()
			return 0, nil, nil, nil, err
		}
		var p content.ConnectionPuzzle
		if err := json.Unmarshal(raw, &p); err != nil {
			crows.Close()
			return 0, nil, nil, nil, err
		}
		conns = append(conns, p)
	}
	crows.Close()
	return version, words, questions, conns, crows.Err()
}

// AddWord kelimeyi TR büyük harfe çevirip ekler; zaten varsa eklemez. Yeni
// eklendiyse true döner (tekrar tespiti için).
func (s *Store) AddWord(ctx context.Context, text string) (bool, error) {
	t := upperTR(text)
	ct, err := s.pool.Exec(ctx, `
INSERT INTO words (text, length, created_at) VALUES ($1, $2, $3)
ON CONFLICT (text) DO NOTHING`, t, len([]rune(t)), time.Now().Unix())
	if err != nil {
		return false, err
	}
	if ct.RowsAffected() == 0 {
		return false, nil // zaten vardı
	}
	return true, s.bumpVersion(ctx)
}

// upperTR Turkce kurallarina gore buyuk harfe cevirir (i->İ, ı->I).
func upperTR(s string) string {
	return cases.Upper(language.Turkish).String(strings.TrimSpace(s))
}

// UpperTR upperTR'nin disa acik halidir (or. admin baglanti ekleme).
func UpperTR(s string) string { return upperTR(s) }

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

// --- Baglantilar (Connections) ---

type ConnectionRow struct {
	ID     int64
	Groups []content.ConnectionGroup
}

func (s *Store) CountConnections(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM connections`).Scan(&n)
	return n, err
}

func (s *Store) AddConnection(ctx context.Context, p content.ConnectionPuzzle) error {
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO connections (data, created_at) VALUES ($1, $2)`,
		data, time.Now().Unix()); err != nil {
		return err
	}
	return s.bumpVersion(ctx)
}

func (s *Store) DeleteConnection(ctx context.Context, id int64) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM connections WHERE id=$1`, id); err != nil {
		return err
	}
	return s.bumpVersion(ctx)
}

func (s *Store) ListConnections(ctx context.Context) ([]ConnectionRow, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, data FROM connections ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ConnectionRow{}
	for rows.Next() {
		var id int64
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		var p content.ConnectionPuzzle
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		out = append(out, ConnectionRow{ID: id, Groups: p.Groups})
	}
	return out, rows.Err()
}

// SeedConnectionsIfEmpty connections tablosu bossa verilen icerikten doldurur
// (kelime/soru'dan bagimsiz; mevcut kurulumlara da eklenebilsin diye).
func (s *Store) SeedConnectionsIfEmpty(ctx context.Context, c content.Content) (bool, error) {
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM connections`).Scan(&n); err != nil {
		return false, err
	}
	if n > 0 || len(c.Connections) == 0 {
		return false, nil
	}
	now := time.Now().Unix()
	for _, p := range c.Connections {
		data, err := json.Marshal(p)
		if err != nil {
			return false, err
		}
		if _, err := s.pool.Exec(ctx,
			`INSERT INTO connections (data, created_at) VALUES ($1, $2)`,
			data, now); err != nil {
			return false, err
		}
	}
	return true, nil
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

// CountWords kelime sayısını döner; [search] verilirse (TR büyük) o desene uyanlar.
func (s *Store) CountWords(ctx context.Context, search string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM words WHERE ($1 = '' OR text LIKE '%' || $1 || '%')`,
		upperTR(search)).Scan(&n)
	return n, err
}

func (s *Store) CountQuestions(ctx context.Context) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM questions`).Scan(&n)
	return n, err
}

func (s *Store) ListWordsPaged(ctx context.Context, search string, limit, offset int) ([]WordRow, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, text, length FROM words
		 WHERE ($1 = '' OR text LIKE '%' || $1 || '%')
		 ORDER BY length, text LIMIT $2 OFFSET $3`,
		upperTR(search), limit, offset)
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
