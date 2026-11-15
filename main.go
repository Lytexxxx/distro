package main

import (
	"context"
	"embed"
	"flag"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

//go:embed web
var webFS embed.FS

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func envInt(k string, d int64) int64 {
	if v, err := strconv.ParseInt(os.Getenv(k), 10, 64); err == nil && v > 0 {
		return v
	}
	return d
}

func secureHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data: blob:; media-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		next.ServeHTTP(w, r)
	})
}

func main() {
	addr := flag.String("addr", ":"+envOr("PORT", "8080"), "adresse d'écoute")
	dir := flag.String("data", envOr("DATA_DIR", "data"), "dossier des données")
	maxMB := flag.Int64("max-upload", envInt("MAX_UPLOAD_MB", defaultMaxUpload>>20), "taille maximale d'une pièce jointe, en Mo")
	flag.Parse()
	if *maxMB < 1 {
		*maxMB = 1
	}

	st, err := OpenStore(*dir)
	if err != nil {
		log.Fatalf("ouverture des données: %v", err)
	}
	app := &App{
		s: st, hub: NewHub(), rl: NewLimiter(), voice: NewVoice(),
		secure:     os.Getenv("SECURE_COOKIES") == "1",
		trustProxy: os.Getenv("TRUST_PROXY") == "1",
		maxFile:    *maxMB << 20,
	}

	mux := app.routes()
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatal(err)
	}
	static := http.FileServer(http.FS(sub))
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		static.ServeHTTP(w, r)
	})

	srv := &http.Server{
		Addr:              *addr,
		Handler:           secureHeaders(mux),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second, // levé pour le flux SSE
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		t := time.NewTicker(1 * time.Second)
		gc := 0
		for range t.C {
			if err := st.Save(); err != nil {
				log.Printf("sauvegarde: %v", err)
			}
			if gc++; gc%200 == 0 {
				app.rl.GC()
			}
		}
	}()

	go func() {
		abs, _ := filepath.Abs(*dir)
		log.Printf("Distro écoute sur %s", *addr)
		log.Printf("DONNEES ENREGISTREES DANS : %s  (a sauvegarder ; sauvegardes auto dans le sous-dossier backups)", abs)
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Println("arrêt…")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
	st.Close()
}
