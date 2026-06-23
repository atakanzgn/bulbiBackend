// Package server HTTP API'sini tanimlar: icerik servisi, liderlik tablosu ve
// cihaz token kaydi.
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"bulbi-backend/internal/cache"
	"bulbi-backend/internal/content"
	"bulbi-backend/internal/push"
	"bulbi-backend/internal/store"

	"github.com/xuri/excelize/v2"
)

type Server struct {
	Store           *store.Store
	Cache           *cache.Cache
	Push            *push.Sender
	AdminPassword   string // bos ise admin paneli kapali
	RateLimitPerMin int    // 0 -> 120
	MinAppBuild     int    // istemcinin gerektirdigi minimum build numarasi
	UploadDir       string // yuklenen bildirim gorselleri
	PublicBaseURL   string // gorsel URL'i icin (bos ise istek Host'undan)

	mem *memStore
}

const adminPageSize = 20

const cacheKeyBundle = "content:bundle"

var validPuzzles = map[string]bool{
	"word": true, "number": true, "quiz": true,
	// "daily": bugun cozulen gunluk oyun sayisi (birlesik liderlik).
	"daily": true,
	// Ileride oyun-bazli liderlik acmak istenirse hazir.
	"hangman": true, "wordsearch": true, "sudoku": true,
	"connections": true, "memory": true,
}

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
	mux.HandleFunc("POST /api/v1/devices/delete", s.postDeviceDelete)

	// Admin paneli (Basic Auth, ADMIN_PASSWORD)
	mux.HandleFunc("GET /admin", s.adminGuard(s.adminHome))
	mux.HandleFunc("POST /admin/words", s.adminGuard(s.adminAddWord))
	mux.HandleFunc("POST /admin/words/delete", s.adminGuard(s.adminDeleteWord))
	mux.HandleFunc("POST /admin/questions", s.adminGuard(s.adminAddQuestion))
	mux.HandleFunc("POST /admin/questions/delete", s.adminGuard(s.adminDeleteQuestion))
	mux.HandleFunc("POST /admin/import/words", s.adminGuard(s.adminImportWords))
	mux.HandleFunc("POST /admin/import/questions", s.adminGuard(s.adminImportQuestions))
	mux.HandleFunc("POST /admin/notify", s.adminGuard(s.adminNotify))
	mux.HandleFunc("POST /admin/connections", s.adminGuard(s.adminAddConnection))
	mux.HandleFunc("POST /admin/connections/delete",
		s.adminGuard(s.adminDeleteConnection))

	// Yuklenen bildirim gorselleri (FCM image URL'i icin herkese acik).
	mux.Handle("GET /uploads/",
		http.StripPrefix("/uploads/", http.FileServer(http.Dir(s.UploadDir))))

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
	version, words, questions, connections, err := s.Store.ContentBundle(ctx)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{
		"version":     version,
		"words":       words,
		"questions":   questions,
		"connections": connections,
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

	// period=weekly -> son 7 gunun (gun-6..gun) gunluk skorlarinin toplami.
	weekly := q.Get("period") == "weekly"
	weekStart := day - 6
	if weekStart < 1 {
		weekStart = 1
	}

	var top []store.Entry
	if weekly {
		top, err = s.Store.LeaderboardWeekly(r.Context(), puzzle, weekStart, day, limit)
	} else {
		top, err = s.Store.Leaderboard(r.Context(), puzzle, day, limit)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "liderlik alinamadi")
		return
	}
	resp := map[string]any{"top": top}
	if deviceID := q.Get("deviceId"); deviceID != "" {
		var (
			rank, score int
			found       bool
			merr        error
		)
		if weekly {
			rank, score, found, merr = s.Store.MyRankWeekly(r.Context(), puzzle, weekStart, day, deviceID)
		} else {
			rank, score, found, merr = s.Store.MyRank(r.Context(), puzzle, day, deviceID)
		}
		if merr == nil && found {
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

// postDeviceDelete bildirim kapatildiginda cihaz token kaydini siler.
func (s *Server) postDeviceDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		DeviceID string `json:"deviceId"`
	}
	if !decode(w, r, &req) {
		return
	}
	if req.DeviceID == "" {
		writeError(w, http.StatusBadRequest, "deviceId gerekli")
		return
	}
	if err := s.Store.DeleteDevice(r.Context(), req.DeviceID); err != nil {
		writeError(w, http.StatusInternalServerError, "cihaz silinemedi")
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
	Version        int
	Words          []store.WordRow
	Questions      []store.QuestionRow
	WordsTotal     int
	QuestionsTotal int
	WordPage       int
	WordPages      int
	QuestionPage   int
	QuestionPages  int
	Connections      []store.ConnectionRow
	ConnectionsTotal int
	PushEnabled    bool
	Notice         string
}

func pageParam(r *http.Request, key string) int {
	if p, err := strconv.Atoi(r.URL.Query().Get(key)); err == nil && p > 0 {
		return p
	}
	return 1
}

func pageCount(total, size int) int {
	if total <= 0 {
		return 1
	}
	return (total + size - 1) / size
}

func (s *Server) adminHome(w http.ResponseWriter, r *http.Request) {
	wp := pageParam(r, "wp")
	qp := pageParam(r, "qp")
	wordsTotal, _ := s.Store.CountWords(r.Context())
	qTotal, _ := s.Store.CountQuestions(r.Context())
	words, err := s.Store.ListWordsPaged(r.Context(), adminPageSize, (wp-1)*adminPageSize)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	questions, err :=
		s.Store.ListQuestionsPaged(r.Context(), adminPageSize, (qp-1)*adminPageSize)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	version, _ := s.Store.ContentVersion(r.Context())
	conns, _ := s.Store.ListConnections(r.Context())
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := adminTmpl.Execute(w, adminData{
		Version:          version,
		Words:            words,
		Questions:        questions,
		WordsTotal:       wordsTotal,
		QuestionsTotal:   qTotal,
		WordPage:         wp,
		WordPages:        pageCount(wordsTotal, adminPageSize),
		QuestionPage:     qp,
		QuestionPages:    pageCount(qTotal, adminPageSize),
		Connections:      conns,
		ConnectionsTotal: len(conns),
		PushEnabled:      s.Push != nil,
		Notice:           r.URL.Query().Get("msg"),
	}); err != nil {
		log.Printf("admin template: %v", err)
	}
}

func redirectMsg(w http.ResponseWriter, r *http.Request, msg string) {
	http.Redirect(w, r, "/admin?msg="+url.QueryEscape(msg), http.StatusSeeOther)
}

func (s *Server) adminImportWords(w http.ResponseWriter, r *http.Request) {
	rows, err := parseUploadedSheet(r)
	if err != nil {
		redirectMsg(w, r, "Excel okunamadi: "+err.Error())
		return
	}
	words := make([]string, 0, len(rows))
	for i, row := range rows {
		c := strings.TrimSpace(cell(row, 0))
		if c == "" || (i == 0 && isHeader(c, "kelime", "word")) {
			continue
		}
		words = append(words, c)
	}
	n, err := s.Store.ImportWords(r.Context(), words)
	if err != nil {
		redirectMsg(w, r, "Hata: "+err.Error())
		return
	}
	s.Cache.Del(r.Context(), cacheKeyBundle)
	redirectMsg(w, r, fmt.Sprintf("%d kelime eklendi", n))
}

// adminAddConnection: her satir bir grup -> "Kategori: k1,k2,k3,k4" (4 satir).
func (s *Server) adminAddConnection(w http.ResponseWriter, r *http.Request) {
	var groups []content.ConnectionGroup
	for _, ln := range strings.Split(r.FormValue("groups"), "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		parts := strings.SplitN(ln, ":", 2)
		if len(parts) != 2 {
			continue
		}
		cat := strings.TrimSpace(parts[0])
		var ws []string
		for _, x := range strings.Split(parts[1], ",") {
			x = store.UpperTR(x)
			if x != "" {
				ws = append(ws, x)
			}
		}
		if cat == "" || len(ws) != 4 {
			continue
		}
		groups = append(groups, content.ConnectionGroup{
			Category: cat,
			Words:    ws,
			Level:    len(groups),
		})
	}
	if len(groups) != 4 {
		redirectMsg(w, r, "4 grup gerekli; her satir: 'Kategori: k1,k2,k3,k4'")
		return
	}
	if err := s.Store.AddConnection(
		r.Context(), content.ConnectionPuzzle{Groups: groups}); err != nil {
		redirectMsg(w, r, "Hata: "+err.Error())
		return
	}
	s.Cache.Del(r.Context(), cacheKeyBundle)
	redirectMsg(w, r, "Bağlantı bulmacası eklendi")
}

func (s *Server) adminDeleteConnection(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.FormValue("id"), 10, 64)
	if err := s.Store.DeleteConnection(r.Context(), id); err != nil {
		redirectMsg(w, r, "Hata: "+err.Error())
		return
	}
	s.Cache.Del(r.Context(), cacheKeyBundle)
	redirectMsg(w, r, "Silindi")
}

