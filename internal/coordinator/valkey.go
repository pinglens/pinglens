package coordinator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/pinglens/pinglens/internal/protocol"
	"github.com/pinglens/pinglens/internal/target"
)

const valkeyAdmissionScript = `
local job = ARGV[1]
local active_ttl = tonumber(ARGV[2])
local window = tonumber(ARGV[3])
local global_concurrency = tonumber(ARGV[4])
local client_concurrency = tonumber(ARGV[5])
local target_concurrency = tonumber(ARGV[6])
local global_rate = tonumber(ARGV[7])
local client_rate = tonumber(ARGV[8])
local target_rate = tonumber(ARGV[9])

if global_concurrency > 0 and tonumber(redis.call('SCARD', KEYS[1])) >= global_concurrency then
  return 'global active test limit reached'
end
if client_concurrency > 0 and tonumber(redis.call('SCARD', KEYS[2])) >= client_concurrency then
  return 'client active test limit reached'
end
if target_concurrency > 0 and tonumber(redis.call('SCARD', KEYS[3])) >= target_concurrency then
  return 'target active test limit reached'
end

local function check_rate(key, limit, message)
  if limit <= 0 then
    return nil
  end
  local current = tonumber(redis.call('GET', key) or '0')
  if current >= limit then
    return message
  end
  return nil
end

local blocked = check_rate(KEYS[4], global_rate, 'global test rate limit exceeded')
if blocked then return blocked end
blocked = check_rate(KEYS[5], client_rate, 'client test rate limit exceeded')
if blocked then return blocked end
blocked = check_rate(KEYS[6], target_rate, 'target test rate limit exceeded')
if blocked then return blocked end

local function hit_rate(key, limit)
  if limit <= 0 then
    return
  end
  local value = redis.call('INCR', key)
  if value == 1 then
    redis.call('PEXPIRE', key, window)
  end
end

redis.call('SADD', KEYS[1], job)
redis.call('SADD', KEYS[2], job)
redis.call('SADD', KEYS[3], job)
redis.call('PEXPIRE', KEYS[1], active_ttl)
redis.call('PEXPIRE', KEYS[2], active_ttl)
redis.call('PEXPIRE', KEYS[3], active_ttl)
hit_rate(KEYS[4], global_rate)
hit_rate(KEYS[5], client_rate)
hit_rate(KEYS[6], target_rate)
return ''
`

type valkeyStore struct {
	client *redis.Client
	prefix string
	logger *slog.Logger
}

func newValkeyStore(ctx context.Context, cfg Config, logger *slog.Logger) (*valkeyStore, error) {
	var client *redis.Client
	if strings.TrimSpace(cfg.ValkeyURL) != "" {
		options, err := redis.ParseURL(cfg.ValkeyURL)
		if err != nil {
			return nil, err
		}
		client = redis.NewClient(options)
	} else if len(cfg.ValkeySentinelAddrs) != 0 {
		client = redis.NewFailoverClient(&redis.FailoverOptions{
			MasterName:    cfg.ValkeySentinelMaster,
			SentinelAddrs: cfg.ValkeySentinelAddrs,
			Password:      cfg.ValkeyPassword,
		})
	} else {
		return nil, nil
	}
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, err
	}
	prefix := strings.Trim(strings.TrimSpace(cfg.ValkeyPrefix), ":")
	if prefix == "" {
		prefix = "pinglens"
	}
	return &valkeyStore{client: client, prefix: prefix, logger: logger}, nil
}

func (v *valkeyStore) close() error {
	if v == nil || v.client == nil {
		return nil
	}
	return v.client.Close()
}

func (v *valkeyStore) key(parts ...string) string {
	out := v.prefix
	for _, part := range parts {
		out += ":" + part
	}
	return out
}

func (v *valkeyStore) hashed(kind, value string) string {
	sum := sha256.Sum256([]byte(value))
	return v.key(kind, hex.EncodeToString(sum[:]))
}

func (v *valkeyStore) jobTTL(cfg Config) time.Duration {
	ttl := cfg.JobRetention + cfg.JobDuration + time.Minute
	if ttl < time.Minute {
		return time.Minute
	}
	return ttl
}

func (v *valkeyStore) registerAgent(ctx context.Context, id string, name string, now time.Time) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("agent id is required")
	}
	pipe := v.client.Pipeline()
	pipe.SAdd(ctx, v.key("agents"), id)
	pipe.HSet(ctx, v.key("agent", id), "name", strings.TrimSpace(name), "last_seen_ms", now.UnixMilli())
	pipe.Expire(ctx, v.key("agent", id), 24*time.Hour)
	_, err := pipe.Exec(ctx)
	return err
}

