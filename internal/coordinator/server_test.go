package coordinator

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
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
	if poll.Job == nil || poll.Job.Target != "example.com" || poll.Job.Count != 3 || poll.Job.DurationMillis != 3000 ||
		poll.Job.Trace != (protocol.TraceConfig{IntervalMillis: 1000, TimeoutMillis: 900, MaxTTL: 30, ReportMillis: 5000}) {
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

func TestAgentPollKeepsDisplayNameWhenReplicaOmitsIt(t *testing.T) {
	srv := newTestServer(t, Config{AgentTokens: map[string]string{"dfw1": "secret"}, AgentPollTimeout: 10 * time.Millisecond})

	namedBody, _ := json.Marshal(protocol.AgentPollRequest{ClusterID: "dfw1", DisplayName: "DFW (Dallas, US)", Version: protocol.Version})
	namedReq := httptest.NewRequest(http.MethodPost, "/api/agent/poll", bytes.NewReader(namedBody))
	namedReq.Header.Set("Authorization", "Bearer secret")
	namedReq.Header.Set("Content-Type", "application/json")
	namedResp := httptest.NewRecorder()
	srv.ServeHTTP(namedResp, namedReq)
	if namedResp.Code != http.StatusOK {
		t.Fatalf("named poll failed: %d %s", namedResp.Code, namedResp.Body.String())
	}

	unnamedBody, _ := json.Marshal(protocol.AgentPollRequest{ClusterID: "dfw1", Version: protocol.Version})
	unnamedReq := httptest.NewRequest(http.MethodPost, "/api/agent/poll", bytes.NewReader(unnamedBody))
	unnamedReq.Header.Set("Authorization", "Bearer secret")
	unnamedReq.Header.Set("Content-Type", "application/json")
	unnamedResp := httptest.NewRecorder()
	srv.ServeHTTP(unnamedResp, unnamedReq)
	if unnamedResp.Code != http.StatusOK {
		t.Fatalf("unnamed poll failed: %d %s", unnamedResp.Code, unnamedResp.Body.String())
	}

	regionsReq := httptest.NewRequest(http.MethodGet, "/api/regions", nil)
	regionsResp := httptest.NewRecorder()
	srv.ServeHTTP(regionsResp, regionsReq)
	if regionsResp.Code != http.StatusOK {
		t.Fatalf("regions failed: %d %s", regionsResp.Code, regionsResp.Body.String())
	}
	var regions protocol.RegionsResponse
	if err := json.Unmarshal(regionsResp.Body.Bytes(), &regions); err != nil {
		t.Fatal(err)
	}
	if len(regions.Regions) != 1 || regions.Regions[0].Name != "DFW (Dallas, US)" {
		t.Fatalf("unexpected regions: %+v", regions.Regions)
	}
}

func TestRegionsOnlyReturnsOnlineAgents(t *testing.T) {
	cfg := Config{AgentTokens: map[string]string{"dfw1": "secret"}, AgentOnlineTimeout: 50 * time.Millisecond}
	srv := newTestServer(t, cfg)
	ctx := context.Background()
	now := time.Now().UTC()
	if _, err := srv.store.registerAgent(ctx, cfg, "dfw1", "", "DFW (Dallas, US)", now); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.store.registerAgent(ctx, cfg, "ams1", "", "AMS (Amsterdam, NL)", now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}

	regionsReq := httptest.NewRequest(http.MethodGet, "/api/regions", nil)
	regionsResp := httptest.NewRecorder()
	srv.ServeHTTP(regionsResp, regionsReq)
	if regionsResp.Code != http.StatusOK {
		t.Fatalf("regions failed: %d %s", regionsResp.Code, regionsResp.Body.String())
	}
	var regions protocol.RegionsResponse
	if err := json.Unmarshal(regionsResp.Body.Bytes(), &regions); err != nil {
		t.Fatal(err)
	}
	if len(regions.Regions) != 1 || regions.Regions[0].ID != "dfw1" || !regions.Regions[0].Online {
		t.Fatalf("unexpected regions: %+v", regions.Regions)
	}
	stalePresent, err := srv.store.client.SIsMember(ctx, srv.store.key("agents"), "ams1").Result()
	if err != nil {
		t.Fatal(err)
	}
	if stalePresent {
		t.Fatal("stale agent remained in agents set")
	}
}

func TestCreateJobRequiresOnlineAgent(t *testing.T) {
	srv := newTestServer(t, Config{AgentTokens: map[string]string{"dfw1": "secret"}, AgentPollTimeout: 10 * time.Millisecond})

	body, _ := json.Marshal(protocol.JobCreateRequest{Target: "example.com"})
	req := httptest.NewRequest(http.MethodPost, "/api/jobs", bytes.NewReader(body))
	req.RemoteAddr = "198.51.100.11:12345"
	resp := httptest.NewRecorder()
	srv.ServeHTTP(resp, req)
	if resp.Code != http.StatusServiceUnavailable {
		t.Fatalf("create got %d %s, want 503", resp.Code, resp.Body.String())
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

func TestAgentConnectionLogsRejectedTokenHintWithoutSecret(t *testing.T) {
	var log bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&log, nil))
	srv := newTestServerWithLogger(t, Config{Regions: []string{"dfw1"}, AgentTokens: map[string]string{"dfw1": "secret"}}, logger)
	wrongToken := "wrong-agent-token"
	body, _ := json.Marshal(protocol.AgentPollRequest{ClusterID: "dfw1", DisplayName: "DFW", Version: protocol.Version})
	req := httptest.NewRequest(http.MethodPost, "/api/agent/poll", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+wrongToken)
	resp := httptest.NewRecorder()
	srv.ServeHTTP(resp, req)
	if resp.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.Code)
	}
	output := log.String()
	if !strings.Contains(output, "agent connection attempt") || !strings.Contains(output, "rejected agent connection") {
		t.Fatalf("missing agent connection logs: %s", output)
	}
	if strings.Contains(output, wrongToken) {
		t.Fatalf("log contains raw token: %s", output)
	}
	if !strings.Contains(output, "wron****oken") {
		t.Fatalf("log does not contain obfuscated token hint: %s", output)
	}
}

