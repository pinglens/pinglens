package ping

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"net/netip"
	"os"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

type Result struct {
	Seq     int
	Success bool
	RTT     time.Duration
	Error   string
}

type Summary struct {
	Sent     int
	Received int
	LossPct  float64
	Min      time.Duration
	Avg      time.Duration
	Max      time.Duration
	StdDev   time.Duration
}

type Config struct {
	Count    int
	Interval time.Duration
	Timeout  time.Duration
}

func Run(ctx context.Context, addr netip.Addr, cfg Config, onResult func(Result)) Summary {
	if cfg.Count <= 0 {
		cfg.Count = 15
	}
	if cfg.Interval <= 0 {
		cfg.Interval = time.Second
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 3 * time.Second
	}

	pinger, err := open(addr)
	if err != nil {
		result := Result{Seq: 1, Error: err.Error()}
		if onResult != nil {
			onResult(result)
		}
		return Summarize([]Result{result})
	}
	defer pinger.Close()

	results := make([]Result, 0, cfg.Count)
	for seq := 1; seq <= cfg.Count; seq++ {
		if err := ctx.Err(); err != nil {
			result := Result{Seq: seq, Error: err.Error()}
			results = append(results, result)
			if onResult != nil {
				onResult(result)
			}
			break
		}
		result := pinger.probe(seq, cfg.Timeout)
		results = append(results, result)
		if onResult != nil {
			onResult(result)
		}
		if seq == cfg.Count {
			break
		}
		timer := time.NewTimer(cfg.Interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return Summarize(results)
		case <-timer.C:
		}
	}
	return Summarize(results)
}

func Summarize(results []Result) Summary {
	summary := Summary{Sent: len(results)}
	if len(results) == 0 {
		return summary
	}
	var total float64
	values := make([]float64, 0, len(results))
	for _, result := range results {
		if !result.Success {
			continue
		}
		summary.Received++
		rtt := result.RTT
		if summary.Min == 0 || rtt < summary.Min {
			summary.Min = rtt
		}
		if rtt > summary.Max {
			summary.Max = rtt
		}
		ms := float64(rtt) / float64(time.Millisecond)
		total += ms
		values = append(values, ms)
	}
	summary.LossPct = 100 * float64(summary.Sent-summary.Received) / float64(summary.Sent)
	if summary.Received == 0 {
		return summary
	}
	avgMillis := total / float64(summary.Received)
	summary.Avg = time.Duration(avgMillis * float64(time.Millisecond))
	if summary.Received > 1 {
		var variance float64
		for _, value := range values {
			delta := value - avgMillis
			variance += delta * delta
		}
		variance /= float64(summary.Received)
		summary.StdDev = time.Duration(math.Sqrt(variance) * float64(time.Millisecond))
	}
	return summary
}

type pinger struct {
	conn      *icmp.PacketConn
	addr      netip.Addr
	dst       *net.IPAddr
	protocol  int
	echoType  icmp.Type
	replyType icmp.Type
	id        int
}

func open(addr netip.Addr) (*pinger, error) {
	addr = addr.Unmap()
	var network string
	var listen string
	var protocol int
	var echoType icmp.Type
	var replyType icmp.Type
	if addr.Is4() {
		network = "ip4:icmp"
		listen = "0.0.0.0"
		protocol = 1
		echoType = ipv4.ICMPTypeEcho
		replyType = ipv4.ICMPTypeEchoReply
	} else if addr.Is6() {
		network = "ip6:ipv6-icmp"
		listen = "::"
		protocol = 58
		echoType = ipv6.ICMPTypeEchoRequest
		replyType = ipv6.ICMPTypeEchoReply
	} else {
		return nil, fmt.Errorf("unsupported address family")
	}
	conn, err := icmp.ListenPacket(network, listen)
	if err != nil {
		return nil, err
	}
	return &pinger{
		conn:      conn,
		addr:      addr,
		dst:       &net.IPAddr{IP: net.ParseIP(addr.String())},
		protocol:  protocol,
		echoType:  echoType,
		replyType: replyType,
		id:        os.Getpid() & 0xffff,
	}, nil
}

func (p *pinger) Close() error {
	return p.conn.Close()
}

func (p *pinger) probe(seq int, timeout time.Duration) Result {
	payload := []byte("pinglens")
	message := icmp.Message{
		Type: p.echoType,
		Code: 0,
		Body: &icmp.Echo{ID: p.id, Seq: seq, Data: payload},
	}
	data, err := message.Marshal(nil)
	if err != nil {
		return Result{Seq: seq, Error: err.Error()}
	}
	started := time.Now()
	if _, err := p.conn.WriteTo(data, p.dst); err != nil {
		return Result{Seq: seq, Error: err.Error()}
	}
	deadline := started.Add(timeout)
	buf := make([]byte, 1500)
	for {
		if err := p.conn.SetReadDeadline(deadline); err != nil {
			return Result{Seq: seq, Error: err.Error()}
		}
		n, _, err := p.conn.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				return Result{Seq: seq, Error: "timeout"}
			}
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				return Result{Seq: seq, Error: "timeout"}
			}
			return Result{Seq: seq, Error: err.Error()}
		}
		parsed, err := icmp.ParseMessage(p.protocol, buf[:n])
		if err != nil || parsed.Type != p.replyType {
			continue
		}
		echo, ok := parsed.Body.(*icmp.Echo)
		// The agent's trace engine shares the network namespace's ICMP
		// traffic; checking the payload keeps a trace echo reply whose ID and
		// sequence happen to match from counting as this ping.
		if !ok || echo.ID != p.id || echo.Seq != seq || !bytes.Equal(echo.Data, payload) {
			continue
		}
		return Result{Seq: seq, Success: true, RTT: time.Since(started)}
	}
}
