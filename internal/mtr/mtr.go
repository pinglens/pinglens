package mtr

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/netip"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/pinglens/pinglens/internal/protocol"
	"github.com/pinglens/pinglens/internal/target"
)

type Config struct {
	Cycles      int
	MaxTTL      int
	Timeout     time.Duration
	OutputLimit int64
}

type TimeoutError struct {
	Timeout time.Duration
}

func (e TimeoutError) Error() string {
	return fmt.Sprintf("MTR timed out after %s", e.Timeout)
}

func DefaultConfig() Config {
	return Config{Cycles: 10, MaxTTL: 30, Timeout: 55 * time.Second, OutputLimit: 256 * 1024}
}

func BuildArgs(target string, cfg Config) []string {
	if cfg.Cycles <= 0 {
		cfg.Cycles = 10
	}
	if cfg.MaxTTL <= 0 {
		cfg.MaxTTL = 30
	}
	return []string{
		"--report",
		"--json",
		"--show-ips",
		"--report-cycles", strconv.Itoa(cfg.Cycles),
		"--max-ttl", strconv.Itoa(cfg.MaxTTL),
		target,
	}
}

func Run(ctx context.Context, binary string, target string, cfg Config) (protocol.TraceReport, error) {
	if binary == "" {
		binary = "mtr"
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultConfig().Timeout
	}
	if cfg.OutputLimit <= 0 {
		cfg.OutputLimit = DefaultConfig().OutputLimit
	}
	runCtx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, binary, BuildArgs(target, cfg)...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return protocol.TraceReport{}, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return protocol.TraceReport{}, err
	}
	limited := io.LimitReader(stdout, cfg.OutputLimit+1)
	data, readErr := io.ReadAll(limited)
	waitErr := cmd.Wait()
	if readErr != nil {
		if err := mtrContextError(ctx, runCtx, cfg.Timeout); err != nil {
			return protocol.TraceReport{}, err
		}
		return protocol.TraceReport{}, readErr
	}
	if int64(len(data)) > cfg.OutputLimit {
		return protocol.TraceReport{}, fmt.Errorf("mtr output exceeded %d bytes", cfg.OutputLimit)
	}
	if waitErr != nil {
		if err := mtrContextError(ctx, runCtx, cfg.Timeout); err != nil {
			return protocol.TraceReport{}, err
		}
		if stderr.Len() > 0 {
			return protocol.TraceReport{}, fmt.Errorf("%w: %s", waitErr, stderr.String())
		}
		return protocol.TraceReport{}, waitErr
	}
	report, err := ParseJSON(data)
	if err != nil {
		return protocol.TraceReport{}, err
	}
	enrichASN(runCtx, nil, &report)
	return report, nil
}

func mtrContextError(parent context.Context, runCtx context.Context, timeout time.Duration) error {
	if parent.Err() != nil {
		return parent.Err()
	}
	if runCtx.Err() != nil {
		return TimeoutError{Timeout: timeout}
	}
	return nil
}

