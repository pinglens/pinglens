package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"sync/atomic"
	"time"

	"github.com/pinglens/pinglens/internal/mtr"
	"github.com/pinglens/pinglens/internal/ping"
	"github.com/pinglens/pinglens/internal/protocol"
	"github.com/pinglens/pinglens/internal/target"
)

type Config struct {
	CoordinatorURL string
	ClusterID      string
	DisplayName    string
	Token          string
	MTRBinary      string
	UserAgent      string
	PollBackoff    time.Duration
}

var errJobNotFound = errors.New("coordinator no longer has this job")
var errJobCanceled = errors.New("coordinator canceled this job")

func Run(ctx context.Context, cfg Config, logger *slog.Logger) error {
	if cfg.PollBackoff <= 0 {
		cfg.PollBackoff = 2 * time.Second
	}
	client := &http.Client{Timeout: 40 * time.Second}
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		job, err := poll(ctx, client, cfg)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			logger.Warn("agent poll failed", "err", err)
			sleep(ctx, cfg.PollBackoff)
			continue
		}
		if job == nil {
			continue
		}
		go func(job protocol.ProbeJob) {
			logger.Info("starting probe job", "job", job.JobID, "target", job.Target)
			executeJob(ctx, client, cfg, job, logger)
		}(*job)
	}
}

func poll(ctx context.Context, client *http.Client, cfg Config) (*protocol.ProbeJob, error) {
	body, err := json.Marshal(protocol.AgentPollRequest{ClusterID: cfg.ClusterID, DisplayName: cfg.DisplayName, Version: protocol.Version})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.CoordinatorURL+"/api/agent/poll", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	authorize(req, cfg)
	var response protocol.AgentPollResponse
	if err := roundTripJSON(client, req, &response); err != nil {
		return nil, err
	}
	return response.Job, nil
}

func executeJob(ctx context.Context, client *http.Client, cfg Config, job protocol.ProbeJob, logger *slog.Logger) {
	jobCtx, cancel := context.WithTimeout(ctx, jobDuration(job))
	defer cancel()
	var orphaned atomic.Bool

	event := func(event protocol.AgentEvent) bool {
		event.JobID = job.JobID
		event.ClusterID = cfg.ClusterID
		if event.At.IsZero() {
			event.At = time.Now().UTC()
		}
		if err := postEvent(ctx, client, cfg, event); err != nil {
			if errors.Is(err, errJobCanceled) {
				logger.Info("coordinator canceled probe job", "job", job.JobID)
				orphaned.Store(true)
				cancel()
				return false
			}
			if errors.Is(err, errJobNotFound) {
				logger.Warn("coordinator no longer has job; stopping probe", "job", job.JobID)
				orphaned.Store(true)
				cancel()
				return false
			}
			logger.Warn("failed to post event", "job", job.JobID, "err", err)
		}
		return true
	}

	addrs, err := target.ResolveAllowed(jobCtx, nil, job.Target)
	if err != nil {
		event(protocol.AgentEvent{Type: "error", Target: job.Target, Error: err.Error()})
		return
	}
	resolved := chooseAddress(addrs)
	resolvedString := resolved.String()
	if !event(protocol.AgentEvent{Type: "start", Target: job.Target, Resolved: resolvedString}) {
		return
	}

	traceDone := make(chan struct{})
	go func() {
		defer close(traceDone)
		traceLoop(jobCtx, cfg, job, resolvedString, event)
	}()

	results := make([]ping.Result, 0, job.Count)
	summary := ping.Run(jobCtx, resolved, ping.Config{Count: job.Count, Interval: time.Duration(job.IntervalMillis) * time.Millisecond, Timeout: time.Duration(job.TimeoutMillis) * time.Millisecond}, func(result ping.Result) {
		results = append(results, result)
		if !event(protocol.AgentEvent{Type: "ping", Target: job.Target, Resolved: resolvedString, Ping: &protocol.PingResult{Seq: result.Seq, Success: result.Success, RTTMillis: millis(result.RTT), Error: result.Error}}) {
			return
		}
		protocolSummary := summaryEvent(ping.Summarize(results))
		event(protocol.AgentEvent{Type: "summary", Target: job.Target, Resolved: resolvedString, Summary: &protocolSummary})
	})
	if orphaned.Load() {
		return
	}
	protocolSummary := summaryEvent(summary)
	event(protocol.AgentEvent{Type: "summary", Target: job.Target, Resolved: resolvedString, Summary: &protocolSummary})

	if orphaned.Load() {
		return
	}
	select {
	case <-ctx.Done():
	case <-traceDone:
	}
	if !orphaned.Load() {
		event(protocol.AgentEvent{Type: "complete", Target: job.Target, Resolved: resolvedString})
		logger.Info("completed probe job", "job", job.JobID)
	}
}

func postEvent(ctx context.Context, client *http.Client, cfg Config, event protocol.AgentEvent) error {
	body, err := json.Marshal(event)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.CoordinatorURL+"/api/agent/events", bytes.NewReader(body))
	if err != nil {
		return err
	}
	authorize(req, cfg)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusGone {
		return errJobCanceled
	}
	if resp.StatusCode == http.StatusNotFound {
		return errJobNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%s", resp.Status)
	}
	return nil
}

func authorize(req *http.Request, cfg Config) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	if cfg.UserAgent != "" {
		req.Header.Set("User-Agent", cfg.UserAgent)
	}
}

func roundTripJSON(client *http.Client, req *http.Request, output any) error {
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("%s", resp.Status)
	}
	if output == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(output)
}

func chooseAddress(addrs []netip.Addr) netip.Addr {
	for _, addr := range addrs {
		if addr.Is4() {
			return addr
		}
	}
	return addrs[0]
}

func traceLoop(ctx context.Context, cfg Config, job protocol.ProbeJob, resolved string, event func(protocol.AgentEvent) bool) {
	interval := time.Duration(job.MTR.IntervalMillis) * time.Millisecond
	if interval <= 0 {
		interval = 5 * time.Second
	}
	for {
		report, err := mtr.Run(ctx, cfg.MTRBinary, resolved, mtr.Config{Cycles: job.MTR.Cycles, MaxTTL: job.MTR.MaxTTL, Timeout: mtrTimeout(job), OutputLimit: 256 * 1024})
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			report.Error = err.Error()
		}
		if !event(protocol.AgentEvent{Type: "trace", Target: job.Target, Resolved: resolved, Trace: &report}) || ctx.Err() != nil {
			return
		}
		sleep(ctx, interval)
	}
}

func jobDuration(job protocol.ProbeJob) time.Duration {
	duration := time.Duration(job.DurationMillis) * time.Millisecond
	if duration <= 0 {
		return 10 * time.Minute
	}
	return duration
}

func summaryEvent(summary ping.Summary) protocol.PingSummary {
	return protocol.PingSummary{
		Sent:         summary.Sent,
		Received:     summary.Received,
		LossPct:      summary.LossPct,
		MinMillis:    millis(summary.Min),
		AvgMillis:    millis(summary.Avg),
		MaxMillis:    millis(summary.Max),
		StdDevMillis: millis(summary.StdDev),
	}
}

func mtrTimeout(job protocol.ProbeJob) time.Duration {
	cycles := job.MTR.Cycles
	if cycles <= 0 {
		cycles = 10
	}
	return time.Duration(cycles+10) * time.Second
}

func millis(duration time.Duration) float64 {
	if duration <= 0 {
		return 0
	}
	return float64(duration) / float64(time.Millisecond)
}

func sleep(ctx context.Context, duration time.Duration) {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}
