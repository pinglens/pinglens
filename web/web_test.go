package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func render(t *testing.T, cfg Config) string {
	t.Helper()
	h, err := Handler(cfg)
	if err != nil {
		t.Fatalf("Handler() error = %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	resp := httptest.NewRecorder()
	h.ServeHTTP(resp, req)
	return resp.Body.String()
}

func TestHandlerDefaultsToGenericBranding(t *testing.T) {
	body := render(t, Config{})
	if !strings.Contains(body, "<title>PingLens</title>") {
		t.Fatalf("default page title missing PingLens: %s", body)
	}
	if strings.Contains(body, "Example Networks") {
		t.Fatalf("default page includes deployment branding: %s", body)
	}
	if strings.Contains(body, "<h2>Regions</h2>") || strings.Contains(body, `id="regions"`) {
		t.Fatalf("default page includes standalone regions panel: %s", body)
	}
	if strings.Contains(body, "<style>") {
		t.Fatalf("default page overrides the stylesheet theme: %s", body)
	}
}

func TestHandlerRendersDeploymentBranding(t *testing.T) {
	body := render(t, Config{
		AppName:      "PingLens",
		BrandName:    "Example Networks",
		BrandURL:     "https://www.example.com/",
		BrandLogoURL: "https://www.example.com/icon.png",
	})
	for _, want := range []string{
		"Example Networks",
		"https://www.example.com/",
		`<link rel="icon" href="https://www.example.com/icon.png">`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("branded page missing %q: %s", want, body)
		}
	}
}

func TestHandlerRendersOnlyConfiguredThemeColors(t *testing.T) {
	body := render(t, Config{Theme: Theme{Accent: "#ff2768", Background: "#050208"}})
	for _, want := range []string{"--accent: #ff2768;", "--bg: #050208;"} {
		if !strings.Contains(body, want) {
			t.Fatalf("themed page missing %q: %s", want, body)
		}
	}
	if strings.Contains(body, "--accent-2:") {
		t.Fatalf("themed page overrides unset secondary accent: %s", body)
	}
}

func TestHandlerRejectsNonHexThemeColors(t *testing.T) {
	for _, theme := range []Theme{
		{Accent: "red"},
		{AccentSecondary: "#12345"},
		{Background: "#000;} body{display:none"},
	} {
		if _, err := Handler(Config{Theme: theme}); err == nil {
			t.Fatalf("Handler(%+v) accepted invalid theme color", theme)
		}
	}
}