func (v *valkeyStore) regionStatuses(ctx context.Context, cfg Config, now time.Time) ([]protocol.Region, error) {
	ids, err := v.agentIDs(ctx, cfg)
	if err != nil {
		return nil, err
	}
	regions := make([]protocol.Region, 0, len(ids))
	for _, id := range ids {
		fields, err := v.client.HGetAll(ctx, v.key("agent", id)).Result()
		if err != nil {
			return nil, err
		}
		region := protocol.Region{ID: id, Name: id}
		if name := strings.TrimSpace(fields["name"]); name != "" {
			region.Name = name
		}
		lastSeenMillis, _ := strconv.ParseInt(fields["last_seen_ms"], 10, 64)
		if lastSeenMillis > 0 {
			region.LastSeen = time.UnixMilli(lastSeenMillis).UTC()
			region.Online = now.Sub(region.LastSeen) <= cfg.AgentOnlineTimeout
		}
		regions = append(regions, region)
	}
	return regions, nil
}

func (v *valkeyStore) agentIDs(ctx context.Context, cfg Config) ([]string, error) {
	seen := map[string]struct{}{}
	ids := make([]string, 0, len(cfg.Regions)+len(cfg.AgentTokens))
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
	for _, id := range cfg.Regions {
		add(id)
	}
	tokenIDs := make([]string, 0, len(cfg.AgentTokens))
	for id := range cfg.AgentTokens {
		tokenIDs = append(tokenIDs, id)
	}
	sort.Strings(tokenIDs)
	for _, id := range tokenIDs {
		add(id)
	}
	stored, err := v.client.SMembers(ctx, v.key("agents")).Result()
	if err != nil {
		return nil, err
	}
	sort.Strings(stored)
	for _, id := range stored {
		add(id)
	}
	return ids, nil
}

func (v *valkeyStore) releaseActiveSets(ctx context.Context, jobID, client, target string) {
	pipe := v.client.Pipeline()
	pipe.SRem(ctx, v.key("active", "global"), jobID)
	pipe.SRem(ctx, v.hashed("active:client", client), jobID)
	pipe.SRem(ctx, v.hashed("active:target", target), jobID)
	_, _ = pipe.Exec(ctx)
}

func (v *valkeyStore) admit(ctx context.Context, cfg Config, jobID, client, target string) (string, error) {
	activeTTL := v.jobTTL(cfg)
	keys := []string{
		v.key("active", "global"),
		v.hashed("active:client", client),
		v.hashed("active:target", target),
		v.key("rate", "global"),
		v.hashed("rate:client", client),
		v.hashed("rate:target", target),
	}
	args := []any{
		jobID,
		int(activeTTL / time.Millisecond),
		int(cfg.CreateRateWindow / time.Millisecond),
		cfg.GlobalConcurrencyLimit,
		cfg.ClientConcurrencyLimit,
		cfg.TargetConcurrencyLimit,
		cfg.GlobalCreateRateLimit,
		cfg.ClientCreateRateLimit,
		cfg.TargetCreateRateLimit,
	}
	result, err := v.client.Eval(ctx, valkeyAdmissionScript, keys, args...).Text()
	if errors.Is(err, redis.Nil) {
		return "", nil
	}
	return result, err
}

func (v *valkeyStore) storeJob(ctx context.Context, cfg Config, job protocol.ProbeJob, client string, expected int, now time.Time) error {
	ttl := v.jobTTL(cfg)
	key := v.key("job", job.JobID)
	pipe := v.client.Pipeline()
	pipe.HSet(ctx, key,
		"target", job.Target,
		"client", client,
		"created_ms", now.UnixMilli(),
		"expected", expected,
		"canceled", 0,
		"completed", 0,
	)
	pipe.Expire(ctx, key, ttl)
	pipe.Expire(ctx, v.key("events", job.JobID), ttl)
	pipe.Expire(ctx, v.key("terminal", job.JobID), ttl)
	_, err := pipe.Exec(ctx)
	return err
}

func (v *valkeyStore) jobStatus(ctx context.Context, jobID string) (exists bool, canceled bool, completed bool, err error) {
	fields, err := v.client.HGetAll(ctx, v.key("job", jobID)).Result()
	if err != nil {
		return false, false, false, err
	}
	if len(fields) == 0 {
		return false, false, false, nil
	}
	return true, fields["canceled"] == "1", fields["completed"] == "1", nil
}

