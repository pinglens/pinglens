package coordinator

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/pinglens/pinglens/internal/protocol"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

type Config struct {
	ListenAddr             string
	Regions                []string
	AgentTokens            map[string]string
	UI                     http.Handler
	PingCount              int
	PingInterval           time.Duration
	PingTimeout            time.Duration
	JobDuration            time.Duration
	MTRCycles              int
	MTRMaxTTL              int
	MTRInterval            time.Duration
	AgentPollTimeout       time.Duration
	AgentOnlineTimeout     time.Duration
	JobRetention           time.Duration
	GlobalCreateRateLimit  int
	TargetCreateRateLimit  int
	ClientCreateRateLimit  int
	CreateRateWindow       time.Duration
	GlobalConcurrencyLimit int
	TargetConcurrencyLimit int
	ClientConcurrencyLimit int
	MaxQueuedJobsPerAgent  int
	ValkeyURL              string
	ValkeySentinelAddrs    []string
	ValkeySentinelMaster   string
	ValkeyPassword         string
	ValkeyPrefix           string
}

type Server struct {
	cfg    Config
	logger *slog.Logger
	mux    *http.ServeMux
	store  *valkeyStore

	mu      sync.Mutex
	agents  map[string]*agentState
	jobs    map[string]*jobState
	buckets map[string]*rateBucket

	jobsCreated int64
	eventsSeen  int64
}

type agentState struct {
	id          string
	queue       chan protocol.ProbeJob
	lastSeen    time.Time
	displayName string
}

type jobState struct {
	id          string
	target      string
	client      string
	created     time.Time
	events      []protocol.AgentEvent
	subscribers map[chan protocol.AgentEvent]struct{}
	canceled    bool
	cancelTimer *time.Timer
	completed   bool
	expected    int
	terminal    map[string]struct{}
}

type rateBucket struct {
	reset time.Time
	count int
}

const subscriberCancelGrace = 5 * time.Second

func New(cfg Config, logger *slog.Logger) *Server {
	srv, err := NewServer(cfg, logger)
	if err != nil {
		panic(err)
	}
	return srv
}

func NewServer(cfg Config, logger *slog.Logger) (*Server, error) {
	cfg = normalizeConfig(cfg)
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{cfg: cfg, logger: logger, mux: http.NewServeMux()}
	store, err := newValkeyStore(context.Background(), cfg, logger)
	if err != nil {
		return nil, err
	}
	if store == nil {
		return nil, fmt.Errorf("valkey configuration is required")
	}
	s.store = store
	logger.Info("using valkey coordinator backend")
	s.routes()
	return s, nil
}

func normalizeConfig(cfg Config) Config {
	if cfg.PingInterval <= 0 {
		cfg.PingInterval = time.Second
	}
	if cfg.JobDuration <= 0 {
		cfg.JobDuration = 10 * time.Minute
	}
	if cfg.PingCount <= 0 {
		cfg.PingCount = int(cfg.JobDuration / cfg.PingInterval)
		if cfg.PingCount <= 0 {
			cfg.PingCount = 1
		}
	}
	if cfg.PingTimeout <= 0 {
		cfg.PingTimeout = 3 * time.Second
	}
	if cfg.MTRCycles <= 0 {
		cfg.MTRCycles = 10
	}
	if cfg.MTRMaxTTL <= 0 {
		cfg.MTRMaxTTL = 30
	}
	if cfg.MTRInterval <= 0 {
		cfg.MTRInterval = 5 * time.Second
	}
	if cfg.AgentPollTimeout <= 0 {
		cfg.AgentPollTimeout = 30 * time.Second
	}
	if cfg.AgentOnlineTimeout <= 0 {
		cfg.AgentOnlineTimeout = 45 * time.Second
	}
	if cfg.JobRetention <= 0 {
		cfg.JobRetention = 15 * time.Minute
	}
	if cfg.GlobalCreateRateLimit <= 0 {
		cfg.GlobalCreateRateLimit = 120
	}
	if cfg.TargetCreateRateLimit <= 0 {
		cfg.TargetCreateRateLimit = 30
	}
	if cfg.ClientCreateRateLimit <= 0 {
		cfg.ClientCreateRateLimit = 20
	}
	if cfg.CreateRateWindow <= 0 {
		cfg.CreateRateWindow = time.Minute
	}
	if cfg.GlobalConcurrencyLimit <= 0 {
		cfg.GlobalConcurrencyLimit = 100
	}
	if cfg.TargetConcurrencyLimit <= 0 {
		cfg.TargetConcurrencyLimit = 25
	}
	if cfg.ClientConcurrencyLimit <= 0 {
		cfg.ClientConcurrencyLimit = 10
	}
	if cfg.MaxQueuedJobsPerAgent <= 0 {
		cfg.MaxQueuedJobsPerAgent = 16
	}
	if cfg.ValkeySentinelMaster == "" {
		cfg.ValkeySentinelMaster = "myprimary"
	}
	return cfg
}