func (s *Server) adminImportQuestions(w http.ResponseWriter, r *http.Request) {
	rows, err := parseUploadedSheet(r)
	if err != nil {
		redirectMsg(w, r, "Excel okunamadi: "+err.Error())
		return
	}
	qs := make([]content.Question, 0, len(rows))
	for i, row := range rows {
		q := strings.TrimSpace(cell(row, 0))
		if q == "" || (i == 0 && isHeader(q, "soru", "question")) {
			continue
		}
		opts := []string{}
		for c := 1; c <= 4; c++ {
			if v := strings.TrimSpace(cell(row, c)); v != "" {
				opts = append(opts, v)
			}
		}
		if len(opts) < 2 {
			continue
		}
		ans, _ := strconv.Atoi(strings.TrimSpace(cell(row, 5)))
		ans-- // Excel'de 1-tabanli -> 0-tabanli
		if ans < 0 || ans >= len(opts) {
			ans = 0
		}
		qs = append(qs, content.Question{
			Q:          q,
			Options:    opts,
			Answer:     ans,
			Category:   normCat(cell(row, 6)),
			Difficulty: normDiff(cell(row, 7)),
		})
	}
	n, err := s.Store.ImportQuestions(r.Context(), qs)
	if err != nil {
		redirectMsg(w, r, "Hata: "+err.Error())
		return
	}
	s.Cache.Del(r.Context(), cacheKeyBundle)
	redirectMsg(w, r, fmt.Sprintf("%d soru eklendi", n))
}

