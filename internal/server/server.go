// Package server HTTP API'sini tanimlar: icerik servisi, liderlik tablosu ve
// cihaz token kaydi.
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"html/template"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"bulbi-backend/internal/cache"
	"bulbi-backend/internal/store"
)

type Server struct {
	Store           *store.Store
	Cache           *cache.Cache
	AdminPassword   string // bos ise admin paneli kapali
	RateLimitPerMin int    // 0 -> 120
	MinAppBuild     int    // istemcinin gerektirdigi minimum build numarasi

	mem *memStore
}

const cacheKeyBundle = "content:bundle"

var validPuzzles = map[string]bool{"word": true, "number": true, "quiz": true}

// Routes tum uclari baglayip middleware ile sarar.
func (s *Server) Routes() http.Handler {
	if s.mem == nil {
		s.mem = newMemStore()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /api/v1/content", s.getContent)
	mux.HandleFunc("GET /api/v1/content/version", s.getVersion)
	mux.HandleFunc("GET /api/v1/app", s.getAppInfo)
	mux.HandleFunc("POST /api/v1/scores", s.postScore)
	mux.HandleFunc("GET /api/v1/leaderboard", s.getLeaderboard)
	mux.HandleFunc("POST /api/v1/devices", s.postDevice)

	// Admin paneli (Basic Auth, ADMIN_PASSWORD)
	mux.HandleFunc("GET /admin", s.adminGuard(s.adminHome))
	mux.HandleFunc("POST /admin/words", s.adminGuard(s.adminAddWord))
	mux.HandleFunc("POST /admin/words/delete", s.adminGuard(s.adminDeleteWord))
	mux.HandleFunc("POST /admin/questions", s.adminGuard(s.adminAddQuestion))
	mux.HandleFunc("POST /admin/questions/delete", s.adminGuard(s.adminDeleteQuestion))

	return cors(s.rateLimit(logging(mux)))
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) getContent(w http.ResponseWriter, r *http.Request) {
	// Once Redis cache (gun boyu ayni icerik -> DB sorgusu yok).
	if cached, ok := s.Cache.GetString(r.Context(), cacheKeyBundle); ok {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(cached))
		return
	}
	body, err := s.buildContentJSON(r.Context())
	if err != nil {
		log.Printf("icerik hatasi: %v", err)
		writeError(w, http.StatusInternalServerError, "icerik alinamadi")
		return
	}
	s.Cache.SetString(r.Context(), cacheKeyBundle, string(body), 25*time.Hour)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(body)
}

func (s *Server) buildContentJSON(ctx context.Context) ([]byte, error) {
	version, words, questions, err := s.Store.ContentBundle(ctx)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{
		"version":   version,
		"words":     words,
		"questions": questions,
	})
}

// RefreshContentCache cache'i DB'den yeniden olusturur (gunluk TR 00:00 isitma).
func (s *Server) RefreshContentCache(ctx context.Context) {
	body, err := s.buildContentJSON(ctx)
	if err != nil {
		log.Printf("cache isitma hatasi: %v", err)
		return
	}
	s.Cache.SetString(ctx, cacheKeyBundle, string(body), 25*time.Hour)
}

func (s *Server) getVersion(w http.ResponseWriter, r *http.Request) {
	v, err := s.Store.ContentVersion(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "surum alinamadi")
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"version": v})
}

// getAppInfo splash ekranindaki surum/baglanti kontrolu icin.
func (s *Server) getAppInfo(w http.ResponseWriter, r *http.Request) {
	v, _ := s.Store.ContentVersion(r.Context())
	writeJSON(w, http.StatusOK, map[string]int{
		"minBuild":       s.MinAppBuild,
		"contentVersion": v,
	})
}

type scoreRequest struct {
	DeviceID string `json:"deviceId"`
	Name     string `json:"name"`
	Puzzle   string `json:"puzzle"`
	Day      int    `json:"day"`
	Score    int    `json:"score"`
}