func (s *Server) Close() error {
	if s.store != nil {
		return s.store.close()
	}
	return nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	s.mux.HandleFunc("/healthz", s.health)
	s.mux.HandleFunc("/metrics", s.metrics)
	s.mux.HandleFunc("/api/regions", s.regions)
	s.mux.HandleFunc("/api/jobs", s.createJob)
	s.mux.HandleFunc("/api/jobs/", s.jobEvents)
	s.mux.HandleFunc("/api/agent/poll", s.agentPoll)
	s.mux.HandleFunc("/api/agent/events", s.agentEvents)
	if s.cfg.UI != nil {
		s.mux.Handle("/", s.cfg.UI)
	}
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

func (s *Server) metrics(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	agents := len(s.agents)
	jobsCreated := s.jobsCreated
	eventsSeen := s.eventsSeen
	s.mu.Unlock()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	_, _ = fmt.Fprintf(w, "pinglens_agents %d\npinglens_jobs_created_total %d\npinglens_events_total %d\n", agents, jobsCreated, eventsSeen)
}

func (s *Server) regions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	s.regionsValkey(w, r)
}

func (s *Server) createJob(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	s.createJobValkey(w, r)
}

func (s *Server) probeJob(id, normalizedTarget string) protocol.ProbeJob {
	return protocol.ProbeJob{JobID: id, Target: normalizedTarget, Count: s.cfg.PingCount, IntervalMillis: int(s.cfg.PingInterval / time.Millisecond), TimeoutMillis: int(s.cfg.PingTimeout / time.Millisecond), DurationMillis: int(s.cfg.JobDuration / time.Millisecond), MTR: protocol.MTRConfig{Cycles: s.cfg.MTRCycles, MaxTTL: s.cfg.MTRMaxTTL, IntervalMillis: int(s.cfg.MTRInterval / time.Millisecond)}}
}

func (s *Server) jobEvents(w http.ResponseWriter, r *http.Request) {
	id, eventsPath := parseJobPath(r.URL.Path)
	if id == "" {
		writeError(w, http.StatusNotFound, "job not found")
		return
	}
	s.jobEventsValkey(w, r, id, eventsPath)
}

func parseJobPath(path string) (string, bool) {
	rest := strings.TrimPrefix(path, "/api/jobs/")
	if rest == path || rest == "" {
		return "", false
	}
	if id, ok := strings.CutSuffix(rest, "/events"); ok {
		if id == "" || strings.Contains(id, "/") {
			return "", false
		}
		return id, true
	}
	if strings.Contains(rest, "/") {
		return "", false
	}
	return rest, false
}

func (s *Server) agentPoll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var request protocol.AgentPollRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	auth := s.authorizeAgent(request.ClusterID, r.Header.Get("Authorization"))
	s.logAgentPollAttempt(r, request, auth)
	if !auth.ok {
		s.logRejectedAgentConnection(r, "poll", request.ClusterID, "", auth)
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	s.agentPollValkey(w, r, request)
}

func (s *Server) agentEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	var event protocol.AgentEvent
	if err := json.NewDecoder(r.Body).Decode(&event); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	auth := s.authorizeAgent(event.ClusterID, r.Header.Get("Authorization"))
	if !auth.ok {
		s.logRejectedAgentConnection(r, "events", event.ClusterID, event.JobID, auth)
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if event.At.IsZero() {
		event.At = time.Now().UTC()
	}
	s.agentEventsValkey(w, r, event)
}
func (s *Server) regionStatuses(now time.Time) []protocol.Region {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.regionStatusesLocked(now)
}

