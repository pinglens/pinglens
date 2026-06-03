package protocol

import "time"

const Version = "v1"

type AgentPollRequest struct {
	ClusterID   string `json:"cluster_id"`
	InstanceID  string `json:"instance_id,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	Version     string `json:"version"`
}

type AgentPollResponse struct {
	Job *ProbeJob `json:"job,omitempty"`
}

type ProbeJob struct {
	JobID          string    `json:"job_id"`
	Target         string    `json:"target"`
	Count          int       `json:"count"`
	IntervalMillis int       `json:"interval_millis"`
	TimeoutMillis  int       `json:"timeout_millis"`
	DurationMillis int       `json:"duration_millis"`
	MTR            MTRConfig `json:"mtr"`
}

type MTRConfig struct {
	Cycles         int `json:"cycles"`
	MaxTTL         int `json:"max_ttl"`
	IntervalMillis int `json:"interval_millis"`
}

type JobCreateRequest struct {
	Target string `json:"target"`
}

type JobCreateResponse struct {
	JobID   string   `json:"job_id"`
	Target  string   `json:"target"`
	Regions []Region `json:"regions"`
}

type Region struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Online   bool      `json:"online"`
	LastSeen time.Time `json:"last_seen,omitempty"`
}

type RegionsResponse struct {
	Regions []Region `json:"regions"`
}

type AgentEvent struct {
	JobID      string       `json:"job_id"`
	ClusterID  string       `json:"cluster_id"`
	InstanceID string       `json:"instance_id,omitempty"`
	Type       string       `json:"type"`
	At         time.Time    `json:"at"`
	Target     string       `json:"target,omitempty"`
	Resolved   string       `json:"resolved,omitempty"`
	Ping       *PingResult  `json:"ping,omitempty"`
	Summary    *PingSummary `json:"summary,omitempty"`
	Trace      *TraceReport `json:"trace,omitempty"`
	Error      string       `json:"error,omitempty"`
}

type PingResult struct {
	Seq       int     `json:"seq"`
	Success   bool    `json:"success"`
	RTTMillis float64 `json:"rtt_ms,omitempty"`
	Error     string  `json:"error,omitempty"`
}

type PingSummary struct {
	Sent         int     `json:"sent"`
	Received     int     `json:"received"`
	LossPct      float64 `json:"loss_pct"`
	MinMillis    float64 `json:"min_ms,omitempty"`
	AvgMillis    float64 `json:"avg_ms,omitempty"`
	MaxMillis    float64 `json:"max_ms,omitempty"`
	StdDevMillis float64 `json:"stddev_ms,omitempty"`
}

type TraceReport struct {
	Rows  []TraceRow `json:"rows"`
	Error string     `json:"error,omitempty"`
}

type TraceRow struct {
	Hop         int     `json:"hop"`
	Host        string  `json:"host,omitempty"`
	Address     string  `json:"address,omitempty"`
	ASN         string  `json:"asn,omitempty"`
	ASName      string  `json:"as_name,omitempty"`
	LossPct     float64 `json:"loss_pct"`
	Sent        int     `json:"sent"`
	LastMillis  float64 `json:"last_ms,omitempty"`
	AvgMillis   float64 `json:"avg_ms,omitempty"`
	BestMillis  float64 `json:"best_ms,omitempty"`
	WorstMillis float64 `json:"worst_ms,omitempty"`
}
