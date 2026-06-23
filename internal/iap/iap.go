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

// Verifier App Store + Play dogrulayicisi.
type Verifier struct {
	appleSecret    string // App Store paylasilan sirri (verifyReceipt)
	googleSA       *serviceAccount
	androidPackage string
	client         *http.Client

	mu      sync.Mutex
	gToken  string
	gExpiry time.Time
}

// New, env'den gelen yapilandirmayla bir Verifier olusturur. Bos alanlar ilgili
// platformu devre disi birakir (o platform icin ErrNotConfigured doner).
func New(appleSharedSecret string, googleSAJSON []byte, androidPackage string) (*Verifier, error) {
	v := &Verifier{
		appleSecret:    appleSharedSecret,
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

// --- Apple (legacy verifyReceipt) ---

type appleResp struct {
	Status  int `json:"status"`
	Receipt struct {
		InApp []struct {
			ProductID     string `json:"product_id"`
			TransactionID string `json:"transaction_id"`
		} `json:"in_app"`
	} `json:"receipt"`
}

// VerifyApple base64 makbuzu Apple ile dogrular ve [productID] icin islem
// kimligini doner. Prod uctan 21007 gelirse sandbox'a duser.
func (v *Verifier) VerifyApple(ctx context.Context, receipt, productID string) (Result, error) {
	if v.appleSecret == "" {
		return Result{}, ErrNotConfigured
	}
	res, err := v.appleHit(ctx, "https://buy.itunes.apple.com/verifyReceipt", receipt)
	if err != nil {
		return Result{}, err
	}
	if res.Status == 21007 { // sandbox makbuzu prod'a gonderilmis
		res, err = v.appleHit(ctx, "https://sandbox.itunes.apple.com/verifyReceipt", receipt)
		if err != nil {
			return Result{}, err
		}
	}
	if res.Status != 0 {
		return Result{}, ErrInvalid
	}
	var txn string
	for _, it := range res.Receipt.InApp {
		if it.ProductID == productID && it.TransactionID != "" {
			txn = it.TransactionID
		}
	}
	if txn == "" {
		return Result{}, ErrInvalid
	}
	return Result{TransactionID: txn}, nil
}

func (v *Verifier) appleHit(ctx context.Context, endpoint, receipt string) (appleResp, error) {
	body, _ := json.Marshal(map[string]any{
		"receipt-data":             receipt,
		"password":                 v.appleSecret,
		"exclude-old-transactions": true,
	})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := v.client.Do(req)
	if err != nil {
		return appleResp{}, err
	}
	defer resp.Body.Close()
	var out appleResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return appleResp{}, err
	}
	return out, nil
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
		return Result{}, ErrInvalid
	}
	var pr struct {
		PurchaseState *int   `json:"purchaseState"`
		OrderID       string `json:"orderId"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		return Result{}, err
	}
	if pr.PurchaseState == nil || *pr.PurchaseState != 0 { // 0 = Purchased
		return Result{}, ErrInvalid
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