func (s *Server) regionStatusesLocked(now time.Time) []protocol.Region {
	ids := s.agentIDsLocked()
	regions := make([]protocol.Region, 0, len(ids))
	for _, id := range ids {
		state := s.agents[id]
		region := protocol.Region{ID: id, Name: id}
		if state != nil {
			if state.displayName != "" {
				region.Name = state.displayName
			}
			region.LastSeen = state.lastSeen
			region.Online = now.Sub(state.lastSeen) <= s.cfg.AgentOnlineTimeout
		}
		regions = append(regions, region)
	}
	return regions
}

func (s *Server) agentIDsLocked() []string {
	seen := make(map[string]struct{}, len(s.cfg.Regions)+len(s.cfg.AgentTokens)+len(s.agents))
	ids := make([]string, 0, len(seen))
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	for _, id := range s.cfg.Regions {
		add(id)
	}
	tokenIDs := make([]string, 0, len(s.cfg.AgentTokens))
	for id := range s.cfg.AgentTokens {
		tokenIDs = append(tokenIDs, id)
	}
	sort.Strings(tokenIDs)
	for _, id := range tokenIDs {
		add(id)
	}
	agentIDs := make([]string, 0, len(s.agents))
	for id := range s.agents {
		agentIDs = append(agentIDs, id)
	}
	sort.Strings(agentIDs)
	for _, id := range agentIDs {
		add(id)
	}
	return ids
}

func (s *Server) ensureAgent(clusterID, displayName string, now time.Time) *agentState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ensureAgentLocked(clusterID, displayName, now)
}

func (s *Server) ensureAgentLocked(clusterID, displayName string, now time.Time) *agentState {
	state := s.agents[clusterID]
	if state == nil {
		state = &agentState{id: clusterID, queue: make(chan protocol.ProbeJob, s.cfg.MaxQueuedJobsPerAgent)}
		s.agents[clusterID] = state
	}
	if displayName = strings.TrimSpace(displayName); displayName != "" {
		state.displayName = displayName
	}
	state.lastSeen = now
	return state
}

func (s *Server) appendEventLocked(job *jobState, event protocol.AgentEvent) {
	job.events = append(job.events, event)
	if terminalEvent(event.Type) && event.ClusterID != "" {
		job.terminal[event.ClusterID] = struct{}{}
		if job.expected > 0 && len(job.terminal) >= job.expected && !job.completed {
			job.completed = true
			s.logger.Info("completed probe job", "job", job.id, "target", job.target, "backend", "memory")
		}
	}
	for subscriber := range job.subscribers {
		select {
		case subscriber <- event:
		default:
		}
	}
}

type agentAuthorization struct {
	ok        bool
	reason    string
	tokenHint string
}

func (s *Server) authorizeAgent(clusterID, authorization string) agentAuthorization {
	clusterID = strings.TrimSpace(clusterID)
	if clusterID == "" {
		return agentAuthorization{reason: "missing agent id"}
	}
	token := s.cfg.AgentTokens[clusterID]
	if token == "" {
		return agentAuthorization{reason: "unknown agent"}
	}
	supplied := strings.TrimPrefix(authorization, "Bearer ")
	if supplied == authorization || supplied == "" {
		return agentAuthorization{reason: "missing bearer token"}
	}
	if subtle.ConstantTimeCompare([]byte(token), []byte(supplied)) != 1 {
		return agentAuthorization{reason: "invalid token", tokenHint: obfuscateSecret(supplied)}
	}
	return agentAuthorization{ok: true}
}

func (s *Server) logAgentPollAttempt(r *http.Request, request protocol.AgentPollRequest, auth agentAuthorization) {
	attrs := []any{
		"agent", strings.TrimSpace(request.ClusterID),
		"display_name", strings.TrimSpace(request.DisplayName),
		"remote", clientKey(r),
		"authenticated", auth.ok,
	}
	if auth.reason != "" {
		attrs = append(attrs, "reason", auth.reason)
	}
	if auth.tokenHint != "" {
		attrs = append(attrs, "token_hint", auth.tokenHint)
	}
	s.logger.Info("agent connection attempt", attrs...)
}

func (s *Server) logRejectedAgentConnection(r *http.Request, endpoint, agentID, jobID string, auth agentAuthorization) {
	attrs := []any{
		"endpoint", endpoint,
		"agent", strings.TrimSpace(agentID),
		"remote", clientKey(r),
		"reason", auth.reason,
	}
	if jobID != "" {
		attrs = append(attrs, "job", jobID)
	}
	if auth.tokenHint != "" {
		attrs = append(attrs, "token_hint", auth.tokenHint)
	}
	s.logger.Warn("rejected agent connection", attrs...)
}

