// paymux-home serves the static React website. It has no API, SSR or payment-hub dependency.
package main

import (
	"embed"
	"encoding/json"
	"flag"
	"io/fs"
	"log"
	"net/http"
	"strings"
	"time"
)

// Build the frontend with `bun run build:home` before building this binary.
// The tracked .keep lets Go source checks run before frontend installation.
//
//go:embed all:dist
var assets embed.FS

//go:embed docs-catalog.json
var docsCatalog []byte

func handler() http.Handler {
	site, err := fs.Sub(assets, "dist")
	if err != nil {
		panic(err)
	}
	return websiteHandler(site)
}
func websiteHandler(site fs.FS) http.Handler {
	files := http.FileServerFS(site)
	index, indexErr := fs.ReadFile(site, "index.html")
	var guides []struct {
		Slug string `json:"slug"`
	}
	if err := json.Unmarshal(docsCatalog, &guides); err != nil {
		panic(err)
	}
	pages := map[string]bool{"/": true, "/integrations": true, "/docs": true, "/self-hosting": true}
	for _, guide := range guides {
		if guide.Slug != "" {
			pages["/docs/"+guide.Slug] = true
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if indexErr != nil {
			http.Error(w, "frontend is not built", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /assets/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		files.ServeHTTP(w, r)
	})
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if indexErr != nil {
			http.Error(w, "Build the website with bun run build:home, then rebuild paymux-home.", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		if !pages[strings.TrimSuffix(r.URL.Path, "/")] && r.URL.Path != "/" {
			w.WriteHeader(http.StatusNotFound)
		}
		w.Write(index)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		mux.ServeHTTP(w, r)
	})
}
func main() {
	listen := flag.String("listen", "127.0.0.1:8090", "static website listen address")
	flag.Parse()
	s := &http.Server{Addr: *listen, Handler: handler(), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
	log.Printf("Paymux website listening on %s", *listen)
	log.Fatal(s.ListenAndServe())
}