func (s *Server) adminNotify(w http.ResponseWriter, r *http.Request) {
	if s.Push == nil {
		redirectMsg(w, r, "Push kapali (FCM yapilandirilmamis)")
		return
	}
	if err := r.ParseMultipartForm(16 << 20); err != nil {
		redirectMsg(w, r, "Form okunamadi")
		return
	}
	title := strings.TrimSpace(r.FormValue("title"))
	body := strings.TrimSpace(r.FormValue("body"))
	if title == "" || body == "" {
		redirectMsg(w, r, "Baslik ve icerik gerekli")
		return
	}
	image := ""
	if file, hdr, err := r.FormFile("image"); err == nil {
		defer file.Close()
		path, serr := s.saveUpload(file, hdr)
		if serr != nil {
			redirectMsg(w, r, "Gorsel: "+serr.Error())
			return
		}
		image = s.publicURL(r, path)
	}
	go func() {
		tokens, err := s.Store.AllTokens(context.Background())
		if err != nil {
			log.Printf("notify: token listesi: %v", err)
			return
		}
		sent := s.Push.SendToAll(context.Background(), tokens, title, body, image)
		log.Printf("admin bildirim: %d/%d gonderildi (gorsel: %t)", sent, len(tokens), image != "")
	}()
	redirectMsg(w, r, "Bildirim gonderiliyor…")
}

var allowedImageExt = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".webp": true, ".gif": true,
}

// saveUpload yuklenen gorseli UploadDir'e kaydeder, herkese acik yolu doner.
func (s *Server) saveUpload(file multipart.File, hdr *multipart.FileHeader) (string, error) {
	ext := strings.ToLower(filepath.Ext(hdr.Filename))
	if !allowedImageExt[ext] {
		return "", fmt.Errorf("desteklenmeyen format (jpg/png/webp/gif)")
	}
	if err := os.MkdirAll(s.UploadDir, 0o755); err != nil {
		return "", err
	}
	name := fmt.Sprintf("%d%s", time.Now().UnixNano(), ext)
	dst, err := os.Create(filepath.Join(s.UploadDir, name))
	if err != nil {
		return "", err
	}
	defer dst.Close()
	if _, err := io.Copy(dst, file); err != nil {
		return "", err
	}
	return "/uploads/" + name, nil
}

