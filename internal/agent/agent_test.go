package agent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/pinglens/pinglens/internal/protocol"
)

func TestPostEventDetectsMissingJob(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/events" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	err := postEvent(context.Background(), srv.Client(), Config{CoordinatorURL: srv.URL, Token: "token"}, protocol.AgentEvent{JobID: "missing", ClusterID: "dfw1", Type: "ping"})
	if !errors.Is(err, errJobNotFound) {
		t.Fatalf("expected errJobNotFound, got %v", err)
	}
}

func TestPostEventDetectsCanceledJob(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/agent/events" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusGone)
	}))
	defer srv.Close()

	err := postEvent(context.Background(), srv.Client(), Config{CoordinatorURL: srv.URL, Token: "token"}, protocol.AgentEvent{JobID: "canceled", ClusterID: "dfw1", Type: "ping"})
	if !errors.Is(err, errJobCanceled) {
		t.Fatalf("expected errJobCanceled, got %v", err)
	}
}
