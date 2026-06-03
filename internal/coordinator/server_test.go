package coordinator

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/pinglens/pinglens/internal/protocol"
)

func TestCreateJobQueuesOnlineAgent(t *testing.T) {
	srv := newTestServer(t, Config{AgentTokens: map[string]string{"dfw1": "secret"}, AgentPollTimeout: 10 * time.Millisecond, JobDuration: 3 * time.Second})

	pollBody, _ := json.Marshal(protocol.AgentPollRequest{ClusterID: "dfw1", DisplayName: "Example DFW", Version: protocol.Version})
	pollReq := httptest.NewRequest(http.MethodPost, "/api/agent/poll", bytes.NewReader(pollBody))
	pollReq.Header.Set("Authorization", "Bearer secret")
	pollReq.Header.Set("Content-Type", "application/json")
	pollResp := httptest.NewRecorder()
	srv.ServeHTTP(pollResp, pollReq)
	if pollResp.Code != http.StatusOK {
		t.Fatalf("poll failed: %d %s", pollResp.Code, pollResp.Body.String())
	}

	createBody, _ := json.Marshal(protocol.JobCreateRequest{Target: "example.com"})
	createReq := httptest.NewRequest(http.MethodPost, "/api/jobs", bytes.NewReader(createBody))
	createReq.RemoteAddr = "198.51.100.9:12345"
	createResp := httptest.NewRecorder()
	srv.ServeHTTP(createResp, createReq)
	if createResp.Code != http.StatusAccepted {
		t.Fatalf("create failed: %d %s", createResp.Code, createResp.Body.String())
	}

	pollReq = httptest.NewRequest(http.MethodPost, "/api/agent/poll", bytes.NewReader(pollBody))
	pollReq.Header.Set("Authorization", "Bearer secret")
	pollReq.Header.Set("Content-Type", "application/json")
	pollResp = httptest.NewRecorder()
	srv.ServeHTTP(pollResp, pollReq)
	if pollResp.Code != http.StatusOK {
		t.Fatalf("second poll failed: %d", pollResp.Code)
	}
	var poll protocol.AgentPollResponse
	if err := json.NewDecoder(pollResp.Body).Decode(&poll); err != nil {
		t.Fatal(err)
	}
	if poll.Job == nil || poll.Job.Target != "example.com" || poll.Job.Count != 3 || poll.Job.DurationMillis != 3000 || poll.Job.MTR.Cycles != 10 {
		t.Fatalf("unexpected job: %+v", poll.Job)
	}
	var create protocol.JobCreateResponse
	if err := json.Unmarshal(createResp.Body.Bytes(), &create); err != nil {
		t.Fatal(err)
	}
	if len(create.Regions) != 1 || create.Regions[0].ID != "dfw1" || create.Regions[0].Name != "Example DFW" {
		t.Fatalf("unexpected regions: %+v", create.Regions)
	}
}

func TestCancelJobRejectsFurtherAgentEvents(t *testing.T) {
	srv := newTestServer(t, Config{AgentTokens: map[string]string{"dfw1": "secret"}, AgentPollTimeout: 10 * time.Millisecond, JobDuration: 3 * time.Second})

	pollBody, _ := json.Marshal(protocol.AgentPollRequest{ClusterID: "dfw1", DisplayName: "DFW", Version: protocol.Version})
	pollReq := httptest.NewRequest(http.MethodPost, "/api/agent/poll", bytes.NewReader(pollBody))
	pollReq.Header.Set("Authorization", "Bearer secret")
	pollReq.Header.Set("Content-Type", "application/json")
	srv.ServeHTTP(httptest.NewRecorder(), pollReq)

	createBody, _ := json.Marshal(protocol.JobCreateRequest{Target: "example.com"})
	createReq := httptest.NewRequest(http.MethodPost, "/api/jobs", bytes.NewReader(createBody))
	createReq.RemoteAddr = "198.51.100.10:12345"
	createResp := httptest.NewRecorder()
	srv.ServeHTTP(createResp, createReq)
	if createResp.Code != http.StatusAccepted {
		t.Fatalf("create failed: %d %s", createResp.Code, createResp.Body.String())
	}
	var create protocol.JobCreateResponse
	if err := json.Unmarshal(createResp.Body.Bytes(), &create); err != nil {
		t.Fatal(err)
	}

	cancelReq := httptest.NewRequest(http.MethodDelete, "/api/jobs/"+create.JobID, nil)
	cancelResp := httptest.NewRecorder()
	srv.ServeHTTP(cancelResp, cancelReq)
	if cancelResp.Code != http.StatusAccepted {
		t.Fatalf("cancel failed: %d %s", cancelResp.Code, cancelResp.Body.String())
	}

	eventBody, _ := json.Marshal(protocol.AgentEvent{JobID: create.JobID, ClusterID: "dfw1", Type: "ping"})
	eventReq := httptest.NewRequest(http.MethodPost, "/api/agent/events", bytes.NewReader(eventBody))
	eventReq.Header.Set("Authorization", "Bearer secret")
	eventReq.Header.Set("Content-Type", "application/json")
	eventResp := httptest.NewRecorder()
	srv.ServeHTTP(eventResp, eventReq)
	if eventResp.Code != http.StatusGone {
		t.Fatalf("expected 410 after cancel, got %d %s", eventResp.Code, eventResp.Body.String())
	}
}

