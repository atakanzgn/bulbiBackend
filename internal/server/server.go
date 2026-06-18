// Package server HTTP API'sini tanimlar: icerik servisi, liderlik tablosu ve
// cihaz token kaydi.
package server

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"bulbi-backend/internal/content"
	"bulbi-backend/internal/store"
)

type Server struct {
	Content *content.Store
	Store   *store.Store
}

var validPuzzles = map[string]bool{"word": true, "number": true, "quiz": true}

// Routes tum uclari baglayip middleware ile sarar.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /api/v1/content", s.getContent)
	mux.HandleFunc("GET /api/v1/content/version", s.getVersion)
	mux.HandleFunc("POST /api/v1/scores", s.postScore)
	mux.HandleFunc("GET /api/v1/leaderboard", s.getLeaderboard)
	mux.HandleFunc("POST /api/v1/devices", s.postDevice)
	return cors(logging(mux))
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) getContent(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.Content.Get())
}

func (s *Server) getVersion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]int{"version": s.Content.Version()})
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
	if err := s.Store.SubmitScore(req.DeviceID, name, req.Puzzle, req.Day, req.Score); err != nil {
		log.Printf("skor kaydi hatasi: %v", err)
		writeError(w, http.StatusInternalServerError, "skor kaydedilemedi")
		return
	}
	rank, score, _, err := s.Store.MyRank(req.Puzzle, req.Day, req.DeviceID)
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

	top, err := s.Store.Leaderboard(puzzle, day, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "liderlik alinamadi")
		return
	}
	resp := map[string]any{"top": top}
	if deviceID := q.Get("deviceId"); deviceID != "" {
		rank, score, found, err := s.Store.MyRank(puzzle, day, deviceID)
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
	if err := s.Store.SaveDevice(req.DeviceID, req.Token, req.Platform); err != nil {
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
