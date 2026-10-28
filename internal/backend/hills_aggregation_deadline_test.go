package backend

import (
	"context"
	"testing"
	"time"
)

// The aggregator's global timeout is a return-time bound even if one
// upstream adapter accidentally ignores context cancellation.
func TestAggregationGlobalDeadlineBoundsUnresponsiveTask(t *testing.T) {
	task := upstreamTask{index: 0, fn: func(ctx context.Context) upstreamItemsResult {
		time.Sleep(400 * time.Millisecond)
		return upstreamItemsResult{Items: []map[string]any{{"Id": "late"}}}
	}}
	began := time.Now()
	got := (&App{}).aggregateUpstreams(context.Background(), aggregationConfig{globalTimeout: 70 * time.Millisecond}, []upstreamTask{task})
	if d := time.Since(began); d > 280*time.Millisecond {
		t.Fatalf("global timeout did not bound aggregator latency: %v", d)
	}
	if len(got) != 0 {
		t.Fatalf("late result leaked after deadline: %#v", got)
	}
}
