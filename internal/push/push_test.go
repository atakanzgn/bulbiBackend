package push

import "testing"

func TestTokenIsDead(t *testing.T) {
	dead := []string{
		`{"error":{"code":404,"status":"NOT_FOUND","details":[{"errorCode":"UNREGISTERED"}]}}`,
		`{"error":{"code":400,"status":"INVALID_ARGUMENT","details":[{"errorCode":"INVALID_ARGUMENT"}]}}`,
		`{"error":{"status":"NOT_FOUND"}}`,
	}
	for _, b := range dead {
		if !tokenIsDead([]byte(b)) {
			t.Errorf("olu sayilmaliydi: %s", b)
		}
	}

	alive := []string{
		`{"error":{"code":500,"status":"INTERNAL","details":[{"errorCode":"INTERNAL"}]}}`,
		`{"error":{"code":401,"status":"UNAUTHENTICATED"}}`,
		`{"error":{"code":429,"status":"QUOTA_EXCEEDED"}}`,
		`not json`,
		``,
	}
	for _, b := range alive {
		if tokenIsDead([]byte(b)) {
			t.Errorf("olu sayilmamaliydi: %s", b)
		}
	}
}
