package server

import (
	"strings"
	"testing"
)

func TestPrivacyTemplateRenders(t *testing.T) {
	var b strings.Builder
	if err := privacyTmpl.Execute(&b, map[string]string{
		"Contact": "test@example.com",
		"Updated": "01.01.2026",
	}); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"Gizlilik Politikası", "test@example.com", "01.01.2026", "Cihaz kimliği",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("gizlilik sayfası %q içermeli", want)
		}
	}
}
