// Package content surumlu bulmaca icerigini (kelime + soru) dosyadan yukler ve
// servis eder. Icerigi buyutmek icin sunucudaki JSON'u duzenleyip "version"
// alanini artirmak yeterlidir; uygulama yeni surumu indirip onbellege alir.
package content

import (
	"encoding/json"
	"os"
	"sync"
)

// Question quiz sorusu: metin, secenekler ve dogru secenegin indeksi.
type Question struct {
	Q       string   `json:"q"`
	Options []string `json:"options"`
	Answer  int      `json:"answer"`
}

// Content bir icerik paketi.
type Content struct {
	Version   int        `json:"version"`
	Words     []string   `json:"words"`
	Questions []Question `json:"questions"`
}

// Store icerigi bellekte tutar; dosyadan yeniden yuklenebilir (eszamanli guvenli).
type Store struct {
	mu      sync.RWMutex
	path    string
	current Content
}

// NewStore verilen dosyadan icerigi yukler.
func NewStore(path string) (*Store, error) {
	s := &Store{path: path}
	if err := s.Reload(); err != nil {
		return nil, err
	}
	return s, nil
}

// Reload icerigi dosyadan tekrar okur (sunucuyu yeniden baslatmadan guncelleme).
func (s *Store) Reload() error {
	b, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	var c Content
	if err := json.Unmarshal(b, &c); err != nil {
		return err
	}
	s.mu.Lock()
	s.current = c
	s.mu.Unlock()
	return nil
}

// Get gecerli icerik paketini dondurur.
func (s *Store) Get() Content {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.current
}

// Version gecerli icerik surumunu dondurur (ucuz kontrol icin).
func (s *Store) Version() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.current.Version
}
