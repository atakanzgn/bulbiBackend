package moderation

import "testing"

func TestIsProfaneCatchesEvasions(t *testing.T) {
	profane := []string{
		"orospu", "Or0spu", "o r o s p u", "s1ktir", "siktir lan",
		"amına koyim", "FUCK", "f u c k", "şerefsiz", "pezevenk", " amk ",
		"siiiktir",
	}
	for _, s := range profane {
		if !IsProfane(s) {
			t.Errorf("kufur sayilmaliydi: %q", s)
		}
	}
}

func TestIsProfaneNoFalsePositives(t *testing.T) {
	clean := []string{
		"Atakan", "Mehmet", "Ayşe", "klasik", "psikoloji", "kapıcı", "eksik",
		"Bayraktar", "Selin", "Ali Veli", "Oyuncu123", "Anonim", "götürmek",
		"tamam", "İstanbul",
	}
	for _, s := range clean {
		if IsProfane(s) {
			t.Errorf("temiz sayilmaliydi: %q", s)
		}
	}
}
