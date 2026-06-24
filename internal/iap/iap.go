// Package iap, App Store ve Google Play satin alma makbuzlarini SUNUCU TARAFINDA
// dogrular. Amac: istemcinin "satin aldim" demesine guvenmeden, makbuzu gercek
// magaza ile dogrulayip ancak ondan sonra coin vermek. Yapilandirma yoksa
// (paylasilan sir / servis hesabi verilmemisse) dogrulama REDDEDILIR — yani
// guvenli varsayilan: dogrulanamayan satin alma coin KAZANDIRMAZ.
package iap

import (
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
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

var (
	// ErrNotConfigured ilgili platform icin dogrulama yapilandirilmamis.
	ErrNotConfigured = errors.New("iap: platform dogrulamasi yapilandirilmamis")
	// ErrInvalid makbuz gecersiz / satin alma dogrulanamadi.
	ErrInvalid = errors.New("iap: makbuz gecersiz")
)

// Result dogrulama sonucu. TransactionID idempotency (tekrar kullanim) anahtari.
type Result struct {
	TransactionID string
}

type serviceAccount struct {
	ClientEmail string `json:"client_email"`
	PrivateKey  string `json:"private_key"`
	TokenURI    string `json:"token_uri"`
}

// Verifier App Store (StoreKit 2) + Play dogrulayicisi.
type Verifier struct {
	iosBundleID    string         // beklenen iOS bundle id (bos = kontrol etme)
	appleRoots     *x509.CertPool // Apple Root CA G3 (JWS guven koku)
	googleSA       *serviceAccount
	androidPackage string
	client         *http.Client

	mu      sync.Mutex
	gToken  string
	gExpiry time.Time
}

// New bir Verifier olusturur. iOS StoreKit 2 dogrulamasi her zaman aciktir
// (gomulu Apple Root CA G3 ile, ag gerektirmez). Android, [googleSAJSON]
// verilirse etkinlesir.
func New(iosBundleID string, googleSAJSON []byte, androidPackage string) (*Verifier, error) {
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(appleRootCAG3PEM)) {
		return nil, errors.New("iap: Apple Root CA G3 yuklenemedi")
	}
	v := &Verifier{
		iosBundleID:    iosBundleID,
		appleRoots:     roots,
		androidPackage: androidPackage,
		client:         &http.Client{Timeout: 15 * time.Second},
	}
	if len(googleSAJSON) > 0 {
		var sa serviceAccount
		if err := json.Unmarshal(googleSAJSON, &sa); err != nil {
			return nil, err
		}
		if sa.ClientEmail == "" || sa.PrivateKey == "" {
			return nil, errors.New("iap: eksik Google servis hesabi alanlari")
		}
		v.googleSA = &sa
	}
	return v, nil
}

// --- Apple (StoreKit 2 JWS, cevrimdisi imza dogrulamasi) ---

// VerifyApple istemciden gelen StoreKit 2 imzali islemini (JWS) dogrular ve
// islem kimligini doner. Apple ile ag gorusmesi gerekmez: imza + sertifika
// zinciri Apple Root CA G3'e kadar denetlenir. ([ctx] kullanilmaz; imza Google
// ile tutarli olsun diye korunur.)
func (v *Verifier) VerifyApple(ctx context.Context, jws, productID string) (Result, error) {
	_ = ctx
	p, err := verifyAppleJWS(jws, v.appleRoots)
	if err != nil {
		return Result{}, err
	}
	if v.iosBundleID != "" && p.BundleID != v.iosBundleID {
		return Result{}, fmt.Errorf("%w (bundle uyusmuyor: %s)", ErrInvalid, p.BundleID)
	}
	if p.ProductID != productID {
		return Result{}, fmt.Errorf("%w (urun uyusmuyor: %s)", ErrInvalid, p.ProductID)
	}
	if p.TransactionID == "" {
		return Result{}, fmt.Errorf("%w (islem kimligi yok)", ErrInvalid)
	}
	return Result{TransactionID: p.TransactionID}, nil
}

// --- Google Play (Developer API) ---

// VerifyGoogle purchaseToken'i Play Developer API ile dogrular. purchaseState=0
// (satin alindi) degilse gecersiz sayar.
func (v *Verifier) VerifyGoogle(ctx context.Context, productID, token string) (Result, error) {
	if v.googleSA == nil || v.androidPackage == "" {
		return Result{}, ErrNotConfigured
	}
	at, err := v.googleToken(ctx)
	if err != nil {
		return Result{}, err
	}
	endpoint := fmt.Sprintf(
		"https://androidpublisher.googleapis.com/androidpublisher/v3/applications/%s/purchases/products/%s/tokens/%s",
		url.PathEscape(v.androidPackage), url.PathEscape(productID), url.PathEscape(token))
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	req.Header.Set("Authorization", "Bearer "+at)
	resp, err := v.client.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Result{}, fmt.Errorf("%w (play http %d)", ErrInvalid, resp.StatusCode)
	}
	var pr struct {
		PurchaseState *int   `json:"purchaseState"`
		OrderID       string `json:"orderId"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		return Result{}, err
	}
	if pr.PurchaseState == nil || *pr.PurchaseState != 0 { // 0 = Purchased
		return Result{}, fmt.Errorf("%w (play purchaseState=%v)", ErrInvalid, pr.PurchaseState)
	}
	txn := pr.OrderID
	if txn == "" {
		txn = token // emniyet: orderId yoksa token'i idempotency anahtari yap
	}
	return Result{TransactionID: txn}, nil
}

// googleToken androidpublisher kapsamli OAuth2 erisim token'i dondurur (onbellekli).
func (v *Verifier) googleToken(ctx context.Context) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.gToken != "" && time.Now().Before(v.gExpiry) {
		return v.gToken, nil
	}
	signed, err := v.signJWT()
	if err != nil {
		return "", err
	}
	form := url.Values{}
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:jwt-bearer")
	form.Set("assertion", signed)

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, v.tokenURI(), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := v.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("iap: token alinamadi %s: %s", resp.Status, string(msg))
	}
	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return "", err
	}
	v.gToken = tr.AccessToken
	v.gExpiry = time.Now().Add(time.Duration(tr.ExpiresIn-60) * time.Second)
	return v.gToken, nil
}

func (v *Verifier) signJWT() (string, error) {
	now := time.Now()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]any{
		"iss":   v.googleSA.ClientEmail,
		"scope": "https://www.googleapis.com/auth/androidpublisher",
		"aud":   v.tokenURI(),
		"iat":   now.Unix(),
		"exp":   now.Add(time.Hour).Unix(),
	})
	signingInput := header + "." + base64.RawURLEncoding.EncodeToString(claims)
	key, err := parseKey(v.googleSA.PrivateKey)
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

func (v *Verifier) tokenURI() string {
	if v.googleSA != nil && v.googleSA.TokenURI != "" {
		return v.googleSA.TokenURI
	}
	return "https://oauth2.googleapis.com/token"
}

func parseKey(pemStr string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, errors.New("iap: private_key PEM cozulemedi")
	}
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		rk, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("iap: PKCS8 anahtari RSA degil")
		}
		return rk, nil
	}
	return x509.ParsePKCS1PrivateKey(block.Bytes)
}
