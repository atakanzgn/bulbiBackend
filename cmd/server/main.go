// Bulbi backend: icerik servisi + liderlik tablosu (PostgreSQL) + push bildirim.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
	_ "time/tzdata"

	"bulbi-backend/internal/cache"
	"bulbi-backend/internal/content"
	"bulbi-backend/internal/push"
	"bulbi-backend/internal/server"
	"bulbi-backend/internal/store"
)

func main() {
	addr := env("ADDR", ":8080")
	contentPath := env("CONTENT_PATH", "data/content.json")

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Fatal("DATABASE_URL gerekli (orn: postgres://bulbi:sifre@db:5432/bulbi?sslmode=disable)")
	}

	c, err := content.NewStore(contentPath)
	if err != nil {
		log.Fatalf("icerik yuklenemedi (%s): %v", contentPath, err)
	}
	log.Printf("icerik yuklendi: v%d, %d kelime, %d soru",
		c.Version(), len(c.Get().Words), len(c.Get().Questions))

	openCtx, cancelOpen := context.WithTimeout(context.Background(), 15*time.Second)
	st, err := store.Open(openCtx, dsn)
	cancelOpen()
	if err != nil {
		log.Fatalf("veritabani acilamadi: %v", err)
	}
	defer st.Close()
	log.Println("veritabani baglandi (postgres)")

	// Tablolar bossa content.json'dan ilk icerigi seed et.
	seedCtx, cancelSeed := context.WithTimeout(context.Background(), 30*time.Second)
	seeded, err := st.SeedIfEmpty(seedCtx, c.Get())
	cancelSeed()
	if err != nil {
		log.Printf("seed hatasi: %v", err)
	} else if seeded {
		log.Println("icerik DB'ye seed edildi (content.json)")
	}

	// Redis (opsiyonel): rate limit, admin brute-force ve icerik cache.
	redisCtx, cancelRedis := context.WithTimeout(context.Background(), 10*time.Second)
	rc, err := cache.New(redisCtx, os.Getenv("REDIS_URL"))
	cancelRedis()
	if err != nil {
		log.Fatalf("redis baglanamadi: %v", err)
	}
	defer rc.Close()
	if rc.Enabled() {
		log.Println("redis: etkin")
	} else {
		log.Println("redis: devre disi (REDIS_URL yok) — in-memory yedek kullanilir")
	}

	// Push (opsiyonel): FCM_CREDENTIALS verilirse etkin.
	var sender *push.Sender
	if credPath := os.Getenv("FCM_CREDENTIALS"); credPath != "" {
		creds, err := os.ReadFile(credPath)
		if err != nil {
			log.Fatalf("FCM kimlik dosyasi okunamadi: %v", err)
		}
		sender, err = push.NewSender(creds, os.Getenv("FCM_PROJECT_ID"))
		if err != nil {
			log.Fatalf("FCM gonderici olusturulamadi: %v", err)
		}
		log.Println("push: etkin")
	} else {
		log.Println("push: devre disi (FCM_CREDENTIALS tanimli degil)")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if sender != nil {
		bc := &push.Broadcaster{
			Sender: sender,
			Tokens: st.AllTokens,
			Hour:   envInt("PUSH_HOUR", 10),
			Title:  "Bulbi",
			Body:   "Bugünün bulmacaları hazır! 🧩",
		}
		go bc.Run(ctx)
	}

	uploadDir := env("UPLOAD_DIR", "data/uploads")
	if err := os.MkdirAll(uploadDir, 0o755); err != nil {
		log.Printf("upload dizini olusturulamadi: %v", err)
	}
	// Eski bildirim gorsellerini periyodik temizle (varsayilan 30 gun).
	go cleanupUploads(ctx, uploadDir, envInt("UPLOAD_RETENTION_DAYS", 30))
	srv := &server.Server{
		Store:           st,
		Cache:           rc,
		Push:            sender,
		AdminPassword:   os.Getenv("ADMIN_PASSWORD"),
		RateLimitPerMin: envInt("RATE_LIMIT_PER_MIN", 120),
		MinAppBuild:     envInt("MIN_APP_BUILD", 1),
		UploadDir:       uploadDir,
		PublicBaseURL:   os.Getenv("PUBLIC_BASE_URL"),
	}
	if srv.AdminPassword != "" {
		log.Println("admin paneli etkin: /admin")
	} else {
		log.Println("admin paneli kapali (ADMIN_PASSWORD tanimli degil)")
	}

	// Icerik cache'ini isit + her gun TR 00:00'da yenile.
	srv.RefreshContentCache(context.Background())
	go dailyCacheRefresh(ctx, srv)
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("Bulbi backend dinliyor: %s", addr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server hatasi: %v", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	log.Println("kapatildi")
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// cleanupUploads UploadDir'deki retentionDays'ten eski gorselleri her gun siler.
// retentionDays <= 0 ise temizleme kapalidir.
func cleanupUploads(ctx context.Context, dir string, retentionDays int) {
	if retentionDays <= 0 {
		return
	}
	run := func() {
		cutoff := time.Now().AddDate(0, 0, -retentionDays)
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		removed := 0
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			if info.ModTime().Before(cutoff) {
				if err := os.Remove(filepath.Join(dir, e.Name())); err == nil {
					removed++
				}
			}
		}
		if removed > 0 {
			log.Printf("upload temizligi: %d eski gorsel silindi (>%d gun)", removed, retentionDays)
		}
	}
	run() // baslangicta bir kez
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}

// dailyCacheRefresh her gun Turkiye saatiyle 00:00'da icerik cache'ini yeniler.
func dailyCacheRefresh(ctx context.Context, srv *server.Server) {
	loc, err := time.LoadLocation("Europe/Istanbul")
	if err != nil {
		loc = time.UTC
	}
	for {
		now := time.Now().In(loc)
		next := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc).
			AddDate(0, 0, 1)
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			srv.RefreshContentCache(context.Background())
			log.Println("icerik cache yenilendi (TR 00:00)")
		}
	}
}
