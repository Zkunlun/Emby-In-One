package backend

import (
	"testing"
	"time"
)

func TestPhase4CReserveReportsCreatedVsReusedLease(t *testing.T) {
	limiter := NewPlaybackLimiter()

	first := limiter.Reserve("user-a", "server-a", "xbox-001", "item-a", "session-a")
	if !first.Allowed || !first.Created {
		t.Fatalf("first reservation = %+v, want allowed+created", first)
	}

	repeat := limiter.Reserve("user-a", "server-a", "xbox-001", "item-b", "session-b")
	if !repeat.Allowed || repeat.Created {
		t.Fatalf("same-device refresh = %+v, want allowed without created", repeat)
	}

	blocked := limiter.Reserve("user-a", "server-a", "phone-001", "item-a", "session-phone")
	if blocked.Allowed || blocked.Created {
		t.Fatalf("different-device reservation = %+v, want denied without created", blocked)
	}

	key := streamKey{UserID: "user-a", ServerID: "server-a"}
	limiter.mu.Lock()
	limiter.streams[key].LastHeartbeat = time.Now().Add(-playbackHeartbeatTimeout - time.Second)
	limiter.mu.Unlock()

	takeover := limiter.Reserve("user-a", "server-a", "phone-001", "item-c", "session-c")
	if !takeover.Allowed || !takeover.Created {
		t.Fatalf("stale takeover = %+v, want allowed+created", takeover)
	}

	limiter.mu.Lock()
	entry := *limiter.streams[key]
	limiter.mu.Unlock()
	if entry.DeviceID != "phone-001" || entry.ItemID != "item-c" || entry.PlaySessionID != "session-c" {
		t.Fatalf("takeover entry = %+v, want phone/item-c/session-c", entry)
	}
}
