package backend

import "testing"

func TestPhase4GSameDeviceSessionUpdateIsCommittedOnlyWhenKnown(t *testing.T) {
	limiter := NewPlaybackLimiter()
	key := streamKey{UserID: "user-a", ServerID: "server-a"}

	first := limiter.Reserve("user-a", "server-a", "xbox-001", "item-a", "session-a")
	if !first.Allowed || !first.Created {
		t.Fatalf("initial reserve = %+v, want allowed+created", first)
	}

	// A new PlaybackInfo attempt starts before its next PlaySessionID is known.
	// This is only a provisional refresh and must not destroy the committed state.
	pending := limiter.Reserve("user-a", "server-a", "xbox-001", "item-b", "")
	if !pending.Allowed || pending.Created {
		t.Fatalf("provisional same-device reserve = %+v, want allowed+reused", pending)
	}
	limiter.mu.Lock()
	entry := *limiter.streams[key]
	limiter.mu.Unlock()
	if entry.ItemID != "item-a" || entry.PlaySessionID != "session-a" {
		t.Fatalf("provisional reserve changed committed state: %+v", entry)
	}

	// Once PlaybackInfo succeeds, the explicit new session atomically becomes the
	// current playback state without creating a second lease.
	committed := limiter.Reserve("user-a", "server-a", "xbox-001", "item-b", "session-b")
	if !committed.Allowed || committed.Created {
		t.Fatalf("committed same-device reserve = %+v, want allowed+reused", committed)
	}
	limiter.mu.Lock()
	entry = *limiter.streams[key]
	limiter.mu.Unlock()
	if entry.DeviceID != "xbox-001" || entry.ItemID != "item-b" || entry.PlaySessionID != "session-b" {
		t.Fatalf("committed lease = %+v, want xbox/item-b/session-b", entry)
	}

	if limiter.Stop("user-a", "server-a", "xbox-001", "session-a") {
		t.Fatal("old session unexpectedly released newer playback")
	}
	if !limiter.Stop("user-a", "server-a", "xbox-001", "session-b") {
		t.Fatal("current session should release newer playback")
	}
}

func TestPhase4GProvisionalLeaseRejectsUnrelatedStoppedSession(t *testing.T) {
	limiter := NewPlaybackLimiter()
	lease := limiter.Reserve("user-a", "server-a", "xbox-001", "item-a", "")
	if !lease.Allowed || !lease.Created {
		t.Fatalf("provisional lease = %+v, want allowed+created", lease)
	}

	if limiter.Stop("user-a", "server-a", "xbox-001", "stale-session") {
		t.Fatal("non-empty stopped session unexpectedly released provisional lease")
	}
	if !limiter.Stop("user-a", "server-a", "xbox-001", "") {
		t.Fatal("matching empty provisional session should release lease")
	}
}