func TestUnauthorizedAgentRejected(t *testing.T) {
	srv := newTestServer(t, Config{Regions: []string{"dfw1"}, AgentTokens: map[string]string{"dfw1": "secret"}})
	body, _ := json.Marshal(protocol.AgentPollRequest{ClusterID: "dfw1", Version: protocol.Version})
	req := httptest.NewRequest(http.MethodPost, "/api/agent/poll", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer wrong")
	resp := httptest.NewRecorder()
	srv.ServeHTTP(resp, req)
	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.Code)
	}
}

func TestConcurrencyLimits(t *testing.T) {
	tests := []struct {
		name   string
		cfg    Config
		first  string
		second string
		ip1    string
		ip2    string
	}{
		{
			name:   "global",
			cfg:    Config{GlobalConcurrencyLimit: 1, TargetConcurrencyLimit: 10, ClientConcurrencyLimit: 10},
			first:  "example.com",
			second: "cloudflare.com",
			ip1:    "198.51.100.1:1",
			ip2:    "198.51.100.2:1",
		},
		{
			name:   "target",
			cfg:    Config{GlobalConcurrencyLimit: 10, TargetConcurrencyLimit: 1, ClientConcurrencyLimit: 10},
			first:  "example.com",
			second: "example.com",
			ip1:    "198.51.100.1:1",
			ip2:    "198.51.100.2:1",
		},
		{
			name:   "client",
			cfg:    Config{GlobalConcurrencyLimit: 10, TargetConcurrencyLimit: 10, ClientConcurrencyLimit: 1},
			first:  "example.com",
			second: "cloudflare.com",
			ip1:    "198.51.100.1:1",
			ip2:    "198.51.100.1:2",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.cfg.AgentTokens = map[string]string{"dfw1": "secret"}
			test.cfg.AgentPollTimeout = 10 * time.Millisecond
			srv := newTestServer(t, test.cfg)
			registerAgent(t, srv)
			if code := createJob(t, srv, test.first, test.ip1); code != http.StatusAccepted {
				t.Fatalf("first create got %d", code)
			}
			if code := createJob(t, srv, test.second, test.ip2); code != http.StatusTooManyRequests {
				t.Fatalf("second create got %d, want 429", code)
			}
		})
	}
}

func TestCreateRateLimits(t *testing.T) {
	tests := []struct {
		name   string
		cfg    Config
		first  string
		second string
		ip1    string
		ip2    string
	}{
		{
			name:   "global",
			cfg:    Config{GlobalCreateRateLimit: 1, TargetCreateRateLimit: 10, ClientCreateRateLimit: 10, GlobalConcurrencyLimit: 10, TargetConcurrencyLimit: 10, ClientConcurrencyLimit: 10, CreateRateWindow: time.Hour},
			first:  "example.com",
			second: "cloudflare.com",
			ip1:    "198.51.100.1:1",
			ip2:    "198.51.100.2:1",
		},
		{
			name:   "target",
			cfg:    Config{GlobalCreateRateLimit: 10, TargetCreateRateLimit: 1, ClientCreateRateLimit: 10, GlobalConcurrencyLimit: 10, TargetConcurrencyLimit: 10, ClientConcurrencyLimit: 10, CreateRateWindow: time.Hour},
			first:  "example.com",
			second: "example.com",
			ip1:    "198.51.100.1:1",
			ip2:    "198.51.100.2:1",
		},
		{
			name:   "client",
			cfg:    Config{GlobalCreateRateLimit: 10, TargetCreateRateLimit: 10, ClientCreateRateLimit: 1, GlobalConcurrencyLimit: 10, TargetConcurrencyLimit: 10, ClientConcurrencyLimit: 10, CreateRateWindow: time.Hour},
			first:  "example.com",
			second: "cloudflare.com",
			ip1:    "198.51.100.1:1",
			ip2:    "198.51.100.1:2",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.cfg.AgentTokens = map[string]string{"dfw1": "secret"}
			test.cfg.AgentPollTimeout = 10 * time.Millisecond
			srv := newTestServer(t, test.cfg)
			registerAgent(t, srv)
			if code := createJob(t, srv, test.first, test.ip1); code != http.StatusAccepted {
				t.Fatalf("first create got %d", code)
			}
			if code := createJob(t, srv, test.second, test.ip2); code != http.StatusTooManyRequests {
				t.Fatalf("second create got %d, want 429", code)
			}
		})
	}
}

func registerAgent(t *testing.T, srv *Server) {
	t.Helper()
	body, _ := json.Marshal(protocol.AgentPollRequest{ClusterID: "dfw1", DisplayName: "DFW", Version: protocol.Version})
	req := httptest.NewRequest(http.MethodPost, "/api/agent/poll", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	srv.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("poll failed: %d %s", resp.Code, resp.Body.String())
	}
}

func createJob(t *testing.T, srv *Server, target, remoteAddr string) int {
	t.Helper()
	body, _ := json.Marshal(protocol.JobCreateRequest{Target: target})
	req := httptest.NewRequest(http.MethodPost, "/api/jobs", bytes.NewReader(body))
	req.RemoteAddr = remoteAddr
	resp := httptest.NewRecorder()
	srv.ServeHTTP(resp, req)
	return resp.Code
}

func newTestServer(t *testing.T, cfg Config) *Server {
	t.Helper()
	mr := miniredis.RunT(t)
	cfg.ValkeyURL = "redis://" + mr.Addr() + "/0"
	srv, err := NewServer(cfg, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = srv.Close()
	})
	return srv
}