func (s *Server) postScore(w http.ResponseWriter, r *http.Request) {
	var req scoreRequest
	if !decode(w, r, &req) {
		return
	}
	if req.DeviceID == "" || !validPuzzles[req.Puzzle] || req.Day <= 0 || req.Score < 0 {
		writeError(w, http.StatusBadRequest, "gecersiz istek")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = "Anonim"
	}
	if len(name) > 24 {
		name = name[:24]
	}
	if err := s.Store.SubmitScore(r.Context(), req.DeviceID, name, req.Puzzle, req.Day, req.Score); err != nil {
		log.Printf("skor kaydi hatasi: %v", err)
		writeError(w, http.StatusInternalServerError, "skor kaydedilemedi")
		return
	}
	rank, score, _, err := s.Store.MyRank(r.Context(), req.Puzzle, req.Day, req.DeviceID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "siralama alinamadi")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rank": rank, "score": score})
}

func (s *Server) getLeaderboard(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	puzzle := q.Get("puzzle")
	if !validPuzzles[puzzle] {
		writeError(w, http.StatusBadRequest, "gecersiz puzzle")
		return
	}
	day, err := strconv.Atoi(q.Get("day"))
	if err != nil || day <= 0 {
		writeError(w, http.StatusBadRequest, "gecersiz day")
		return
	}
	limit := 50
	if l, err := strconv.Atoi(q.Get("limit")); err == nil && l > 0 && l <= 200 {
		limit = l
	}

	top, err := s.Store.Leaderboard(r.Context(), puzzle, day, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "liderlik alinamadi")
		return
	}
	resp := map[string]any{"top": top}
	if deviceID := q.Get("deviceId"); deviceID != "" {
		rank, score, found, err := s.Store.MyRank(r.Context(), puzzle, day, deviceID)
		if err == nil && found {
			resp["me"] = map[string]int{"rank": rank, "score": score}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

type deviceRequest struct {
	DeviceID string `json:"deviceId"`
	Token    string `json:"token"`
	Platform string `json:"platform"`
}

func (s *Server) postDevice(w http.ResponseWriter, r *http.Request) {
	var req deviceRequest
	if !decode(w, r, &req) {
		return
	}
	if req.DeviceID == "" || req.Token == "" {
		writeError(w, http.StatusBadRequest, "deviceId ve token gerekli")
		return
	}
	if err := s.Store.SaveDevice(r.Context(), req.DeviceID, req.Token, req.Platform); err != nil {
		writeError(w, http.StatusInternalServerError, "cihaz kaydedilemedi")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// --- yardimcilar ---

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "gecersiz JSON")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("panik: %v", rec)
				writeError(w, http.StatusInternalServerError, "sunucu hatasi")
			}
		}()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start))
	})
}

// --- Gercek istemci IP (Cloudflare turuncu bulut + NPM arkasinda) ---

func clientIP(r *http.Request) string {
	if ip := r.Header.Get("CF-Connecting-IP"); ip != "" {
		return strings.TrimSpace(ip)
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// --- IP basina istek limiti (Redis; yoksa in-memory) ---

func (s *Server) rateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") &&
			!s.allowRequest(r.Context(), clientIP(r)) {
			w.Header().Set("Retry-After", "60")
			writeError(w, http.StatusTooManyRequests, "cok fazla istek, biraz bekleyin")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) allowRequest(ctx context.Context, ip string) bool {
	limit := s.RateLimitPerMin
	if limit <= 0 {
		limit = 120
	}
	if n, ok := s.Cache.Incr(ctx, "rl:"+ip, time.Minute); ok {
		return n <= int64(limit)
	}
	return s.mem.allowRate(ip, limit, time.Minute)
}

// --- Admin brute-force korumasi ---

const adminMaxFails = 5

func (s *Server) adminAllowed(ctx context.Context, ip string) bool {
	if n, ok := s.Cache.GetInt(ctx, "af:"+ip); ok {
		return n < adminMaxFails
	}
	return s.mem.failCount(ip) < adminMaxFails
}

func (s *Server) adminRecordFail(ctx context.Context, ip string) {
	if _, ok := s.Cache.Incr(ctx, "af:"+ip, 15*time.Minute); ok {
		return
	}
	s.mem.recordFail(ip, 15*time.Minute)
}

func (s *Server) adminResetFail(ctx context.Context, ip string) {
	s.Cache.Del(ctx, "af:"+ip)
	s.mem.resetFail(ip)
}

// --- Redis yoksa in-memory yedek (tek instance icin yeterli) ---

type memStore struct {
	mu    sync.Mutex
	rate  map[string]*memWindow
	fails map[string]*memWindow
}

type memWindow struct {
	count   int
	resetAt time.Time
}

func newMemStore() *memStore {
	return &memStore{
		rate:  map[string]*memWindow{},
		fails: map[string]*memWindow{},
	}
}

func (m *memStore) allowRate(ip string, limit int, per time.Duration) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	if w := m.rate[ip]; w != nil && now.Before(w.resetAt) {
		w.count++
		return w.count <= limit
	}
	m.rate[ip] = &memWindow{count: 1, resetAt: now.Add(per)}
	return true
}

func (m *memStore) failCount(ip string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	w := m.fails[ip]
	if w == nil || time.Now().After(w.resetAt) {
		return 0
	}
	return w.count
}

func (m *memStore) recordFail(ip string, per time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	if w := m.fails[ip]; w != nil && now.Before(w.resetAt) {
		w.count++
		return
	}
	m.fails[ip] = &memWindow{count: 1, resetAt: now.Add(per)}
}

func (m *memStore) resetFail(ip string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.fails, ip)
}

