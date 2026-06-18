package store

import (
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestSubmitScoreKeepsBest(t *testing.T) {
	s := newTestStore(t)

	if err := s.SubmitScore("dev1", "Ali", "word", 5, 3); err != nil {
		t.Fatal(err)
	}
	// Daha dusuk skor mevcut en iyiyi degistirmemeli.
	if err := s.SubmitScore("dev1", "Ali", "word", 5, 1); err != nil {
		t.Fatal(err)
	}
	rank, score, found, err := s.MyRank("word", 5, "dev1")
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
	s := newTestStore(t)
	_ = s.SubmitScore("a", "Ayşe", "quiz", 10, 5)
	_ = s.SubmitScore("b", "Veli", "quiz", 10, 3)
	_ = s.SubmitScore("c", "Can", "quiz", 10, 4)

	top, err := s.Leaderboard("quiz", 10, 10)
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

	rank, score, found, _ := s.MyRank("quiz", 10, "b")
	if !found || score != 3 || rank != 3 {
		t.Errorf("Veli rank=3 score=3 olmali, rank=%d score=%d found=%v", rank, score, found)
	}
}

func TestDeviceTokens(t *testing.T) {
	s := newTestStore(t)
	if err := s.SaveDevice("dev1", "tokenA", "android"); err != nil {
		t.Fatal(err)
	}
	// Ayni cihaz token'i guncellenmeli (yeni kayit eklenmemeli).
	if err := s.SaveDevice("dev1", "tokenB", "android"); err != nil {
		t.Fatal(err)
	}
	tokens, err := s.AllTokens()
	if err != nil {
		t.Fatal(err)
	}
	if len(tokens) != 1 || tokens[0] != "tokenB" {
		t.Errorf("tek guncel token bekleniyordu, %v", tokens)
	}
}
