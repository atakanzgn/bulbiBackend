package content

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.json")
	data := `{"version":3,"words":["ARABA","KİTAP"],"questions":[{"q":"x","options":["a","b"],"answer":1}]}`
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := NewStore(p)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if s.Version() != 3 {
		t.Errorf("version 3 bekleniyordu, %d bulundu", s.Version())
	}
	c := s.Get()
	if len(c.Words) != 2 || c.Words[0] != "ARABA" {
		t.Errorf("kelime listesi hatali: %v", c.Words)
	}
	if len(c.Questions) != 1 || c.Questions[0].Answer != 1 {
		t.Errorf("soru hatali: %+v", c.Questions)
	}
}
