// paymux-home is the independently deployed public Paymux website.
package main

import (
	"embed"
	"flag"
	"html/template"
	"log"
	"net/http"
	"time"
)

//go:embed static/*
var assets embed.FS

var pages = map[string]string{"/": "home", "/integrations": "integrations", "/docs": "docs", "/self-hosting": "hosting"}

func handler() http.Handler {
	t := template.Must(template.ParseFS(assets, "static/*.html"))
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.FileServer(http.FS(assets)))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		page, ok := pages[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		titles := map[string]string{"home": "Payment infrastructure on your terms", "integrations": "Provider capabilities", "docs": "Start building", "hosting": "Your infrastructure. Your responsibility."}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := t.ExecuteTemplate(w, "layout", map[string]string{"Page": page, "Title": titles[page]}); err != nil {
			log.Printf("render: %v", err)
		}
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		mux.ServeHTTP(w, r)
	})
}
func main() {
	listen := flag.String("listen", "127.0.0.1:8090", "website listen address")
	flag.Parse()
	s := &http.Server{Addr: *listen, Handler: handler(), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
	log.Printf("Paymux website listening on %s", *listen)
	log.Fatal(s.ListenAndServe())
}
