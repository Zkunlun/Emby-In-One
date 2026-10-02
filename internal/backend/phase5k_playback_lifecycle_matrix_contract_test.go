package backend

import (
	"net/http"
	"testing"
	"time"
)

func phase5KLeaseSnapshot(l *PlaybackLimiter, userID, serverID string) *streamEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry := l.streams[streamKey{UserID: userID, ServerID: serverID}]
	if entry == nil {
		return nil
	}
	copy := *entry
	return &copy
}

func phase5KAgeLease(l *PlaybackLimiter, userID, serverID string, heartbeat time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if entry := l.streams[streamKey{UserID: userID, ServerID: serverID}]; entry != nil {
		entry.LastHeartbeat = heartbeat
	}
}

// TestPhase5KPlaybackLifecycleMatrix consolidates the state transitions that Phase 5
// wires together. HTTP DeviceID extraction is intentionally not part of this matrix;
// Phase 6 owns that boundary. These contracts start from an already-resolved device
// identity and assert the lifecycle state that handlers are expected to preserve.
func TestPhase5KPlaybackLifecycleMatrix(t *testing.T) {
	t.Run("01 initial PlaybackInfo success commits session", func(t *testing.T) {
		l := NewPlaybackLimiter()
		provisional := l.Reserve("user", "server-a", "xbox", "item-a", "")
		if !provisional.Allowed || !provisional.Created {
			t.Fatalf("provisional reserve = %+v, want allowed+created", provisional)
		}
		committed := l.Reserve("user", "server-a", "xbox", "item-a", "session-a")
		if !committed.Allowed || committed.Created {
			t.Fatalf("commit reserve = %+v, want allowed existing lease", committed)
		}
		entry := phase5KLeaseSnapshot(l, "user", "server-a")
		if entry == nil || entry.ItemID != "item-a" || entry.PlaySessionID != "session-a" {
			t.Fatalf("committed lease = %+v", entry)
		}
	})

	t.Run("02 initial PlaybackInfo total failure leaves no lease", func(t *testing.T) {
		l := NewPlaybackLimiter()
		created := l.Reserve("user", "server-a", "xbox", "item-a", "")
		if !created.Created || !l.RollbackReservation("user", "server-a", "xbox", created.Revision) {
			t.Fatalf("created reservation did not roll back: %+v", created)
		}
		if got := l.CountForServer("server-a"); got != 0 {
			t.Fatalf("lease count=%d, want 0", got)
		}
	})

	t.Run("03 same-device repeated success replaces committed item and session", func(t *testing.T) {
		l := NewPlaybackLimiter()
		l.Reserve("user", "server-a", "xbox", "item-a", "session-a")
		result := l.Reserve("user", "server-a", "xbox", "item-b", "session-b")
		if !result.Allowed || result.Created {
			t.Fatalf("repeat reserve = %+v", result)
		}
		entry := phase5KLeaseSnapshot(l, "user", "server-a")
		if entry == nil || entry.ItemID != "item-b" || entry.PlaySessionID != "session-b" {
			t.Fatalf("repeat commit = %+v", entry)
		}
	})

	t.Run("04 same-device provisional retry preserves committed state", func(t *testing.T) {
		l := NewPlaybackLimiter()
		l.Reserve("user", "server-a", "xbox", "item-a", "session-a")
		result := l.Reserve("user", "server-a", "xbox", "item-b", "")
		if !result.Allowed || result.Created {
			t.Fatalf("provisional retry = %+v", result)
		}
		entry := phase5KLeaseSnapshot(l, "user", "server-a")
		if entry == nil || entry.ItemID != "item-a" || entry.PlaySessionID != "session-a" {
			t.Fatalf("provisional retry polluted committed state: %+v", entry)
		}
	})

	t.Run("05 creator rollback cannot delete later accepted reservation", func(t *testing.T) {
		l := NewPlaybackLimiter()
		first := l.Reserve("user", "server-a", "xbox", "item-a", "")
		later := l.Reserve("user", "server-a", "xbox", "item-a", "session-a")
		if first.Revision == 0 || later.Revision == first.Revision {
			t.Fatalf("revisions did not advance: first=%+v later=%+v", first, later)
		}
		if l.RollbackReservation("user", "server-a", "xbox", first.Revision) {
			t.Fatal("older creator rollback deleted later reservation")
		}
	})

	t.Run("06 Playing owner heartbeat refreshes lease", func(t *testing.T) {
		l := NewPlaybackLimiter()
		l.Reserve("user", "server-a", "xbox", "item-a", "session-a")
		old := time.Now().Add(-time.Minute).Round(0)
		phase5KAgeLease(l, "user", "server-a", old)
		if !l.Heartbeat("user", "server-a", "xbox") {
			t.Fatal("owner heartbeat rejected")
		}
		if entry := phase5KLeaseSnapshot(l, "user", "server-a"); entry == nil || !entry.LastHeartbeat.After(old) {
			t.Fatalf("owner heartbeat not refreshed: %+v", entry)
		}
	})

	t.Run("07 Progress owner heartbeat uses the same device ownership", func(t *testing.T) {
		l := NewPlaybackLimiter()
		l.Reserve("user", "server-a", "xbox", "item-a", "session-a")
		if !l.Heartbeat("user", "server-a", "xbox") {
			t.Fatal("progress owner heartbeat rejected")
		}
		if !l.Reserve("user", "server-a", "xbox", "item-a", "session-a").Allowed {
			t.Fatal("owner lost lease after heartbeat")
		}
	})

	t.Run("08 wrong-device heartbeat does not refresh lease", func(t *testing.T) {
		l := NewPlaybackLimiter()
		l.Reserve("user", "server-a", "xbox", "item-a", "session-a")
		old := time.Now().Add(-time.Minute).Round(0)
		phase5KAgeLease(l, "user", "server-a", old)
		if l.Heartbeat("user", "server-a", "phone") {
			t.Fatal("wrong-device heartbeat accepted")
		}
		if entry := phase5KLeaseSnapshot(l, "user", "server-a"); entry == nil || !entry.LastHeartbeat.Equal(old) {
			t.Fatalf("wrong-device heartbeat changed lease: %+v", entry)
		}
	})

	t.Run("09 current Stopped releases matching lease", func(t *testing.T) {
		l := NewPlaybackLimiter()
		l.Reserve("user", "server-a", "xbox", "item-a", "session-a")
		if !l.Stop("user", "server-a", "xbox", "session-a") {
			t.Fatal("matching stopped event did not release lease")
		}
		if got := l.CountForServer("server-a"); got != 0 {
			t.Fatalf("lease count=%d, want 0", got)
		}
	})

	t.Run("10 wrong-device Stopped cannot release lease", func(t *testing.T) {
		l := NewPlaybackLimiter()
		l.Reserve("user", "server-a", "xbox", "item-a", "session-a")
		if l.Stop("user", "server-a", "phone", "session-a") {
			t.Fatal("wrong-device stopped event released lease")
		}
		if got := l.CountForServer("server-a"); got != 1 {
			t.Fatalf("lease count=%d, want 1", got)
		}
	})

	t.Run("11 old-session Stopped cannot release newer playback", func(t *testing.T) {
		l := NewPlaybackLimiter()
		l.Reserve("user", "server-a", "xbox", "item-a", "session-a")
		l.Reserve("user", "server-a", "xbox", "item-b", "session-b")
		if l.Stop("user", "server-a", "xbox", "session-a") {
			t.Fatal("old session stopped event released newer lease")
		}
		entry := phase5KLeaseSnapshot(l, "user", "server-a")
		if entry == nil || entry.PlaySessionID != "session-b" {
			t.Fatalf("newer lease not preserved: %+v", entry)
		}
	})

	t.Run("12 stopped preparation-error ownership stays exact", func(t *testing.T) {
		l := NewPlaybackLimiter()
		l.Reserve("user", "server-a", "xbox", "item-a", "session-a")
		if l.Stop("user", "server-a", "phone", "session-a") {
			t.Fatal("preparation-error wrong device would release lease")
		}
		if !l.Stop("user", "server-a", "xbox", "session-a") {
			t.Fatal("preparation-error matching owner would not release lease")
		}
	})

	t.Run("13 stale takeover followed by failed PlaybackInfo cleans takeover only", func(t *testing.T) {
		l := NewPlaybackLimiter()
		l.Reserve("user", "server-a", "xbox", "item-a", "session-a")
		phase5KAgeLease(l, "user", "server-a", time.Now().Add(-playbackHeartbeatTimeout-time.Second))
		takeover := l.Reserve("user", "server-a", "phone", "item-b", "")
		if !takeover.Allowed || !takeover.Created {
			t.Fatalf("stale takeover = %+v, want created", takeover)
		}
		if !l.RollbackReservation("user", "server-a", "phone", takeover.Revision) {
			t.Fatal("failed takeover PlaybackInfo did not clean its reservation")
		}
		if got := l.CountForServer("server-a"); got != 0 {
			t.Fatalf("old expired lease was restored or takeover leaked: count=%d", got)
		}
	})

	t.Run("14 same user has independent lifecycle per upstream", func(t *testing.T) {
		l := NewPlaybackLimiter()
		if !l.Reserve("user", "server-a", "xbox", "item-a", "session-a").Allowed {
			t.Fatal("server-a reserve rejected")
		}
		if !l.Reserve("user", "server-b", "phone", "item-b", "session-b").Allowed {
			t.Fatal("server-b reserve rejected")
		}
		if !l.Stop("user", "server-a", "xbox", "session-a") {
			t.Fatal("server-a stop rejected")
		}
		if got := l.CountForServer("server-b"); got != 1 {
			t.Fatalf("server-a lifecycle mutated server-b: count=%d", got)
		}
	})

	t.Run("15 admin lifecycle remains outside limiter", func(t *testing.T) {
		upstream := phase1DPlaybackUpstream(t)
		defer upstream.Close()
		withTempAppConfig(t, phase1DPlaybackConfig(upstream.URL, 1), func(app *App, _ http.Handler) {
			ctx := &RequestContext{
				ProxyUser:        &tokenInfo{UserID: "admin-id", Role: "admin"},
				PlaybackDeviceID: "xbox",
			}
			if app.heartbeatPlaybackLease(ctx, "server-a") {
				t.Fatal("admin heartbeat unexpectedly entered limiter")
			}
			if app.stopPlaybackLease(ctx, "server-a", "session-a") {
				t.Fatal("admin stop unexpectedly entered limiter")
			}
			if got := app.PlaybackLimiter.CountForServer("server-a"); got != 0 {
				t.Fatalf("admin lifecycle created limiter state: count=%d", got)
			}
		})
	})
}
