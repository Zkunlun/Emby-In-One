package backend

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// Phase 5 contractual check: one authenticated upstream login reused by two
// EIO accounts must not let account A populate account B's discovery cache.
// Cache snapshots are copied, not shared mutable maps, and expire for refresh.
func TestPhase5SNTPSourceCacheUserIsolationExpiryAndCopy(t *testing.T) {
	cache := &detailProbeCache{}
	base := detailProbeKey{
		UpstreamUserID: "shared-upstream-login",
		UpstreamItemID: "movie-1",
		ProxyUserID:    "account-a",
	}
	calls := 0
	fetched := func(name string) func() []map[string]any {
		return func() []map[string]any {
			calls++
			return []map[string]any{{
				"Id": name, "MediaStreams": []any{map[string]any{"Codec": "aac"}},
			}}
		}
	}
	first := cache.lookupOrFetch(context.Background(), base, fetched("a-cut"))
	if calls != 1 || len(first) != 1 {
		t.Fatalf("first probe should run once, calls=%d first=%v", calls, first)
	}
	first[0]["Id"] = "tampered"
	stream := first[0]["MediaStreams"].([]any)[0].(map[string]any)
	stream["Codec"] = "tampered"
	repeated := cache.lookupOrFetch(context.Background(), base, fetched("must-not-fetch"))
	if calls != 1 || repeated[0]["Id"] != "a-cut" {
		t.Fatalf("cached metadata leaked caller mutation or reprobed: calls=%d repeated=%v", calls, repeated)
	}
	cachedStream := repeated[0]["MediaStreams"].([]any)[0].(map[string]any)
	if cachedStream["Codec"] != "aac" {
		t.Fatalf("nested cache entry is mutable across callers: %v", cachedStream)
	}
	otherUser := base
	otherUser.ProxyUserID = "account-b"
	gotB := cache.lookupOrFetch(context.Background(), otherUser, fetched("b-cut"))
	if calls != 2 || gotB[0]["Id"] != "b-cut" {
		t.Fatalf("different EIO account must not share cached sources, calls=%d got=%v", calls, gotB)
	}
	otherItem := base
	otherItem.UpstreamItemID = "movie-2"
	if got := cache.lookupOrFetch(context.Background(), otherItem, fetched("second-item")); calls != 3 || got[0]["Id"] != "second-item" {
		t.Fatalf("different movie must have distinct cache key, calls=%d got=%v", calls, got)
	}
	cache.mu.Lock()
	entry := cache.entries[base]
	entry.Expires = time.Now().Add(-time.Second)
	cache.entries[base] = entry
	cache.mu.Unlock()
	refreshed := cache.lookupOrFetch(context.Background(), base, fetched("fresh-cut"))
	if calls != 4 || refreshed[0]["Id"] != "fresh-cut" {
		t.Fatalf("expired source list must be refreshed, calls=%d got=%v", calls, refreshed)
	}
}

// A client canceling while another request owns the upstream probe must return
// without waiting for the upstream or canceling the remaining client's probe.
func TestPhase5SNTPCacheCanceledFollowerDoesNotBlockOtherDetails(t *testing.T) {
	cache := &detailProbeCache{}
	key := detailProbeKey{ProxyUserID: "user", UpstreamItemID: "episode-123"}
	started := make(chan struct{})
	release := make(chan struct{})
	leaderDone := make(chan []map[string]any, 1)
	var called atomic.Int32
	go func() {
		leaderDone <- cache.lookupOrFetch(context.Background(), key, func() []map[string]any {
			called.Add(1)
			close(started)
			<-release
			return []map[string]any{{"Id": "source-1"}}
		})
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("leader never entered fetch")
	}
	canceled, cancel := context.WithCancel(context.Background())
	followerDone := make(chan []map[string]any, 1)
	go func() {
		followerDone <- cache.lookupOrFetch(canceled, key, func() []map[string]any {
			called.Add(1)
			return []map[string]any{{"Id": "unexpected"}}
		})
	}()
	cancel()
	select {
	case got := <-followerDone:
		if len(got) > 0 {
			t.Fatalf("canceled follower must not receive sources: %v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled follower blocked on upstream fetch")
	}
	close(release)
	select {
	case result := <-leaderDone:
		if len(result) != 1 || result[0]["Id"] != "source-1" {
			t.Fatalf("leader source lost after follower cancellation: %v", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("leader stalled after follower cancellation")
	}
	repeated := cache.lookupOrFetch(context.Background(), key, func() []map[string]any {
		called.Add(1)
		return nil
	})
	if called.Load() != 1 || len(repeated) != 1 {
		t.Fatalf("leader result should be shared, probes=%d cached=%v", called.Load(), repeated)
	}
}

// Failed metadata probes may have a short negative cache, but must be retried
// once its backoff expires rather than making a version permanently disappear.
func TestPhase5SNTPFailedProbeExpiresAndRetries(t *testing.T) {
	cache := &detailProbeCache{}
	key := detailProbeKey{UpstreamUserID: "up", UpstreamItemID: "movie", ProxyUserID: "test"}
	calls := 0
	failing := func() []map[string]any { calls++; return nil }
	if got := cache.lookupOrFetch(context.Background(), key, failing); len(got) > 0 {
		t.Fatalf("expected empty failed result: %v", got)
	}
	if got := cache.lookupOrFetch(context.Background(), key, failing); len(got) > 0 || calls != 1 {
		t.Fatalf("negative cache failed, sources=%v probes=%d", got, calls)
	}
	cache.mu.Lock()
	entry := cache.entries[key]
	if remaining := time.Until(entry.Expires); remaining <= 0 || remaining > 10*time.Second {
		cache.mu.Unlock()
		t.Fatalf("negative cache TTL should be short, got %v", remaining)
	}
	entry.Expires = time.Now().Add(-time.Second)
	cache.entries[key] = entry
	cache.mu.Unlock()
	result := cache.lookupOrFetch(context.Background(), key, func() []map[string]any {
		calls++
		return []map[string]any{{"Id": "recovered"}}
	})
	if len(result) != 1 || result[0]["Id"] != "recovered" || calls != 2 {
		t.Fatalf("expired negative cache didn't recover: result=%v calls=%d", result, calls)
	}
}

// Guard the cap in the short-lived per-app cache: a single account must not
// retain an unbounded number of distinct details after rapid browsing.
func TestPhase5SNTPCacheIsBounded(t *testing.T) {
	cache := &detailProbeCache{}
	for n := 0; n < 560; n++ {
		id := fmt.Sprintf("movie-%d", n)
		key := detailProbeKey{ProxyUserID: "test", UpstreamItemID: id}
		if got := cache.lookupOrFetch(context.Background(), key, func() []map[string]any {
			return []map[string]any{{"Id": id}}
		}); len(got) != 1 {
			t.Fatalf("failed to cache item %s", id)
		}
	}
	cache.mu.Lock()
	total := len(cache.entries)
	cache.mu.Unlock()
	if total > 512 {
		t.Fatalf("cache grows without bound: %d entries", total)
	}
}
