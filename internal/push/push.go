// Package push FCM HTTP v1 ile push bildirim gonderir. Servis hesabi
// JWT'sini stdlib ile imzalar (harici bagimlilik yok). Kimlik bilgisi
// verilmezse devre disi kalir (Sender nil).
package push

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type serviceAccount struct {
	ClientEmail string `json:"client_email"`
	PrivateKey  string `json:"private_key"`
	TokenURI    string `json:"token_uri"`
	ProjectID   string `json:"project_id"`
}

// Sender FCM v1 gonderici.
type Sender struct {
	sa        serviceAccount
	projectID string
	client    *http.Client

	mu          sync.Mutex
	accessToken string
	expiry      time.Time
}

// NewSender servis hesabi JSON'undan bir gonderici olusturur. [credentialsJSON]
// bos ise (nil, nil) doner — push devre disi demektir.
func NewSender(credentialsJSON []byte, projectID string) (*Sender, error) {
	if len(credentialsJSON) == 0 {
		return nil, nil
	}
	var sa serviceAccount
	if err := json.Unmarshal(credentialsJSON, &sa); err != nil {
		return nil, err
	}
	if projectID == "" {
		projectID = sa.ProjectID
	}
	if sa.ClientEmail == "" || sa.PrivateKey == "" || projectID == "" {
		return nil, errors.New("eksik servis hesabi alanlari")
	}
	return &Sender{
		sa:        sa,
		projectID: projectID,
		client:    &http.Client{Timeout: 15 * time.Second},
	}, nil
}

// Send tek bir cihaz token'ina bildirim gonderir ([image] opsiyonel gorsel URL).
func (s *Sender) Send(ctx context.Context, token, title, body, image string) error {
	at, err := s.token(ctx)
	if err != nil {
		return err
	}
	notif := map[string]any{"title": title, "body": body}
	message := map[string]any{
		"token":        token,
		"notification": notif,
	}
	if image != "" {
		notif["image"] = image // Android + platformlar arasi kisayol
		// iOS: gorselin gorunmesi icin mutable-content=1 (uygulamadaki
		// Notification Service Extension'i tetikler) + fcm_options.image.
		message["apns"] = map[string]any{
			"payload":     map[string]any{"aps": map[string]any{"mutable-content": 1}},
			"fcm_options": map[string]any{"image": image},
		}
	}
	payload := map[string]any{"message": message}
	b, _ := json.Marshal(payload)
	endpoint := fmt.Sprintf("https://fcm.googleapis.com/v1/projects/%s/messages:send", s.projectID)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+at)
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("fcm gonderim hatasi %s: %s", resp.Status, string(msg))
	}
	return nil
}

// SendToAll tum token'lara ayni bildirimi eszamanli gonderir; basarili sayisini doner.
func (s *Sender) SendToAll(ctx context.Context, tokens []string, title, body, image string) int {
	const workers = 16
	jobs := make(chan string)
	var wg sync.WaitGroup
	var mu sync.Mutex
	sent := 0
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for tok := range jobs {
				if err := s.Send(ctx, tok, title, body, image); err == nil {
					mu.Lock()
					sent++
					mu.Unlock()
				}
			}
		}()
	}
	for _, t := range tokens {
		jobs <- t
	}
	close(jobs)
	wg.Wait()
	return sent
}

// token gecerli bir OAuth2 erisim token'i dondurur (onbellekli).
func (s *Sender) token(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.accessToken != "" && time.Now().Before(s.expiry) {
		return s.accessToken, nil
	}

	signed, err := s.signJWT()
	if err != nil {
		return "", err
	}
	form := url.Values{}
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:jwt-bearer")
	form.Set("assertion", signed)

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, s.tokenURI(), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("token alinamadi %s: %s", resp.Status, string(msg))
	}
	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return "", err
	}
	s.accessToken = tr.AccessToken
	s.expiry = time.Now().Add(time.Duration(tr.ExpiresIn-60) * time.Second)
	return s.accessToken, nil
}

func (s *Sender) signJWT() (string, error) {
	now := time.Now()
	header := b64(`{"alg":"RS256","typ":"JWT"}`)
	claims, _ := json.Marshal(map[string]any{
		"iss":   s.sa.ClientEmail,
		"scope": "https://www.googleapis.com/auth/firebase.messaging",
		"aud":   s.tokenURI(),
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	})
	signingInput := header + "." + base64.RawURLEncoding.EncodeToString(claims)

	key, err := parseKey(s.sa.PrivateKey)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, h[:])
	if err != nil {
		return "", err
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func (s *Sender) tokenURI() string {
	if s.sa.TokenURI != "" {
		return s.sa.TokenURI
	}
	return "https://oauth2.googleapis.com/token"
}

func b64(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

func parseKey(pemStr string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, errors.New("private_key PEM cozulemedi")
	}
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		rk, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("PKCS8 anahtari RSA degil")
		}
		return rk, nil
	}
	return x509.ParsePKCS1PrivateKey(block.Bytes)
}

// Broadcaster her gun belirlenen saatte kayitli tum cihazlara bildirim yollar.
type Broadcaster struct {
	Sender *Sender
	Tokens func(context.Context) ([]string, error)
	Hour   int
	Title  string
	Body   string
}

// Run zamanlayiciyi baslatir; ctx iptal edilene kadar calisir.
func (b *Broadcaster) Run(ctx context.Context) {
	for {
		next := nextAt(time.Now(), b.Hour)
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			b.broadcast(ctx)
		}
	}
}

func (b *Broadcaster) broadcast(ctx context.Context) {
	if b.Sender == nil {
		return
	}
	tokens, err := b.Tokens(ctx)
	if err != nil {
		log.Printf("push: token listesi alinamadi: %v", err)
		return
	}
	sent := b.Sender.SendToAll(ctx, tokens, b.Title, b.Body, "")
	log.Printf("push: %d/%d bildirim gonderildi", sent, len(tokens))
}

func nextAt(now time.Time, hour int) time.Time {
	n := time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, now.Location())
	if !n.After(now) {
		n = n.Add(24 * time.Hour)
	}
	return n
}
