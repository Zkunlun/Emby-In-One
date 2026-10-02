package backend

import (
	"testing"
	"time"
)

func TestPhase4EHeartbeatRequiresLeaseDeviceOwnership(t *testing.T) {
	limiter := NewPlaybackLimiter()
	key := streamKey{UserID: "user-a", ServerID: "server-a"}

	reserved := limiter.Reserve("user-a", "server-a", "xbox-001", "item-a", "session-a")
	if !reserved.Allowed || !reserved.Created {
		t.Fatalf("initial reserve = %+v, want Allowed=true Created=true", reserved)
	}

	oldHeartbeat := time.Now().Add(-time.Minute).Round(0)
	limiter.mu.Lock()
	limiter.streams[key].LastHeartbeat = oldHeartbeat
	limiter.mu.Unlock()

	if limiter.Heartbeat("user-a", "server-a", "phone-001") {
		t.Fatal("wrong device heartbeat unexpectedly succeeded")
	}
	limiter.mu.Lock()
	gotAfterWrongDevice := limiter.streams[key].LastHeartbeat
	limiter.mu.Unlock()
	if !gotAfterWrongDevice.Equal(oldHeartbeat) {
		t.Fatalf("wrong device refreshed heartbeat: got=%v want=%v", gotAfterWrongDevice, oldHeartbeat)
	}

	if !limiter.Heartbeat("user-a", "server-a", "xbox-001") {
		t.Fatal("owning device heartbeat should succeed")
	}
	limiter.mu.Lock()
	gotAfterOwner := limiter.streams[key].LastHeartbeat
	limiter.mu.Unlock()
	if !gotAfterOwner.After(oldHeartbeat) {
		t.Fatalf("owner heartbeat was not refreshed: got=%v old=%v", gotAfterOwner, oldHeartbeat)
	}
}

func TestPhase4EHeartbeatCannotReviveExpiredLease(t *testing.T) {
	limiter := NewPlaybackLimiter()
	key := streamKey{UserID: "user-a", ServerID: "server-a"}
	if result := limiter.Reserve("user-a", "server-a", "xbox-001", "item-a", "session-a"); !result.Allowed {
		t.Fatalf("initial reserve = %+v, want allowed", result)
	}

	limiter.mu.Lock()
	limiter.streams[key].LastHeartbeat = time.Now().Add(-playbackHeartbeatTimeout - time.Second)
	limiter.mu.Unlock()

	if limiter.Heartbeat("user-a", "server-a", "xbox-001") {
		t.Fatal("expired lease heartbeat unexpectedly succeeded")
	}
	limiter.mu.Lock()
	_, exists := limiter.streams[key]
	limiter.mu.Unlock()
	if exists {
		t.Fatal("expired lease remained after heartbeat attempt")
	}
}
