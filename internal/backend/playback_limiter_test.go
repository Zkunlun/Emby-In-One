package backend

import (
	"testing"
	"time"
)

func TestPlaybackLimiterDifferentUsersDoNotCompete(t *testing.T) {
	l := NewPlaybackLimiter()
	first := l.Reserve("user1", "srv-0", "xbox-001", "item-a", "session-a")
	second := l.Reserve("user2", "srv-0", "phone-001", "item-b", "session-b")
	if !first.Allowed || !second.Allowed {
		t.Fatalf("different users on one upstream should each own a lease: first=%+v second=%+v", first, second)
	}
	if got := l.CountForServer("srv-0"); got != 2 {
		t.Fatalf("CountForServer = %d, want 2 independent user leases", got)
	}
}

func TestPlaybackLimiterRejectsSecondDeviceForSameUser(t *testing.T) {
	l := NewPlaybackLimiter()
	if got := l.Reserve("user1", "srv-0", "xbox-001", "item-a", "session-a"); !got.Allowed || !got.Created {
		t.Fatalf("first lease = %+v, want allowed+created", got)
	}
	if got := l.Reserve("user1", "srv-0", "phone-001", "item-b", "session-b"); got.Allowed {
		t.Fatalf("second device unexpectedly acquired same user/server lease: %+v", got)
	}
}

func TestPlaybackLimiterSameDeviceReusesLease(t *testing.T) {
	l := NewPlaybackLimiter()
	first := l.Reserve("user1", "srv-0", "xbox-001", "item-a", "session-a")
	if !first.Allowed || !first.Created {
		t.Fatalf("first reserve = %+v, want allowed+created", first)
	}
	second := l.Reserve("user1", "srv-0", "xbox-001", "item-b", "session-b")
	if !second.Allowed || second.Created {
		t.Fatalf("same-device reserve = %+v, want allowed+reused", second)
	}
	if got := l.CountForServer("srv-0"); got != 1 {
		t.Fatalf("CountForServer = %d, want one reused lease", got)
	}
}

func TestPlaybackLimiterHeartbeatRefresh(t *testing.T) {
	l := NewPlaybackLimiter()
	l.Reserve("user1", "srv-0", "xbox-001", "item-a", "session-a")

	l.mu.Lock()
	entry := l.streams[streamKey{UserID: "user1", ServerID: "srv-0"}]
	entry.LastHeartbeat = time.Now().Add(-2 * time.Minute)
	l.mu.Unlock()

	if !l.Heartbeat("user1", "srv-0", "xbox-001") {
		t.Fatal("lease owner heartbeat should refresh")
	}

	l.mu.Lock()
	refreshed := l.streams[streamKey{UserID: "user1", ServerID: "srv-0"}]
	l.mu.Unlock()
	if time.Since(refreshed.LastHeartbeat) > time.Second {
		t.Error("Heartbeat should refresh to now")
	}
}

func TestPlaybackLimiterStopRemovesMatchingLease(t *testing.T) {
	l := NewPlaybackLimiter()
	l.Reserve("user1", "srv-0", "xbox-001", "item-a", "session-a")
	if !l.Stop("user1", "srv-0", "xbox-001", "session-a") {
		t.Fatal("matching device/session should release lease")
	}
	if got := l.CountForServer("srv-0"); got != 0 {
		t.Fatalf("CountForServer = %d, want 0 after stop", got)
	}
}

func TestPlaybackLimiterExpiryAllowsDeviceTakeover(t *testing.T) {
	l := NewPlaybackLimiter()
	l.Reserve("user1", "srv-0", "xbox-001", "item-a", "session-a")

	l.mu.Lock()
	l.streams[streamKey{UserID: "user1", ServerID: "srv-0"}].LastHeartbeat = time.Now().Add(-4 * time.Minute)
	l.mu.Unlock()

	takeover := l.Reserve("user1", "srv-0", "phone-001", "item-b", "session-b")
	if !takeover.Allowed || !takeover.Created {
		t.Fatalf("stale lease takeover = %+v, want allowed+created", takeover)
	}
}

