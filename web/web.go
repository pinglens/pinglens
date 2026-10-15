package web

import (
	"embed"
	"fmt"
	"html/template"
	"net/http"
	"regexp"
)

//go:embed index.html app.js styles.css fonts
var content embed.FS

type Config struct {
	AppName      string
	BrandName    string
	BrandURL     string
	BrandLogoURL string
	Theme        Theme
}

// Theme overrides the default palette. Empty fields keep the built-in
// defaults from styles.css; set fields must be hex colors.
type Theme struct {
	Accent          string
	AccentSecondary string
	Background      string
}

func (t Theme) IsZero() bool {
	return t == Theme{}
}

var hexColor = regexp.MustCompile(`^#(?:[0-9a-fA-F]{3,4}|[0-9a-fA-F]{6}|[0-9a-fA-F]{8})$`)

func (t Theme) validate() error {
	for _, field := range []struct{ name, value string }{
		{"accent", t.Accent},
		{"accent secondary", t.AccentSecondary},
		{"background", t.Background},
	} {
		if field.value != "" && !hexColor.MatchString(field.value) {
			return fmt.Errorf("theme %s %q must be a hex color such as #2563eb", field.name, field.value)
		}
	}
	return nil
}

type handler struct {
	files http.Handler
	index *template.Template
	cfg   Config
}

func Handler(cfg Config) (http.Handler, error) {
	if err := cfg.Theme.validate(); err != nil {
		return nil, err
	}
	if cfg.AppName == "" {
		cfg.AppName = "PingLens"
	}
	return &handler{
		files: http.FileServer(http.FS(content)),
		index: template.Must(template.ParseFS(content, "index.html")),
		cfg:   cfg,
	}, nil
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" || r.URL.Path == "/index.html" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = h.index.Execute(w, h.cfg)
		return
	}
	h.files.ServeHTTP(w, r)
}
