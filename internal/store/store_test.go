package store

import (
	"context"
	"errors"
	"os"
	"testing"
)

// TEST_DATABASE_URL tanimli degilse testler atlanir (CI/yerelde bir Postgres
// gerekir). Ornek: postgres://test:test@localhost:5433/testdb?sslmode=disable
func newTestStore(t *testing.T) (*Store, context.Context) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL yok; Postgres testleri atlandi")
	}
	ctx := context.Background()
	s, err := Open(ctx, dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Her test temiz baslasin.
	if _, err := s.pool.Exec(ctx, `TRUNCATE scores, devices`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	t.Cleanup(s.Close)
	return s, ctx
}

func TestSubmitScoreKeepsBest(t *testing.T) {
	s, ctx := newTestStore(t)

	if err := s.SubmitScore(ctx, "dev1", "Ali", "word", 5, 3); err != nil {
		t.Fatal(err)
	}
	// Daha dusuk skor mevcut en iyiyi degistirmemeli.
	if err := s.SubmitScore(ctx, "dev1", "Ali", "word", 5, 1); err != nil {
		t.Fatal(err)
	}
	rank, score, found, err := s.MyRank(ctx, "word", 5, "dev1")
	if err != nil || !found {
		t.Fatalf("MyRank: %v found=%v", err, found)
	}
	if score != 3 {
		t.Errorf("en iyi skor 3 olmali, %d bulundu", score)
	}
	if rank != 1 {
		t.Errorf("rank 1 olmali, %d", rank)
	}
}

func TestLeaderboardOrderAndRank(t *testing.T) {
	s, ctx := newTestStore(t)
	_ = s.SubmitScore(ctx, "a", "Ayşe", "quiz", 10, 5)
	_ = s.SubmitScore(ctx, "b", "Veli", "quiz", 10, 3)
	_ = s.SubmitScore(ctx, "c", "Can", "quiz", 10, 4)

	top, err := s.Leaderboard(ctx, "quiz", 10, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(top) != 3 {
		t.Fatalf("3 kayit bekleniyordu, %d", len(top))
	}
	if top[0].Name != "Ayşe" || top[0].Rank != 1 {
		t.Errorf("ilk sira Ayşe olmali: %+v", top[0])
	}
	if top[2].Name != "Veli" {
		t.Errorf("son sira Veli olmali: %+v", top[2])
	}

	rank, score, found, _ := s.MyRank(ctx, "quiz", 10, "b")
	if !found || score != 3 || rank != 3 {
		t.Errorf("Veli rank=3 score=3 olmali, rank=%d score=%d found=%v", rank, score, found)
	}
}

func TestWeeklyLeaderboardSumsWindow(t *testing.T) {
	s, ctx := newTestStore(t)
	// Hafta penceresi 4..10; 'daily' skorlari.
	_ = s.SubmitScore(ctx, "a", "Ayşe", "daily", 8, 2)
	_ = s.SubmitScore(ctx, "a", "Ayşe", "daily", 9, 3)
	_ = s.SubmitScore(ctx, "a", "Ayşe", "daily", 10, 1) // toplam 6
	_ = s.SubmitScore(ctx, "b", "Veli", "daily", 10, 5) // toplam 5
	// Pencere disindaki gun sayilmamali.
	_ = s.SubmitScore(ctx, "b", "Veli", "daily", 3, 100)

	top, err := s.LeaderboardWeekly(ctx, "daily", 4, 10, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(top) != 2 {
		t.Fatalf("2 kayit bekleniyordu, %d", len(top))
	}
	if top[0].Name != "Ayşe" || top[0].Score != 6 || top[0].Rank != 1 {
		t.Errorf("ilk sira Ayşe 6 olmali: %+v", top[0])
	}
	if top[1].Name != "Veli" || top[1].Score != 5 {
		t.Errorf("ikinci sira Veli 5 olmali: %+v", top[1])
	}

	rank, score, found, _ := s.MyRankWeekly(ctx, "daily", 4, 10, "b")
	if !found || score != 5 || rank != 2 {
		t.Errorf("Veli haftalik rank=2 score=5 olmali, rank=%d score=%d found=%v", rank, score, found)
	}

	if _, _, found2, _ := s.MyRankWeekly(ctx, "daily", 4, 10, "zzz"); found2 {
		t.Error("skoru olmayan cihaz found=false olmali")
	}
}

func TestLeagueCreateJoinBoard(t *testing.T) {
	s, ctx := newTestStore(t)

	lg, err := s.CreateLeague(ctx, "owner", "Ben", "Dostlar")
	if err != nil {
		t.Fatal(err)
	}
	if lg.Code == "" || lg.ID == 0 || lg.Name != "Dostlar" {
		t.Fatalf("gecersiz lig: %+v", lg)
	}

	if _, err := s.JoinLeague(ctx, "friend", "Mehmet", lg.Code); err != nil {
		t.Fatal(err)
	}
	if _, err := s.JoinLeague(ctx, "x", "X", "BULBI-ZZZZ"); !errors.Is(err, ErrLeagueNotFound) {
		t.Errorf("gecersiz kod ErrLeagueNotFound vermeli, %v", err)
	}

	// Haftalik 'daily' skorlar (lig disindaki cihaz sayilmamali).
	_ = s.SubmitScore(ctx, "owner", "Ben", "daily", 10, 5)
	_ = s.SubmitScore(ctx, "friend", "Mehmet", "daily", 10, 8)
	_ = s.SubmitScore(ctx, "stranger", "Yabanci", "daily", 10, 99)

	board, err := s.LeagueBoard(ctx, lg.Code, 4, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(board) != 2 {
		t.Fatalf("2 uye bekleniyordu, %d", len(board))
	}
	if board[0].Name != "Mehmet" || board[0].Score != 8 || board[0].Rank != 1 {
		t.Errorf("ilk sira Mehmet 8 olmali: %+v", board[0])
	}

	mine, err := s.MyLeagues(ctx, "friend")
	if err != nil {
		t.Fatal(err)
	}
	if len(mine) != 1 || mine[0].Code != lg.Code {
		t.Errorf("friend tam 1 ligde olmali: %+v", mine)
	}
}

func TestStreakReminderTokens(t *testing.T) {
	s, ctx := newTestStore(t)
	_ = s.SaveDevice(ctx, "a", "tokA", "ios") // dün oynadı, bugün yok -> hatırlat
	_ = s.SaveDevice(ctx, "b", "tokB", "ios") // dün+bugün -> yok
	_ = s.SaveDevice(ctx, "c", "tokC", "ios") // sadece bugün -> yok
	_ = s.SaveDevice(ctx, "d", "tokD", "ios") // hiç -> yok

	_ = s.SubmitScore(ctx, "a", "A", "daily", 9, 3)
	_ = s.SubmitScore(ctx, "b", "B", "daily", 9, 2)
	_ = s.SubmitScore(ctx, "b", "B", "daily", 10, 4)
	_ = s.SubmitScore(ctx, "c", "C", "daily", 10, 1)

	toks, err := s.StreakReminderTokens(ctx, 9, 10) // dün=9, bugün=10
	if err != nil {
		t.Fatal(err)
	}
	if len(toks) != 1 || toks[0] != "tokA" {
		t.Errorf("yalnız tokA beklenir: %v", toks)
	}
}

func TestDeviceTokens(t *testing.T) {
	s, ctx := newTestStore(t)
	if err := s.SaveDevice(ctx, "dev1", "tokenA", "android"); err != nil {
		t.Fatal(err)
	}
	// Ayni cihaz token'i guncellenmeli (yeni kayit eklenmemeli).
	if err := s.SaveDevice(ctx, "dev1", "tokenB", "android"); err != nil {
		t.Fatal(err)
	}
	tokens, err := s.AllTokens(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(tokens) != 1 || tokens[0] != "tokenB" {
		t.Errorf("tek guncel token bekleniyordu, %v", tokens)
	}
}
