package backend

import (
	"testing"
	"time"
)

func TestPhase4KPlaybackLimiterCoreContract(t *testing.T) {
	t.Run("first reserve creates lease", func(t *testing.T) {
		limiter := NewPlaybackLimiter()
		got := limiter.Reserve("user-a", "server-a", "xbox-001", "item-a", "session-a")
		if !got.Allowed || !got.Created {
			t.Fatalf("first reserve = %+v, want allowed+created", got)
		}
	})

	t.Run("same device reuses lease", func(t *testing.T) {
		limiter := NewPlaybackLimiter()
		limiter.Reserve("user-a", "server-a", "xbox-001", "item-a", "session-a")
		got := limiter.Reserve("user-a", "server-a", "xbox-001", "item-b", "session-b")
		if !got.Allowed || got.Created {
			t.Fatalf("same-device reserve = %+v, want allowed+reused", got)
		}
	})

	t.Run("second device same user and server is rejected", func(t *testing.T) {
		limiter := NewPlaybackLimiter()
		limiter.Reserve("user-a", "server-a", "xbox-001", "item-a", "session-a")
		got := limiter.Reserve("user-a", "server-a", "phone-001", "item-b", "session-b")
		if got.Allowed || got.Created {
			t.Fatalf("different-device reserve = %+v, want denied", got)
		}
	})

	t.Run("stale lease permits device takeover", func(t *testing.T) {
		limiter := NewPlaybackLimiter()
		key := streamKey{UserID: "user-a", ServerID: "server-a"}
		limiter.Reserve("user-a", "server-a", "xbox-001", "item-a", "session-a")
		limiter.mu.Lock()
		limiter.streams[key].LastHeartbeat = time.Now().Add(-playbackHeartbeatTimeout - time.Second)
		limiter.mu.Unlock()

		got := limiter.Reserve("user-a", "server-a", "phone-001", "item-b", "session-b")
		if !got.Allowed || !got.Created {
			t.Fatalf("stale takeover = %+v, want allowed+created", got)
		}
	})

	t.Run("same user leases are independent per upstream", func(t *testing.T) {
		limiter := NewPlaybackLimiter()
		first := limiter.Reserve("user-a", "server-a", "xbox-001", "item-a", "session-a")
		second := limiter.Reserve("user-a", "server-b", "phone-001", "item-b", "session-b")
		if !first.Allowed || !second.Allowed {
			t.Fatalf("per-upstream reserves: first=%+v second=%+v, want both allowed", first, second)
		}
	})

	t.Run("wrong device heartbeat cannot refresh", func(t *testing.T) {
		limiter := NewPlaybackLimiter()
		key := streamKey{UserID: "user-a", ServerID: "server-a"}
		limiter.Reserve("user-a", "server-a", "xbox-001", "item-a", "session-a")
		old := time.Now().Add(-time.Minute).Round(0)
		limiter.mu.Lock()
		limiter.streams[key].LastHeartbeat = old
		limiter.mu.Unlock()

		if limiter.Heartbeat("user-a", "server-a", "phone-001") {
			t.Fatal("wrong-device heartbeat unexpectedly succeeded")
		}
		limiter.mu.Lock()
		got := limiter.streams[key].LastHeartbeat
		limiter.mu.Unlock()
		if !got.Equal(old) {
			t.Fatalf("wrong-device heartbeat changed timestamp: got=%v want=%v", got, old)
		}
	})

	t.Run("owning device heartbeat refreshes", func(t *testing.T) {
		limiter := NewPlaybackLimiter()
		key := streamKey{UserID: "user-a", ServerID: "server-a"}
		limiter.Reserve("user-a", "server-a", "xbox-001", "item-a", "session-a")
		old := time.Now().Add(-time.Minute).Round(0)
		limiter.mu.Lock()
		limiter.streams[key].LastHeartbeat = old
		limiter.mu.Unlock()

		if !limiter.Heartbeat("user-a", "server-a", "xbox-001") {
			t.Fatal("owner heartbeat unexpectedly failed")
		}
		limiter.mu.Lock()
		got := limiter.streams[key].LastHeartbeat
		limiter.mu.Unlock()
		if !got.After(old) {
			t.Fatalf("owner heartbeat did not refresh: got=%v old=%v", got, old)
		}
	})

	t.Run("wrong device stop cannot release", func(t *testing.T) {
		limiter := NewPlaybackLimiter()
		key := streamKey{UserID: "user-a", ServerID: "server-a"}
		limiter.Reserve("user-a", "server-a", "xbox-001", "item-a", "session-a")
		if limiter.Stop("user-a", "server-a", "phone-001", "session-a") {
			t.Fatal("wrong-device stop unexpectedly released lease")
		}
		limiter.mu.Lock()
		entry := limiter.streams[key]
		limiter.mu.Unlock()
		if entry == nil || entry.DeviceID != "xbox-001" {
			t.Fatalf("lease changed after wrong-device stop: %+v", entry)
		}
	})

	t.Run("old session stop cannot release newer playback", func(t *testing.T) {
		limiter := NewPlaybackLimiter()
		key := streamKey{UserID: "user-a", ServerID: "server-a"}
		limiter.Reserve("user-a", "server-a", "xbox-001", "item-a", "session-a")
		limiter.Reserve("user-a", "server-a", "xbox-001", "item-b", "session-b")
		if limiter.Stop("user-a", "server-a", "xbox-001", "session-a") {
			t.Fatal("old PlaySessionID unexpectedly released newer playback")
		}
		limiter.mu.Lock()
		entry := limiter.streams[key]
		limiter.mu.Unlock()
		if entry == nil || entry.PlaySessionID != "session-b" {
			t.Fatalf("newer lease changed after old-session stop: %+v", entry)
		}
	})

	t.Run("current session stop releases lease", func(t *testing.T) {
		limiter := NewPlaybackLimiter()
		key := streamKey{UserID: "user-a", ServerID: "server-a"}
		limiter.Reserve("user-a", "server-a", "xbox-001", "item-a", "session-a")
		if !limiter.Stop("user-a", "server-a", "xbox-001", "session-a") {
			t.Fatal("current device/current session stop should release lease")
		}
		limiter.mu.Lock()
		entry := limiter.streams[key]
		limiter.mu.Unlock()
		if entry != nil {
			t.Fatalf("lease remained after current stop: %+v", entry)
		}
	})
}