func (s *Server) logAgentRegistration(registration agentRegistration, source string) {
	if registration.id == "" {
		return
	}
	attrs := []any{
		"agent", registration.id,
		"display_name", registration.displayName,
		"source", source,
		"backend", "valkey",
	}
	if !registration.previousLastSeen.IsZero() {
		attrs = append(attrs, "previous_last_seen", registration.previousLastSeen)
	}
	if registration.offlineTransition {
		offlineAttrs := append([]any{}, attrs...)
		offlineAttrs = append(offlineAttrs, "reason", "timeout")
		s.logger.Warn("agent offline", offlineAttrs...)
	}
	if registration.onlineTransition {
		s.logger.Info("agent online", attrs...)
	}
}

func obfuscateSecret(secret string) string {
	secret = strings.TrimSpace(secret)
	if len(secret) <= 8 {
		return "****"
	}
	return secret[:4] + "****" + secret[len(secret)-4:]
}

func terminalEvent(eventType string) bool {
	switch eventType {
	case "complete", "offline", "busy", "error":
		return true
	default:
		return false
	}
}

func (s *Server) admissionFailureLocked(client, target string, now time.Time) string {
	globalActive, clientActive, targetActive := s.activeJobCountsLocked(client, target)
	if globalActive >= s.cfg.GlobalConcurrencyLimit {
		return "global active test limit reached"
	}
	if clientActive >= s.cfg.ClientConcurrencyLimit {
		return "client active test limit reached"
	}
	if targetActive >= s.cfg.TargetConcurrencyLimit {
		return "target active test limit reached"
	}
	if !s.allowCreateLocked("global", s.cfg.GlobalCreateRateLimit, now) {
		return "global test rate limit exceeded"
	}
	if !s.allowCreateLocked("client:"+client, s.cfg.ClientCreateRateLimit, now) {
		return "client test rate limit exceeded"
	}
	if !s.allowCreateLocked("target:"+target, s.cfg.TargetCreateRateLimit, now) {
		return "target test rate limit exceeded"
	}
	return ""
}

func (s *Server) activeJobCountsLocked(client, target string) (int, int, int) {
	var globalActive int
	var clientActive int
	var targetActive int
	for _, job := range s.jobs {
		if job.canceled || job.completed {
			continue
		}
		globalActive++
		if job.client == client {
			clientActive++
		}
		if job.target == target {
			targetActive++
		}
	}
	return globalActive, clientActive, targetActive
}

func (s *Server) allowCreateLocked(key string, limit int, now time.Time) bool {
	if limit <= 0 {
		return true
	}
	bucket := s.buckets[key]
	if bucket == nil || now.After(bucket.reset) {
		s.buckets[key] = &rateBucket{reset: now.Add(s.cfg.CreateRateWindow), count: 1}
		return true
	}
	if bucket.count >= limit {
		return false
	}
	bucket.count++
	return true
}

func (s *Server) pruneLocked(now time.Time) {
	for id, job := range s.jobs {
		if now.Sub(job.created) > s.cfg.JobRetention {
			if job.cancelTimer != nil {
				job.cancelTimer.Stop()
			}
			delete(s.jobs, id)
		}
	}
}

func writeSSE(w http.ResponseWriter, event protocol.AgentEvent) {
	data, _ := json.Marshal(event)
	_, _ = w.Write([]byte("event: "))
	_, _ = w.Write([]byte(event.Type))
	_, _ = w.Write([]byte("\n"))
	_, _ = w.Write([]byte("data: "))
	_, _ = w.Write(data)
	_, _ = w.Write([]byte("\n\n"))
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if value != nil {
		_ = json.NewEncoder(w).Encode(value)
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func methodNotAllowed(w http.ResponseWriter) {
	writeError(w, http.StatusMethodNotAllowed, "method not allowed")
}

func randomID() (string, error) {
	var data [8]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(data[:]), nil
}

func clientKey(r *http.Request) string {
	forwarded := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-For"), ",")[0])
	if forwarded != "" {
		return forwarded
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

func SortedRegions(regions []string) []string {
	out := append([]string(nil), regions...)
	sort.Strings(out)
	return out
}