func TestAgentOnlineAndTimeoutLogged(t *testing.T) {
	var log bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&log, nil))
	srv := newTestServerWithLogger(t, Config{AgentTokens: map[string]string{"dfw1": "secret"}, AgentPollTimeout: time.Millisecond, AgentOnlineTimeout: 10 * time.Millisecond}, logger)
	body, _ := json.Marshal(protocol.AgentPollRequest{ClusterID: "dfw1", DisplayName: "DFW", Version: protocol.Version})
	req := httptest.NewRequest(http.MethodPost, "/api/agent/poll", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer secret")
	resp := httptest.NewRecorder()
	srv.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("poll failed: %d %s", resp.Code, resp.Body.String())
	}
	if !strings.Contains(log.String(), "agent online") {
		t.Fatalf("missing online log: %s", log.String())
	}
	time.Sleep(20 * time.Millisecond)
	regionsReq := httptest.NewRequest(http.MethodGet, "/api/regions", nil)
	regionsResp := httptest.NewRecorder()
	srv.ServeHTTP(regionsResp, regionsReq)
	if regionsResp.Code != http.StatusOK {
		t.Fatalf("regions failed: %d %s", regionsResp.Code, regionsResp.Body.String())
	}
	output := log.String()
	if !strings.Contains(output, "agent offline") || !strings.Contains(output, "reason=timeout") {
		t.Fatalf("missing timeout offline log: %s", output)
	}
}

func TestMetricsOnlyServedByMetricsHandler(t *testing.T) {
	srv := newTestServer(t, Config{AgentTokens: map[string]string{"dfw1": "secret"}, AgentPollTimeout: time.Millisecond})
	publicReq := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	publicResp := httptest.NewRecorder()
	srv.ServeHTTP(publicResp, publicReq)
	if publicResp.Code == http.StatusOK {
		t.Fatalf("public listener served metrics")
	}
	for _, instanceID := range []string{"pod-a", "pod-b"} {
		body, _ := json.Marshal(protocol.AgentPollRequest{ClusterID: "dfw1", InstanceID: instanceID, DisplayName: "DFW", Version: protocol.Version})
		req := httptest.NewRequest(http.MethodPost, "/api/agent/poll", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer secret")
		resp := httptest.NewRecorder()
		srv.ServeHTTP(resp, req)
		if resp.Code != http.StatusOK {
			t.Fatalf("poll failed: %d %s", resp.Code, resp.Body.String())
		}
	}
	if code := createJob(t, srv, "example.com", "198.51.100.55:12345"); code != http.StatusAccepted {
		t.Fatalf("create got %d", code)
	}
	metricsReq := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	metricsResp := httptest.NewRecorder()
	srv.MetricsHandler().ServeHTTP(metricsResp, metricsReq)
	if metricsResp.Code != http.StatusOK {
		t.Fatalf("metrics failed: %d %s", metricsResp.Code, metricsResp.Body.String())
	}
	output := metricsResp.Body.String()
	for _, want := range []string{
		"pinglens_agent_ids_online 1",
		"pinglens_agent_instances_online 2",
		"pinglens_agent_instances{agent_id=\"dfw1\"} 2",
		"pinglens_sessions_active 1",
		"pinglens_sessions_active_unique_clients 1",
		"pinglens_sessions_active_unique_targets 1",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("missing metric %q in:\n%s", want, output)
		}
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
	return newTestServerWithLogger(t, cfg, slog.Default())
}

func newTestServerWithLogger(t *testing.T, cfg Config, logger *slog.Logger) *Server {
	t.Helper()
	mr := miniredis.RunT(t)
	cfg.ValkeyURL = "redis://" + mr.Addr() + "/0"
	srv, err := NewServer(cfg, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = srv.Close()
	})
	return srv
}
