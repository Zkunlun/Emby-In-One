package backend

import (
	"testing"
	"time"
)

func TestPhase4HPlaybackLeaseExpiryBoundaryIsExact(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

	justLive := &streamEntry{LastHeartbeat: now.Add(-playbackHeartbeatTimeout + time.Nanosecond)}
	if playbackLeaseExpired(now, justLive) {
		t.Fatal("lease younger than timeout must remain live")
	}

	exactBoundary := &streamEntry{LastHeartbeat: now.Add(-playbackHeartbeatTimeout)}
	if !playbackLeaseExpired(now, exactBoundary) {
		t.Fatal("lease exactly at timeout must be stale")
	}

	older := &streamEntry{LastHeartbeat: now.Add(-playbackHeartbeatTimeout - time.Nanosecond)}
	if !playbackLeaseExpired(now, older) {
		t.Fatal("lease older than timeout must be stale")
	}
}

func TestPhase4HCountForServerPrunesExpiredLeases(t *testing.T) {
	limiter := NewPlaybackLimiter()
	now := time.Now()

	limiter.mu.Lock()
	limiter.streams[streamKey{UserID: "live-user", ServerID: "server-a"}] = &streamEntry{
		DeviceID:      "xbox-001",
		PlaySessionID: "session-live",
		LastHeartbeat: now,
	}
	staleKey := streamKey{UserID: "stale-user", ServerID: "server-b"}
	limiter.streams[staleKey] = &streamEntry{
		DeviceID:      "phone-001",
		PlaySessionID: "session-stale",
		LastHeartbeat: now.Add(-playbackHeartbeatTimeout - time.Second),
	}
	limiter.mu.Unlock()

	if got := limiter.CountForServer("server-a"); got != 1 {
		t.Fatalf("CountForServer(server-a) = %d, want 1", got)
	}

	limiter.mu.Lock()
	_, staleStillPresent := limiter.streams[staleKey]
	limiter.mu.Unlock()
	if staleStillPresent {
		t.Fatal("CountForServer must prune stale leases, including stale entries on other servers")
	}
}