func ParseJSON(data []byte) (protocol.TraceReport, error) {
	var raw struct {
		Report struct {
			Hubs []map[string]any `json:"hubs"`
		} `json:"report"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return protocol.TraceReport{}, err
	}
	rows := make([]protocol.TraceRow, 0, len(raw.Report.Hubs))
	for i, hub := range raw.Report.Hubs {
		hop := intNumber(first(hub, "count", "Count"))
		if hop == 0 {
			hop = i + 1
		}
		rawHost := stringValue(first(hub, "host", "Host"))
		host, address := splitHostAddress(rawHost)
		row := protocol.TraceRow{
			Hop:         hop,
			Host:        host,
			Address:     address,
			LossPct:     floatNumber(first(hub, "Loss%", "loss", "Loss")),
			Sent:        intNumber(first(hub, "Snt", "sent", "Sent")),
			LastMillis:  finite(floatNumber(first(hub, "Last", "last"))),
			AvgMillis:   finite(floatNumber(first(hub, "Avg", "avg"))),
			BestMillis:  finite(floatNumber(first(hub, "Best", "best"))),
			WorstMillis: finite(floatNumber(first(hub, "Wrst", "Worst", "worst"))),
		}
		rows = append(rows, row)
	}
	return protocol.TraceReport{Rows: rows}, nil
}

func splitHostAddress(value string) (string, string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", ""
	}
	if addr, err := netip.ParseAddr(strings.Trim(value, "[]")); err == nil {
		return "", addr.String()
	}
	open := strings.LastIndexByte(value, '(')
	close := strings.LastIndexByte(value, ')')
	if open >= 0 && close > open {
		candidate := strings.TrimSpace(value[open+1 : close])
		if addr, err := netip.ParseAddr(strings.Trim(candidate, "[]")); err == nil {
			return strings.TrimSpace(value[:open]), addr.String()
		}
	}
	parts := strings.Fields(value)
	for i := len(parts) - 1; i >= 0; i-- {
		candidate := strings.Trim(parts[i], "[]()")
		if addr, err := netip.ParseAddr(candidate); err == nil {
			host := strings.TrimSpace(strings.Replace(value, parts[i], "", 1))
			return host, addr.String()
		}
	}
	return value, ""
}

func enrichASN(ctx context.Context, resolver *net.Resolver, report *protocol.TraceReport) {
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	for i := range report.Rows {
		if report.Rows[i].Address == "" {
			continue
		}
		addr, err := netip.ParseAddr(report.Rows[i].Address)
		if err != nil || !target.AddressAllowed(addr) {
			continue
		}
		asn, name := lookupASN(ctx, resolver, addr)
		report.Rows[i].ASN = asn
		report.Rows[i].ASName = name
	}
}

func lookupASN(ctx context.Context, resolver *net.Resolver, addr netip.Addr) (string, string) {
	query := asnQuery(addr)
	if query == "" {
		return "", ""
	}
	txts, err := resolver.LookupTXT(ctx, query)
	if err != nil {
		return "", ""
	}
	for _, txt := range txts {
		parts := strings.Split(txt, "|")
		if len(parts) < 1 {
			continue
		}
		asn := strings.TrimSpace(parts[0])
		if asn == "" {
			continue
		}
		name := ""
		if len(parts) >= 7 {
			name = strings.TrimSpace(parts[6])
		}
		return "AS" + asn, name
	}
	return "", ""
}

func asnQuery(addr netip.Addr) string {
	addr = addr.Unmap()
	if addr.Is4() {
		octets := addr.As4()
		return fmt.Sprintf("%d.%d.%d.%d.origin.asn.cymru.com", octets[3], octets[2], octets[1], octets[0])
	}
	if addr.Is6() {
		bytes := addr.As16()
		var b strings.Builder
		for i := len(bytes) - 1; i >= 0; i-- {
			lo := bytes[i] & 0x0f
			hi := bytes[i] >> 4
			_, _ = fmt.Fprintf(&b, "%x.%x.", lo, hi)
		}
		b.WriteString("origin6.asn.cymru.com")
		return b.String()
	}
	return ""
}

func first(values map[string]any, keys ...string) any {
	for _, key := range keys {
		if value, ok := values[key]; ok {
			return value
		}
	}
	return nil
}

func stringValue(value any) string {
	switch typed := value.(type) {
	case string:
		if typed == "???" {
			return ""
		}
		return typed
	default:
		return ""
	}
}

func intNumber(value any) int {
	switch typed := value.(type) {
	case float64:
		return int(typed)
	case string:
		parsed, _ := strconv.Atoi(typed)
		return parsed
	default:
		return 0
	}
}

func floatNumber(value any) float64 {
	switch typed := value.(type) {
	case float64:
		return typed
	case string:
		parsed, _ := strconv.ParseFloat(typed, 64)
		return parsed
	default:
		return 0
	}
}

func finite(value float64) float64 {
	if math.IsInf(value, 0) || math.IsNaN(value) {
		return 0
	}
	return value
}