func TestPlaybackLimiterCountForServer(t *testing.T) {
	l := NewPlaybackLimiter()
	l.Reserve("user1", "srv-0", "xbox-001", "item-a", "session-a")
	l.Reserve("user2", "srv-0", "phone-001", "item-b", "session-b")
	l.Reserve("user1", "srv-1", "tablet-001", "item-c", "session-c")

	if c := l.CountForServer("srv-0"); c != 2 {
		t.Errorf("CountForServer(0) = %d, want 2", c)
	}
	if c := l.CountForServer("srv-1"); c != 1 {
		t.Errorf("CountForServer(1) = %d, want 1", c)
	}
}

func TestPlaybackLimiterHasNoMaxConcurrentSwitch(t *testing.T) {
	l := NewPlaybackLimiter()
	if got := l.Reserve("user1", "srv-0", "xbox-001", "item-a", "session-a"); !got.Allowed {
		t.Fatalf("first device reserve = %+v, want allowed", got)
	}
	if got := l.Reserve("user1", "srv-0", "phone-001", "item-a", "session-b"); got.Allowed {
		t.Fatalf("second device unexpectedly allowed; authorization capacity must not disable the lease rule: %+v", got)
	}
}

func TestPlaybackLimiterSameUserDifferentServersAreIndependent(t *testing.T) {
	l := NewPlaybackLimiter()
	first := l.Reserve("user1", "srv-0", "xbox-001", "item-a", "session-a")
	second := l.Reserve("user1", "srv-1", "phone-001", "item-b", "session-b")
	if !first.Allowed || !second.Allowed {
		t.Fatalf("same user should hold independent leases per upstream: first=%+v second=%+v", first, second)
	}
}

func TestPlaybackLimiterExpiredEntryIsReplacedWithFreshOwnership(t *testing.T) {
	l := NewPlaybackLimiter()
	l.Reserve("user-a", "srv-0", "xbox-001", "item-a", "session-a")
	key := streamKey{UserID: "user-a", ServerID: "srv-0"}
	l.mu.Lock()
	l.streams[key].LastHeartbeat = time.Now().Add(-playbackHeartbeatTimeout - time.Minute)
	l.mu.Unlock()

	result := l.Reserve("user-a", "srv-0", "phone-001", "item-b", "session-b")
	if !result.Allowed || !result.Created {
		t.Fatalf("expired takeover = %+v, want allowed+created", result)
	}
	l.mu.Lock()
	entry := *l.streams[key]
	l.mu.Unlock()
	if entry.DeviceID != "phone-001" || entry.ItemID != "item-b" || entry.PlaySessionID != "session-b" {
		t.Fatalf("replacement entry = %+v, want phone/item-b/session-b", entry)
	}
}

func TestPlaybackLimiterCountPrunesExpiredEntries(t *testing.T) {
	l := NewPlaybackLimiter()
	l.Reserve("user-a", "srv-0", "xbox-001", "item-a", "session-a")
	l.Reserve("user-b", "srv-0", "phone-001", "item-b", "session-b")
	now := time.Now()
	l.mu.Lock()
	l.streams[streamKey{UserID: "user-a", ServerID: "srv-0"}].LastHeartbeat = now.Add(-playbackHeartbeatTimeout - time.Minute)
	l.streams[streamKey{UserID: "user-b", ServerID: "srv-0"}].LastHeartbeat = now.Add(-playbackHeartbeatTimeout - time.Minute)
	l.mu.Unlock()

	if got := l.CountForServer("srv-0"); got != 0 {
		t.Fatalf("CountForServer = %d, want 0 after pruning stale leases", got)
	}
	l.mu.Lock()
	remaining := len(l.streams)
	l.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("stale leases remained after CountForServer prune: %d", remaining)
	}
}
