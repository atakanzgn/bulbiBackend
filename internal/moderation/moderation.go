// Package moderation kullanici adlarini kufur/hakaret icin denetler. Once
// normalize eder (TR->ASCII, kucuk harf, leetspeak coz, harf-disi sil, ardisik
// tekrari darlat) sonra kara liste koklerini substring olarak arar. Boylece
// "k.ü.f.ü.r", "kufuuur", "kuf0r" gibi atlatmalar da yakalanir.
package moderation

import "strings"

// Kara liste: NORMALIZE edilmis (ASCII, kucuk, tekrarsiz) kufur/hakaret kokleri.
// Substring arandigi icin ekler/varyantlar da yakalanir. Yanlis-pozitifi azaltmak
// icin kisa/yaygin hecelerden kacinilir. Genisletilebilir.
var blocklist = []string{
	// TR (hepsi normalize edilmis: ASCII, kucuk, tekrarsiz)
	"orospu", "orosbu", "amcik", "amina", "aminako", "amk",
	"yarak", "yaragi", "yaragim", "siktir", "sikeyim", "sikerim", "sikim",
	"sikis", "sikici", "siktin", "gotveren", "gotlek", "pezevenk", "pezeven",
	"ibne", "serefsiz", "pust", "kahpe", "kaltak", "gavat", "godos",
	"surtuk", "yavsak",
	// EN
	"fuck", "motherfuck", "shit", "bitch", "asshole", "cunt", "nigger",
	"faggot", "bastard", "whore", "slut",
}

var trFold = strings.NewReplacer(
	"ı", "i", "İ", "i", "ş", "s", "Ş", "s", "ç", "c", "Ç", "c",
	"ğ", "g", "Ğ", "g", "ü", "u", "Ü", "u", "ö", "o", "Ö", "o",
	"I", "i",
)

var leet = strings.NewReplacer(
	"0", "o", "1", "i", "3", "e", "4", "a", "5", "s", "7", "t",
	"@", "a", "$", "s", "8", "b",
)

// normalize: TR harflerini ASCII'ye katlar, kucuk yapar, leetspeak cozer,
// yalniz a-z birakir ve ardisik tekrarlari tek harfe indirir.
func normalize(s string) string {
	s = trFold.Replace(s)
	s = strings.ToLower(s)
	s = leet.Replace(s)
	var b strings.Builder
	var last rune
	for _, r := range s {
		if r < 'a' || r > 'z' {
			continue // harf disi (bosluk/nokta...) atlanir -> atlatma engellenir
		}
		if r != last {
			b.WriteRune(r)
			last = r
		}
	}
	return b.String()
}

// IsProfane verilen ad kufur/hakaret iceriyorsa true doner.
func IsProfane(name string) bool {
	n := normalize(name)
	if n == "" {
		return false
	}
	for _, w := range blocklist {
		if strings.Contains(n, w) {
			return true
		}
	}
	return false
}
