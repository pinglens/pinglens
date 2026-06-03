package ping

import (
	"testing"
	"time"
)

func TestSummarize(t *testing.T) {
	summary := Summarize([]Result{
		{Seq: 1, Success: true, RTT: 10 * time.Millisecond},
		{Seq: 2, Success: false, Error: "timeout"},
		{Seq: 3, Success: true, RTT: 20 * time.Millisecond},
	})
	if summary.Sent != 3 || summary.Received != 2 {
		t.Fatalf("unexpected counts: %+v", summary)
	}
	if summary.LossPct < 33 || summary.LossPct > 34 {
		t.Fatalf("unexpected loss: %v", summary.LossPct)
	}
	if summary.Min != 10*time.Millisecond || summary.Max != 20*time.Millisecond || summary.Avg != 15*time.Millisecond {
		t.Fatalf("unexpected latency summary: %+v", summary)
	}
}