func (v *valkeyStore) appendEvent(ctx context.Context, cfg Config, event protocol.AgentEvent) error {
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	ttl := v.jobTTL(cfg)
	pipe := v.client.Pipeline()
	pipe.RPush(ctx, v.key("events", event.JobID), data)
	pipe.Expire(ctx, v.key("events", event.JobID), ttl)
	_, err = pipe.Exec(ctx)
	if err != nil {
		return err
	}
	if terminalEvent(event.Type) && event.ClusterID != "" {
		if err := v.client.SAdd(ctx, v.key("terminal", event.JobID), event.ClusterID).Err(); err != nil {
			return err
		}
		expected, err := v.expected(ctx, event.JobID)
		if err != nil {
			return err
		}
		count, err := v.client.SCard(ctx, v.key("terminal", event.JobID)).Result()
		if err != nil {
			return err
		}
		if expected > 0 && count >= int64(expected) {
			return v.finishJob(ctx, event.JobID)
		}
	}
	return nil
}

func (v *valkeyStore) expected(ctx context.Context, jobID string) (int, error) {
	value, err := v.client.HGet(ctx, v.key("job", jobID), "expected").Int()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	return value, err
}

func (v *valkeyStore) finishJob(ctx context.Context, jobID string) error {
	fields, err := v.client.HGetAll(ctx, v.key("job", jobID)).Result()
	if err != nil || len(fields) == 0 {
		return err
	}
	if fields["completed"] == "1" {
		return nil
	}
	pipe := v.client.Pipeline()
	pipe.HSet(ctx, v.key("job", jobID), "completed", 1)
	pipe.SRem(ctx, v.key("active", "global"), jobID)
	pipe.SRem(ctx, v.hashed("active:client", fields["client"]), jobID)
	pipe.SRem(ctx, v.hashed("active:target", fields["target"]), jobID)
	_, err = pipe.Exec(ctx)
	if err == nil && v.logger != nil && fields["canceled"] != "1" {
		v.logger.Info("completed probe job", "job", jobID, "target", fields["target"], "backend", "valkey")
	}
	return err
}

func (v *valkeyStore) cancelJob(ctx context.Context, cfg Config, jobID, reason string) error {
	exists, canceled, completed, err := v.jobStatus(ctx, jobID)
	if err != nil {
		return err
	}
	if !exists {
		return redis.Nil
	}
	if canceled || completed {
		return nil
	}
	if reason == "" {
		reason = "job canceled"
	}
	if err := v.client.HSet(ctx, v.key("job", jobID), "canceled", 1).Err(); err != nil {
		return err
	}
	fields, _ := v.client.HGetAll(ctx, v.key("job", jobID)).Result()
	if v.logger != nil {
		v.logger.Info("canceled probe job", "job", jobID, "target", fields["target"], "reason", reason, "backend", "valkey")
	}
	_ = v.appendEvent(ctx, cfg, protocol.AgentEvent{JobID: jobID, Type: "canceled", At: time.Now().UTC(), Target: fields["target"], Error: reason})
	return v.finishJob(ctx, jobID)
}

func (v *valkeyStore) unsubscribe(ctx context.Context, cfg Config, jobID string) {
	key := v.key("subscribers", jobID)
	remaining, err := v.client.Decr(ctx, key).Result()
	if err != nil || remaining > 0 {
		return
	}
	_ = v.client.Del(ctx, key).Err()
	time.AfterFunc(subscriberCancelGrace, func() {
		checkCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		count, err := v.client.Get(checkCtx, key).Int64()
		if err == nil && count > 0 {
			return
		}
		_ = v.cancelJob(checkCtx, cfg, jobID, "browser disconnected")
	})
}

func (s *Server) regionsValkey(w http.ResponseWriter, r *http.Request) {
	regions, err := s.store.regionStatuses(r.Context(), s.cfg, time.Now())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, protocol.RegionsResponse{Regions: regions})
}