func (s *Server) publicURL(r *http.Request, path string) string {
	base := s.PublicBaseURL
	if base == "" {
		base = "https://" + r.Host
	}
	return strings.TrimRight(base, "/") + path
}

func parseUploadedSheet(r *http.Request) ([][]string, error) {
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		return nil, err
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		return nil, err
	}
	defer file.Close()
	f, err := excelize.OpenReader(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sheets := f.GetSheetList()
	if len(sheets) == 0 {
		return nil, fmt.Errorf("bos dosya")
	}
	return f.GetRows(sheets[0])
}

func cell(row []string, i int) string {
	if i < len(row) {
		return row[i]
	}
	return ""
}

func isHeader(v string, keys ...string) bool {
	lv := strings.ToLower(strings.TrimSpace(v))
	for _, k := range keys {
		if lv == k {
			return true
		}
	}
	return false
}

func normCat(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "spor":
		return "spor"
	case "bilim":
		return "bilim"
	case "sanat":
		return "sanat"
	default:
		return "genel"
	}
}

func normDiff(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "kolay":
		return "kolay"
	case "zor":
		return "zor"
	default:
		return "orta"
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

var adminTmpl = template.Must(template.New("admin").Funcs(template.FuncMap{
	"add": func(a, b int) int { return a + b },
}).Parse(`<!doctype html>
<html lang="tr"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Bulbi Admin</title>
<style>
 body{font-family:system-ui,Arial,sans-serif;max-width:920px;margin:0 auto;padding:20px;background:#f6f6fb;color:#1a1a2e}
 h1{color:#5C5CE0;margin-bottom:0} h2{margin-top:0;font-size:18px}
 .card{background:#fff;border-radius:12px;padding:16px;margin:14px 0;box-shadow:0 1px 4px rgba(0,0,0,.06)}
 input,select{padding:8px;border:1px solid #ccd;border-radius:8px;margin:4px 4px 4px 0;font-size:14px}
 input.full{width:100%}
 button{background:#5C5CE0;color:#fff;border:0;border-radius:8px;padding:9px 16px;cursor:pointer;font-size:14px;font-weight:600}
 button.del{background:#d64545;padding:5px 10px}
 table{width:100%;border-collapse:collapse;margin-top:8px}
 td,th{text-align:left;padding:6px 8px;border-bottom:1px solid #eee;font-size:14px;vertical-align:top}
 .muted{color:#888;font-size:13px}
 .notice{background:#e6f7ec;border:1px solid #9bdcb4;color:#176b3a;padding:10px 14px;border-radius:10px;margin:12px 0}
 .pager{display:flex;gap:12px;align-items:center;margin-top:10px;font-size:14px}
 .pager a{color:#5C5CE0;text-decoration:none;font-weight:600}
 .sub{border-top:1px dashed #ddd;margin-top:12px;padding-top:12px}
 .lists{display:flex;gap:14px;align-items:flex-start;flex-wrap:wrap}
 .lists>.card{flex:1;min-width:300px;margin:0}
</style></head><body>
<h1>Bulbi Admin</h1>
<p class="muted">İçerik sürümü: {{.Version}} · Kelime: {{.WordsTotal}} · Soru: {{.QuestionsTotal}} · Bağlantı: {{.ConnectionsTotal}}</p>
{{if .Notice}}<div class="notice">{{.Notice}}</div>{{end}}

{{if .PushEnabled}}
<div class="card">
 <h2>📣 Bildirim gönder — tüm kullanıcılar</h2>
 <form method="post" action="/admin/notify" enctype="multipart/form-data">
  <input class="full" name="title" placeholder="Başlık" required>
  <input class="full" name="body" placeholder="İçerik" required>
  <div class="muted" style="margin-top:6px">Görsel (opsiyonel, jpg/png):</div>
  <input type="file" name="image" accept="image/*">
  <button type="submit">Gönder</button>
  <div class="muted">Kayıtlı tüm cihazlara anında FCM bildirimi gider.</div>
 </form>
</div>
{{end}}

<div class="card">
 <h2>Kelime ekle</h2>
 <form method="post" action="/admin/words">
  <input name="text" placeholder="örn. araba (otomatik BÜYÜK harfe çevrilir)" required>
  <button type="submit">Ekle</button>
 </form>
 <div class="sub">
  <form method="post" action="/admin/import/words" enctype="multipart/form-data">
   <b>Excel ile toplu:</b> <input type="file" name="file" accept=".xlsx" required>
   <button type="submit">Yükle</button>
   <div class="muted">A sütununda kelimeler (her satır bir kelime). Başlık satırı atlanır.</div>
  </form>
 </div>
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
 <div class="sub">
  <form method="post" action="/admin/import/questions" enctype="multipart/form-data">
   <b>Excel ile toplu:</b> <input type="file" name="file" accept=".xlsx" required>
   <button type="submit">Yükle</button>
   <div class="muted">Sütunlar: Soru | Seçenek1 | Seçenek2 | Seçenek3 | Seçenek4 | Doğru(1-4) | Tür | Zorluk</div>
  </form>
 </div>
</div>

<div class="card">
 <h2>🔗 Bağlantı bulmacası ekle ({{.ConnectionsTotal}})</h2>
 <form method="post" action="/admin/connections">
  <div class="muted">4 satır — her satır bir grup: <b>Kategori: k1,k2,k3,k4</b></div>
  <textarea name="groups" rows="5" required style="width:100%;box-sizing:border-box;padding:8px;border:1px solid #ccd;border-radius:8px;margin:6px 0;font-size:14px" placeholder="Meyveler: ELMA,ARMUT,KİRAZ,ÜZÜM&#10;Gezegenler: MARS,VENÜS,DÜNYA,JÜPİTER&#10;..."></textarea>
  <button type="submit">Ekle</button>
 </form>
 {{range .Connections}}
 <div class="sub">
  {{range .Groups}}<b>{{.Category}}:</b> {{range .Words}}{{.}} {{end}}<br>{{end}}
  <form method="post" action="/admin/connections/delete" onsubmit="return confirm('Silinsin mi?')" style="margin-top:6px"><input type="hidden" name="id" value="{{.ID}}"><button class="del" type="submit">Sil</button></form>
 </div>
 {{end}}
</div>

<div class="lists">
<div class="card">
 <h2>Kelimeler ({{.WordsTotal}})</h2>
 <table><tr><th>Kelime</th><th>Harf</th><th></th></tr>
 {{range .Words}}<tr><td>{{.Text}}</td><td>{{.Length}}</td>
  <td><form method="post" action="/admin/words/delete" onsubmit="return confirm('Silinsin mi?')"><input type="hidden" name="id" value="{{.ID}}"><button class="del" type="submit">Sil</button></form></td></tr>{{end}}
 </table>
 <div class="pager">
  {{if gt .WordPage 1}}<a href="/admin?wp={{add .WordPage -1}}&qp={{.QuestionPage}}">‹ Önceki</a>{{end}}
  <span>Sayfa {{.WordPage}} / {{.WordPages}}</span>
  {{if lt .WordPage .WordPages}}<a href="/admin?wp={{add .WordPage 1}}&qp={{.QuestionPage}}">Sonraki ›</a>{{end}}
 </div>
</div>

<div class="card">
 <h2>Sorular ({{.QuestionsTotal}})</h2>
 <table><tr><th>Soru</th><th>Tür / Zorluk</th><th>Doğru</th><th></th></tr>
 {{range .Questions}}<tr>
  <td>{{.Q}}<div class="muted">{{range .Options}}{{.}} &middot; {{end}}</div></td>
  <td>{{.Category}} / {{.Difficulty}}</td>
  <td>{{index .Options .Answer}}</td>
  <td><form method="post" action="/admin/questions/delete" onsubmit="return confirm('Silinsin mi?')"><input type="hidden" name="id" value="{{.ID}}"><button class="del" type="submit">Sil</button></form></td>
 </tr>{{end}}
 </table>
 <div class="pager">
  {{if gt .QuestionPage 1}}<a href="/admin?wp={{.WordPage}}&qp={{add .QuestionPage -1}}">‹ Önceki</a>{{end}}
  <span>Sayfa {{.QuestionPage}} / {{.QuestionPages}}</span>
  {{if lt .QuestionPage .QuestionPages}}<a href="/admin?wp={{.WordPage}}&qp={{add .QuestionPage 1}}">Sonraki ›</a>{{end}}
 </div>
</div>
</div>
</body></html>`))
