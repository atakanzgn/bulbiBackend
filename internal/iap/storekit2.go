package iap

import (
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// appleRootCAG3PEM, StoreKit 2 imzalı işlemlerin (JWS) güven köküdür.
// SHA-256 parmak izi: 63:34:3A:BF:...:91:79 (Apple'ın yayınladığı Apple Root CA - G3).
const appleRootCAG3PEM = `-----BEGIN CERTIFICATE-----
MIICQzCCAcmgAwIBAgIILcX8iNLFS5UwCgYIKoZIzj0EAwMwZzEbMBkGA1UEAwwS
QXBwbGUgUm9vdCBDQSAtIEczMSYwJAYDVQQLDB1BcHBsZSBDZXJ0aWZpY2F0aW9u
IEF1dGhvcml0eTETMBEGA1UECgwKQXBwbGUgSW5jLjELMAkGA1UEBhMCVVMwHhcN
MTQwNDMwMTgxOTA2WhcNMzkwNDMwMTgxOTA2WjBnMRswGQYDVQQDDBJBcHBsZSBS
b290IENBIC0gRzMxJjAkBgNVBAsMHUFwcGxlIENlcnRpZmljYXRpb24gQXV0aG9y
aXR5MRMwEQYDVQQKDApBcHBsZSBJbmMuMQswCQYDVQQGEwJVUzB2MBAGByqGSM49
AgEGBSuBBAAiA2IABJjpLz1AcqTtkyJygRMc3RCV8cWjTnHcFBbZDuWmBSp3ZHtf
TjjTuxxEtX/1H7YyYl3J6YRbTzBPEVoA/VhYDKX1DyxNB0cTddqXl5dvMVztK517
IDvYuVTZXpmkOlEKMaNCMEAwHQYDVR0OBBYEFLuw3qFYM4iapIqZ3r6966/ayySr
MA8GA1UdEwEB/wQFMAMBAf8wDgYDVR0PAQH/BAQDAgEGMAoGCCqGSM49BAMDA2gA
MGUCMQCD6cHEFl4aXTQY2e3v9GwOAEZLuN+yRhHFD/3meoyhpmvOwgPUnPWTxnS4
at+qIxUCMG1mihDK1A3UT82NQz60imOlM27jbdoXt2QfyFMm+YhidDkLF1vLUagM
6BgD56KyKA==
-----END CERTIFICATE-----`

// jwsTransactionPayload, SK2 imzalı işlem yükünün ilgili alanları.
type jwsTransactionPayload struct {
	TransactionID string `json:"transactionId"`
	ProductID     string `json:"productId"`
	BundleID      string `json:"bundleId"`
}

// verifyAppleJWS bir StoreKit 2 JWS imzasını DOĞRULAR: x5c sertifika zincirini
// Apple Root CA G3'e kadar denetler ve ES256 imzasını leaf anahtarıyla kontrol
// eder. Geçerliyse imzalı yükü döndürür; aksi halde ErrInvalid.
func verifyAppleJWS(jws string, roots *x509.CertPool) (jwsTransactionPayload, error) {
	parts := strings.Split(jws, ".")
	if len(parts) != 3 {
		return jwsTransactionPayload{}, fmt.Errorf("%w (jws bicimi)", ErrInvalid)
	}

	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return jwsTransactionPayload{}, fmt.Errorf("%w (header b64)", ErrInvalid)
	}
	var hdr struct {
		Alg string   `json:"alg"`
		X5c []string `json:"x5c"`
	}
	if err := json.Unmarshal(headerJSON, &hdr); err != nil {
		return jwsTransactionPayload{}, fmt.Errorf("%w (header json)", ErrInvalid)
	}
	if hdr.Alg != "ES256" || len(hdr.X5c) < 2 {
		return jwsTransactionPayload{}, fmt.Errorf("%w (alg/x5c)", ErrInvalid)
	}

	// x5c: [leaf, intermediate, (root)] — standard base64 DER.
	certs := make([]*x509.Certificate, 0, len(hdr.X5c))
	for _, c := range hdr.X5c {
		der, err := base64.StdEncoding.DecodeString(c)
		if err != nil {
			return jwsTransactionPayload{}, fmt.Errorf("%w (x5c b64)", ErrInvalid)
		}
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			return jwsTransactionPayload{}, fmt.Errorf("%w (x5c parse)", ErrInvalid)
		}
		certs = append(certs, cert)
	}
	leaf := certs[0]

	// Sertifika zinciri: leaf -> ara -> Apple Root CA G3 (pinli).
	inter := x509.NewCertPool()
	for _, c := range certs[1:] {
		inter.AddCert(c)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:         roots,
		Intermediates: inter,
		CurrentTime:   time.Now(),
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	}); err != nil {
		return jwsTransactionPayload{}, fmt.Errorf("%w (zincir: %v)", ErrInvalid, err)
	}

	// ES256 imza: header.payload üzerinde, leaf anahtarıyla.
	pub, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok {
		return jwsTransactionPayload{}, fmt.Errorf("%w (anahtar ES256 degil)", ErrInvalid)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(sig) != 64 {
		return jwsTransactionPayload{}, fmt.Errorf("%w (imza bicimi)", ErrInvalid)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	if !ecdsa.Verify(pub, digest[:], r, s) {
		return jwsTransactionPayload{}, fmt.Errorf("%w (imza)", ErrInvalid)
	}

	payloadJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return jwsTransactionPayload{}, fmt.Errorf("%w (payload b64)", ErrInvalid)
	}
	var p jwsTransactionPayload
	if err := json.Unmarshal(payloadJSON, &p); err != nil {
		return jwsTransactionPayload{}, fmt.Errorf("%w (payload json)", ErrInvalid)
	}
	return p, nil
}