func (s *Server) createJobValkey(w http.ResponseWriter, r *http.Request) {
	var request protocol.JobCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	client := clientKey(r)
	normalized, err := target.Normalize(request.Target)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	id, err := randomID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	job := s.probeJob(id, normalized)
	now := time.Now().UTC()
	ctx := r.Context()
	regions, err := s.store.regionStatuses(ctx, s.cfg, now)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	reason, err := s.store.admit(ctx, s.cfg, id, client, normalized)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if reason != "" {
		s.logger.Warn("rejected probe job", "target", normalized, "client", client, "reason", reason, "backend", "valkey")
		writeError(w, http.StatusTooManyRequests, reason)
		return
	}
	if err := s.store.storeJob(ctx, s.cfg, job, client, len(regions), now); err != nil {
		s.store.releaseActiveSets(context.Background(), id, client, normalized)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, region := range regions {
		if !region.Online {
			_ = s.store.appendEvent(ctx, s.cfg, protocol.AgentEvent{JobID: id, ClusterID: region.ID, Type: "offline", At: now, Target: normalized, Error: "agent offline"})
			continue
		}
		data, err := json.Marshal(job)
		if err != nil {
			s.store.releaseActiveSets(context.Background(), id, client, normalized)
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if err := s.store.client.RPush(ctx, s.store.key("queue", region.ID), data).Err(); err != nil {
			s.store.releaseActiveSets(context.Background(), id, client, normalized)
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	s.mu.Lock()
	s.jobsCreated++
	s.mu.Unlock()
	s.logger.Info("created probe job", "job", id, "target", normalized, "client", client, "regions", len(regions), "backend", "valkey")
	writeJSON(w, http.StatusAccepted, protocol.JobCreateResponse{JobID: id, Target: normalized, Regions: regions})
}

func (s *Server) agentPollValkey(w http.ResponseWriter, r *http.Request, request protocol.AgentPollRequest) {
	ctx := r.Context()
	if err := s.store.registerAgent(ctx, request.ClusterID, request.DisplayName, time.Now().UTC()); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	result, err := s.store.client.BRPop(ctx, s.cfg.AgentPollTimeout, s.store.key("queue", request.ClusterID)).Result()
	if errors.Is(err, redis.Nil) {
		writeJSON(w, http.StatusOK, protocol.AgentPollResponse{})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(result) != 2 {
		writeJSON(w, http.StatusOK, protocol.AgentPollResponse{})
		return
	}
	var job protocol.ProbeJob
	if err := json.Unmarshal([]byte(result[1]), &job); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	exists, canceled, completed, err := s.store.jobStatus(ctx, job.JobID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !exists || canceled || completed {
		writeJSON(w, http.StatusOK, protocol.AgentPollResponse{})
		return
	}
	writeJSON(w, http.StatusOK, protocol.AgentPollResponse{Job: &job})
}

func (s *Server) agentEventsValkey(w http.ResponseWriter, r *http.Request, event protocol.AgentEvent) {
	ctx := r.Context()
	_ = s.store.registerAgent(ctx, event.ClusterID, "", time.Now().UTC())
	exists, canceled, _, err := s.store.jobStatus(ctx, event.JobID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !exists {
		writeError(w, http.StatusNotFound, "job not found")
		return
	}
	if canceled {
		writeError(w, http.StatusGone, "job canceled")
		return
	}
	if err := s.store.appendEvent(ctx, s.cfg, event); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if event.Type == "start" {
		s.logger.Info("probe job started", "job", event.JobID, "target", event.Target, "agent", event.ClusterID, "backend", "valkey")
	}
	s.mu.Lock()
	s.eventsSeen++
	s.mu.Unlock()
	writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
}

func (s *Server) jobEventsValkey(w http.ResponseWriter, r *http.Request, id string, eventsPath bool) {
	if !eventsPath {
		if r.Method != http.MethodDelete {
			methodNotAllowed(w)
			return
		}
		if err := s.store.cancelJob(r.Context(), s.cfg, id, "client disconnected"); err != nil {
			if errors.Is(err, redis.Nil) {
				writeError(w, http.StatusNotFound, "job not found")
				return
			}
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	exists, _, _, err := s.store.jobStatus(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !exists {
		writeError(w, http.StatusNotFound, "job not found")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	ctx := r.Context()
	subKey := s.store.key("subscribers", id)
	if err := s.store.client.Incr(ctx, subKey).Err(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = s.store.client.Expire(ctx, subKey, s.cfg.JobRetention).Err()
	defer s.store.unsubscribe(context.Background(), s.cfg, id)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Connection", "keep-alive")
	var index int64
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		events, err := s.store.client.LRange(ctx, s.store.key("events", id), index, -1).Result()
		if err != nil {
			return
		}
		for _, raw := range events {
			var event protocol.AgentEvent
			if json.Unmarshal([]byte(raw), &event) == nil {
				writeSSE(w, event)
			}
		}
		index += int64(len(events))
		flusher.Flush()
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_, _ = w.Write([]byte(": keepalive\n\n"))
			flusher.Flush()
		}
	}
}
