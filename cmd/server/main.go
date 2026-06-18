// Bulbi backend: icerik servisi + liderlik tablosu (PostgreSQL) + push bildirim.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

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

	srv := &server.Server{Content: c, Store: st}
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
