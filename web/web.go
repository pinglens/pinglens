package web

import (
	"embed"
	"html/template"
	"net/http"
)

//go:embed index.html app.js styles.css
var content embed.FS

type Config struct {
	AppName      string
	BrandName    string
	BrandURL     string
	BrandLogoURL string
}

type handler struct {
	files http.Handler
	index *template.Template
	cfg   Config
}

func Handler(cfg Config) http.Handler {
	if cfg.AppName == "" {
		cfg.AppName = "PingLens"
	}
	return &handler{
		files: http.FileServer(http.FS(content)),
		index: template.Must(template.ParseFS(content, "index.html")),
		cfg:   cfg,
	}
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" || r.URL.Path == "/index.html" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = h.index.Execute(w, h.cfg)
		return
	}
	h.files.ServeHTTP(w, r)
}