// --- Admin paneli ---

func (s *Server) adminGuard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.AdminPassword == "" {
			http.Error(w, "admin paneli kapali (ADMIN_PASSWORD tanimli degil)",
				http.StatusServiceUnavailable)
			return
		}
		ip := clientIP(r)
		if !s.adminAllowed(r.Context(), ip) {
			w.Header().Set("Retry-After", "900")
			http.Error(w, "cok fazla hatali giris denemesi; 15 dk sonra tekrar deneyin",
				http.StatusTooManyRequests)
			return
		}
		user, pass, ok := r.BasicAuth()
		valid := ok && user == "admin" &&
			subtle.ConstantTimeCompare([]byte(pass), []byte(s.AdminPassword)) == 1
		if !valid {
			s.adminRecordFail(r.Context(), ip)
			w.Header().Set("WWW-Authenticate", `Basic realm="Bulbi Admin"`)
			http.Error(w, "yetkisiz", http.StatusUnauthorized)
			return
		}
		s.adminResetFail(r.Context(), ip)
		next(w, r)
	}
}

type adminData struct {
	Version   int
	Words     []store.WordRow
	Questions []store.QuestionRow
}

func (s *Server) adminHome(w http.ResponseWriter, r *http.Request) {
	words, err := s.Store.ListWords(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	questions, err := s.Store.ListQuestions(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	version, _ := s.Store.ContentVersion(r.Context())
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := adminTmpl.Execute(w, adminData{
		Version:   version,
		Words:     words,
		Questions: questions,
	}); err != nil {
		log.Printf("admin template: %v", err)
	}
}

func (s *Server) adminAddWord(w http.ResponseWriter, r *http.Request) {
	if text := strings.TrimSpace(r.FormValue("text")); text != "" {
		if err := s.Store.AddWord(r.Context(), text); err != nil {
			log.Printf("kelime ekleme: %v", err)
		}
	}
	s.Cache.Del(r.Context(), cacheKeyBundle)
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) adminDeleteWord(w http.ResponseWriter, r *http.Request) {
	if id, err := strconv.ParseInt(r.FormValue("id"), 10, 64); err == nil {
		_ = s.Store.DeleteWord(r.Context(), id)
	}
	s.Cache.Del(r.Context(), cacheKeyBundle)
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) adminAddQuestion(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.FormValue("q"))
	options := []string{}
	for _, key := range []string{"o0", "o1", "o2", "o3"} {
		if o := strings.TrimSpace(r.FormValue(key)); o != "" {
			options = append(options, o)
		}
	}
	answer, _ := strconv.Atoi(r.FormValue("answer"))
	category := r.FormValue("category")
	difficulty := r.FormValue("difficulty")
	if q != "" && len(options) >= 2 && answer >= 0 && answer < len(options) {
		if err := s.Store.AddQuestion(r.Context(), q, options, answer, category, difficulty); err != nil {
			log.Printf("soru ekleme: %v", err)
		}
	}
	s.Cache.Del(r.Context(), cacheKeyBundle)
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

func (s *Server) adminDeleteQuestion(w http.ResponseWriter, r *http.Request) {
	if id, err := strconv.ParseInt(r.FormValue("id"), 10, 64); err == nil {
		_ = s.Store.DeleteQuestion(r.Context(), id)
	}
	s.Cache.Del(r.Context(), cacheKeyBundle)
	http.Redirect(w, r, "/admin", http.StatusSeeOther)
}

var adminTmpl = template.Must(template.New("admin").Parse(`<!doctype html>
<html lang="tr"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Bulbi Admin</title>
<style>
 body{font-family:system-ui,Arial,sans-serif;max-width:920px;margin:0 auto;padding:20px;background:#f6f6fb;color:#1a1a2e}
 h1{color:#5C5CE0;margin-bottom:0} h2{margin-top:0}
 .card{background:#fff;border-radius:12px;padding:16px;margin:14px 0;box-shadow:0 1px 4px rgba(0,0,0,.06)}
 input,select{padding:8px;border:1px solid #ccd;border-radius:8px;margin:4px 4px 4px 0;font-size:14px}
 input.full{width:100%}
 button{background:#5C5CE0;color:#fff;border:0;border-radius:8px;padding:9px 16px;cursor:pointer;font-size:14px;font-weight:600}
 button.del{background:#d64545;padding:5px 10px}
 table{width:100%;border-collapse:collapse;margin-top:8px}
 td,th{text-align:left;padding:6px 8px;border-bottom:1px solid #eee;font-size:14px;vertical-align:top}
 .muted{color:#888;font-size:13px}
</style></head><body>
<h1>Bulbi Admin</h1>
<p class="muted">İçerik sürümü: {{.Version}} · Kelime: {{len .Words}} · Soru: {{len .Questions}}</p>

<div class="card">
 <h2>Kelime ekle</h2>
 <form method="post" action="/admin/words">
  <input name="text" placeholder="BÜYÜK harfle, örn. ARABA" required>
  <button type="submit">Ekle</button>
  <div class="muted">Harf sayısı otomatik hesaplanır (oyunda 5/6/7 harfliler kullanılır).</div>
 </form>
</div>

<div class="card">
 <h2>Soru ekle</h2>
 <form method="post" action="/admin/questions">
  <input class="full" name="q" placeholder="Soru metni" required>
  <input name="o0" placeholder="Seçenek 1" required>
  <input name="o1" placeholder="Seçenek 2" required>
  <input name="o2" placeholder="Seçenek 3">
  <input name="o3" placeholder="Seçenek 4">
  <div>
   Doğru:
   <select name="answer"><option value="0">1</option><option value="1">2</option><option value="2">3</option><option value="3">4</option></select>
   Tür:
   <select name="category"><option value="genel">Genel Kültür</option><option value="spor">Spor</option><option value="bilim">Bilim</option><option value="sanat">Sanat</option></select>
   Zorluk:
   <select name="difficulty"><option value="kolay">Kolay</option><option value="orta" selected>Orta</option><option value="zor">Zor</option></select>
   <button type="submit">Ekle</button>
  </div>
 </form>
</div>

<div class="card">
 <h2>Kelimeler ({{len .Words}})</h2>
 <table><tr><th>Kelime</th><th>Harf</th><th></th></tr>
 {{range .Words}}<tr><td>{{.Text}}</td><td>{{.Length}}</td>
  <td><form method="post" action="/admin/words/delete" onsubmit="return confirm('Silinsin mi?')"><input type="hidden" name="id" value="{{.ID}}"><button class="del" type="submit">Sil</button></form></td></tr>{{end}}
 </table>
</div>

<div class="card">
 <h2>Sorular ({{len .Questions}})</h2>
 <table><tr><th>Soru</th><th>Tür / Zorluk</th><th>Doğru</th><th></th></tr>
 {{range .Questions}}<tr>
  <td>{{.Q}}<div class="muted">{{range .Options}}{{.}} &middot; {{end}}</div></td>
  <td>{{.Category}} / {{.Difficulty}}</td>
  <td>{{index .Options .Answer}}</td>
  <td><form method="post" action="/admin/questions/delete" onsubmit="return confirm('Silinsin mi?')"><input type="hidden" name="id" value="{{.ID}}"><button class="del" type="submit">Sil</button></form></td>
 </tr>{{end}}
 </table>
</div>
</body></html>`))
