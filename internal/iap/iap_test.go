package iap

import (
	"errors"
	"testing"
)

func TestNewLoadsAppleRoot(t *testing.T) {
	v, err := New("com.atakanzgn.bulbi", nil, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if v.appleRoots == nil {
		t.Fatal("Apple Root CA G3 yuklenmedi")
	}
}

func TestVerifyAppleJWSRejectsMalformed(t *testing.T) {
	v, _ := New("", nil, "")
	for _, bad := range []string{"", "a", "a.b", "not.a.jws", "x.y.z.w"} {
		if _, err := verifyAppleJWS(bad, v.appleRoots); !errors.Is(err, ErrInvalid) {
			t.Errorf("%q icin ErrInvalid bekleniyordu, %v", bad, err)
		}
	}
}
