package main

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

func TestPinglensModeDefaultsToCoordinator(t *testing.T) {
	t.Setenv("PINGLENS_MODE", "")
	t.Setenv("PINGLENS_AGENT_ID", "")
	t.Setenv("PINGLENS_COORDINATOR_URL", "")

	if got := pinglensMode(); got != "coordinator" {
		t.Fatalf("pinglensMode() = %q, want coordinator", got)
	}
}

func TestPinglensModeInfersAgentFromAgentConfig(t *testing.T) {
	t.Setenv("PINGLENS_MODE", "")
	t.Setenv("PINGLENS_AGENT_ID", "dfw1")
	t.Setenv("PINGLENS_COORDINATOR_URL", "http://pinglens-agent:8080")

	if got := pinglensMode(); got != "agent" {
		t.Fatalf("pinglensMode() = %q, want agent", got)
	}
}

func TestPinglensModeRequiresCompleteAgentConfigForInference(t *testing.T) {
	t.Setenv("PINGLENS_MODE", "")
	t.Setenv("PINGLENS_AGENT_ID", "dfw1")
	t.Setenv("PINGLENS_COORDINATOR_URL", "")

	if got := pinglensMode(); got != "coordinator" {
		t.Fatalf("pinglensMode() = %q, want coordinator", got)
	}
}

func TestPinglensModeHonorsExplicitAgentMode(t *testing.T) {
	t.Setenv("PINGLENS_MODE", " agent ")
	t.Setenv("PINGLENS_AGENT_ID", "")
	t.Setenv("PINGLENS_COORDINATOR_URL", "")

	if got := pinglensMode(); got != "agent" {
		t.Fatalf("pinglensMode() = %q, want agent", got)
	}
}

func TestPinglensModeHonorsExplicitAgentModeWithValkeyConfig(t *testing.T) {
	t.Setenv("PINGLENS_MODE", "agent")
	t.Setenv("PINGLENS_AGENT_ID", "dfw1")
	t.Setenv("PINGLENS_COORDINATOR_URL", "http://pinglens-agent:8080")
	t.Setenv("PINGLENS_VALKEY_URL", "redis://pinglens-valkey:6379/0")

	if got := pinglensMode(); got != "agent" {
		t.Fatalf("pinglensMode() = %q, want agent", got)
	}
}

func TestPinglensModeHonorsExplicitCoordinatorMode(t *testing.T) {
	t.Setenv("PINGLENS_MODE", "coordinator")
	t.Setenv("PINGLENS_AGENT_ID", "dfw1")
	t.Setenv("PINGLENS_COORDINATOR_URL", "http://pinglens-agent:8080")

	if got := pinglensMode(); got != "coordinator" {
		t.Fatalf("pinglensMode() = %q, want coordinator", got)
	}
}

func TestRunCoordinatorRequiresValkeyConfig(t *testing.T) {
	t.Setenv("PINGLENS_LISTEN", "127.0.0.1:0")
	t.Setenv("PINGLENS_VALKEY_URL", "")
	t.Setenv("PINGLENS_VALKEY_SENTINEL_ADDRS", "")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := runCoordinator(ctx, slog.Default())
	if err == nil || !strings.Contains(err.Error(), "valkey configuration is required") {
		t.Fatalf("runCoordinator() error = %v, want valkey configuration error", err)
	}
}

func TestRunCoordinatorStartsWithValkeyConfig(t *testing.T) {
	mr := miniredis.RunT(t)
	t.Setenv("PINGLENS_LISTEN", "127.0.0.1:0")
	t.Setenv("PINGLENS_VALKEY_URL", "redis://"+mr.Addr()+"/0")
	t.Setenv("PINGLENS_VALKEY_SENTINEL_ADDRS", "")

	ctx, cancel := context.WithCancel(context.Background())
	timer := time.AfterFunc(10*time.Millisecond, cancel)
	defer timer.Stop()

	if err := runCoordinator(ctx, slog.Default()); err != nil {
		t.Fatalf("runCoordinator() error = %v, want nil", err)
	}
}
