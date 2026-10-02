package backend

import (
	"testing"
	"time"
)

func TestPhase4FStopRequiresDeviceOwnershipAndCurrentSession(t *testing.T) {
	limiter := NewPlaybackLimiter()
	key := streamKey{UserID: "user-a", ServerID: "server-a"}

	first := limiter.Reserve("user-a", "server-a", "xbox-001", "item-a", "session-a")
	if !first.Allowed || !first.Created {
		t.Fatalf("initial reserve = %+v, want allowed+created", first)
	}
	if limiter.Stop("user-a", "server-a", "phone-001", "session-a") {
		t.Fatal("wrong device unexpectedly released Xbox lease")
	}
	limiter.mu.Lock()
	entry := limiter.streams[key]
	limiter.mu.Unlock()
	if entry == nil || entry.DeviceID != "xbox-001" || entry.PlaySessionID != "session-a" {
		t.Fatalf("lease changed after wrong-device stop: %+v", entry)
	}

	newer := limiter.Reserve("user-a", "server-a", "xbox-001", "item-b", "session-b")
	if !newer.Allowed || newer.Created {
		t.Fatalf("same-device newer reserve = %+v, want allowed+reused", newer)
	}
	if limiter.Stop("user-a", "server-a", "xbox-001", "session-a") {
		t.Fatal("stale PlaySessionID unexpectedly released newer lease")
	}
	limiter.mu.Lock()
	entry = limiter.streams[key]
	limiter.mu.Unlock()
	if entry == nil || entry.ItemID != "item-b" || entry.PlaySessionID != "session-b" {
		t.Fatalf("newer lease changed after stale-session stop: %+v", entry)
	}

	if !limiter.Stop("user-a", "server-a", "xbox-001", "session-b") {
		t.Fatal("current device/current session should release lease")
	}
	limiter.mu.Lock()
	entry = limiter.streams[key]
	limiter.mu.Unlock()
	if entry != nil {
		t.Fatalf("lease still present after valid stop: %+v", entry)
	}
}

func TestPhase4FStopRequiresSessionWhenCurrentLeaseHasOne(t *testing.T) {
	limiter := NewPlaybackLimiter()
	limiter.Reserve("user-a", "server-a", "xbox-001", "item-a", "session-a")
	if limiter.Stop("user-a", "server-a", "xbox-001", "") {
		t.Fatal("missing PlaySessionID unexpectedly released session-owned lease")
	}
}

func TestPhase4FStopCannotReviveOrClaimExpiredLease(t *testing.T) {
	limiter := NewPlaybackLimiter()
	key := streamKey{UserID: "user-a", ServerID: "server-a"}
	limiter.Reserve("user-a", "server-a", "xbox-001", "item-a", "session-a")

	limiter.mu.Lock()
	limiter.streams[key].LastHeartbeat = time.Now().Add(-playbackHeartbeatTimeout - time.Second)
	limiter.mu.Unlock()

	if limiter.Stop("user-a", "server-a", "xbox-001", "session-a") {
		t.Fatal("expired lease must not be reported as released by current stop")
	}
	limiter.mu.Lock()
	entry := limiter.streams[key]
	limiter.mu.Unlock()
	if entry != nil {
		t.Fatalf("expired lease still present after stop: %+v", entry)
	}
}
