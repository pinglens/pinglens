package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandlerDefaultsToGenericBranding(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	resp := httptest.NewRecorder()

	Handler(Config{}).ServeHTTP(resp, req)

	body := resp.Body.String()
	if !strings.Contains(body, "<title>PingLens</title>") {
		t.Fatalf("default page title missing PingLens: %s", body)
	}
	if strings.Contains(body, "Example Networks") {
		t.Fatalf("default page includes deployment branding: %s", body)
	}
	if strings.Contains(body, "<h2>Regions</h2>") || strings.Contains(body, `id="regions"`) {
		t.Fatalf("default page includes standalone regions panel: %s", body)
	}
}

func TestHandlerRendersDeploymentBranding(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	resp := httptest.NewRecorder()

	Handler(Config{
		AppName:      "PingLens",
		BrandName:    "Example Networks",
		BrandURL:     "https://www.example.com/",
		BrandLogoURL: "https://www.example.com/favicon.ico",
	}).ServeHTTP(resp, req)

	body := resp.Body.String()
	for _, want := range []string{
		"Example Networks",
		"https://www.example.com/",
		"https://www.example.com/favicon.ico",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("branded page missing %q: %s", want, body)
		}
	}
}
