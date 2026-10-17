package backend

import (
	"testing"
	"time"
)

func TestPhase3ExpiredLeaseCompletesConfirmedWatchOnly(t *testing.T) {
	for _, tc := range []struct {
		name         string
		position     int64
		wantPlayed   bool
		wantPosition int64
	}{
		{"above threshold", 950, true, 0},
		{"below threshold", 800, false, 800},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withUnifiedWatchFixture(t, func(f *unifiedWatchFixture) {
				f.send(t, 0, "/Sessions/Playing", 0)
				if tc.position == 800 {
					f.send(t, 0, "/Sessions/Playing/Progress", 950)
					f.assertProgress(t, 950, false, "server-a")
				}
				f.send(t, 0, "/Sessions/Playing/Progress", tc.position)
				f.assertProgress(t, tc.position, false, "server-a")
				f.app.watchPlayback.mu.Lock()
				state := f.app.watchPlayback.shared[playbackWatchSharedKey{UserID: f.info.UserID, ItemID: f.item}]
				if state == nil || !state.Active {
					f.app.watchPlayback.mu.Unlock()
					t.Fatal("no confirmed session")
				}
				session := state.Session
				f.app.watchPlayback.mu.Unlock()
				reservation := f.app.PlaybackLimiter.Reserve(f.info.UserID, "server-a", session.PlaybackDeviceID, f.item, session.PlaySessionID)
				if !reservation.Allowed {
					t.Fatal("reservation denied")
				}
				f.app.PlaybackLimiter.mu.Lock()
				f.app.PlaybackLimiter.streams[streamKey{UserID: f.info.UserID, ServerID: "server-a"}].LastHeartbeat = time.Now().Add(-4 * time.Minute)
				f.app.PlaybackLimiter.mu.Unlock()
				f.app.settleExpiredPlaybackLeases()
				f.assertProgress(t, tc.wantPosition, tc.wantPlayed, "server-a")
				f.app.settleExpiredPlaybackLeases()
				f.assertProgress(t, tc.wantPosition, tc.wantPlayed, "server-a")
				if !f.app.PlaybackLimiter.Reserve(f.info.UserID, "server-a", session.PlaybackDeviceID, f.item, session.PlaySessionID).Allowed {
					t.Fatal("expired lease should be released for later sessions")
				}
				// Do not finalize a stale event after the expiration generation.
				f.send(t, 0, "/Sessions/Playing/Progress", 100)
				f.assertProgress(t, tc.wantPosition, tc.wantPlayed, "server-a")
			})
		})
	}
}

func TestPhase3ExpiredOldLeaseCannotOverwriteNewSession(t *testing.T) {
	withUnifiedWatchFixture(t, func(f *unifiedWatchFixture) {
		f.send(t, 0, "/Sessions/Playing", 0)
		f.send(t, 0, "/Sessions/Playing/Progress", 950)
		f.app.watchPlayback.mu.Lock()
		session := f.app.watchPlayback.shared[playbackWatchSharedKey{UserID: f.info.UserID, ItemID: f.item}].Session
		f.app.watchPlayback.mu.Unlock()
		f.app.PlaybackLimiter.Reserve(f.info.UserID, "server-a", session.PlaybackDeviceID, f.item, session.PlaySessionID)
		f.app.PlaybackLimiter.mu.Lock()
		f.app.PlaybackLimiter.streams[streamKey{UserID: f.info.UserID, ServerID: "server-a"}].LastHeartbeat = time.Now().Add(-4 * time.Minute)
		f.app.PlaybackLimiter.mu.Unlock()
		// CountForServer prunes the expired lease even before periodic Cleanup.
		if count := f.app.PlaybackLimiter.CountForServer("server-a"); count != 0 {
			t.Fatalf("expired count=%d", count)
		}
		f.send(t, 1, "/Sessions/Playing", 0)
		f.send(t, 1, "/Sessions/Playing/Progress", 300)
		f.assertProgress(t, 300, false, "server-b")
		f.app.settleExpiredPlaybackLeases()
		f.assertProgress(t, 300, false, "server-b")
	})
}

func TestPhase3ExpiryReplacedByNewReservationStillCompletesOldWatch(t *testing.T) {
	withUnifiedWatchFixture(t, func(f *unifiedWatchFixture) {
		f.send(t, 0, "/Sessions/Playing", 0)
		f.send(t, 0, "/Sessions/Playing/Progress", 950)
		f.app.watchPlayback.mu.Lock()
		session := f.app.watchPlayback.shared[playbackWatchSharedKey{UserID: f.info.UserID, ItemID: f.item}].Session
		f.app.watchPlayback.mu.Unlock()
		f.app.PlaybackLimiter.Reserve(f.info.UserID, "server-a", session.PlaybackDeviceID, f.item, session.PlaySessionID)
		f.app.PlaybackLimiter.mu.Lock()
		f.app.PlaybackLimiter.streams[streamKey{UserID: f.info.UserID, ServerID: "server-a"}].LastHeartbeat = time.Now().Add(-4 * time.Minute)
		f.app.PlaybackLimiter.mu.Unlock()
		replacement := f.app.PlaybackLimiter.Reserve(f.info.UserID, "server-a", "new-phone", f.item, "new-session")
		if !replacement.Allowed || !replacement.Created {
			t.Fatalf("new device could not take over expired lease: %+v", replacement)
		}
		f.app.settleExpiredPlaybackLeases()
		f.assertProgress(t, 0, true, "server-a")
		if got := f.app.PlaybackLimiter.CountForServer("server-a"); got != 1 {
			t.Fatalf("settlement wrongly released new device lease: %d", got)
		}
	})
}

func TestPhase3PendingStoppedPreventsExpiryFromOverridingFinalPosition(t *testing.T) {
	withUnifiedWatchFixture(t, func(f *unifiedWatchFixture) {
		f.send(t, 0, "/Sessions/Playing", 0)
		f.send(t, 0, "/Sessions/Playing/Progress", 950)
		f.app.watchPlayback.mu.Lock()
		state := f.app.watchPlayback.shared[playbackWatchSharedKey{UserID: f.info.UserID, ItemID: f.item}]
		if state == nil {
			f.app.watchPlayback.mu.Unlock()
			t.Fatal("missing shared watch state")
		}
		session := state.Session
		// Equivalent to a verified Stopped admission waiting for an upstream
		// reply. The expiry must not commit its earlier 95% position first.
		state.PendingStopped = true
		f.app.watchPlayback.mu.Unlock()
		f.app.PlaybackLimiter.Reserve(f.info.UserID, "server-a", session.PlaybackDeviceID, f.item, session.PlaySessionID)
		f.app.PlaybackLimiter.mu.Lock()
		f.app.PlaybackLimiter.streams[streamKey{UserID: f.info.UserID, ServerID: "server-a"}].LastHeartbeat = time.Now().Add(-4 * time.Minute)
		f.app.PlaybackLimiter.mu.Unlock()
		f.app.settleExpiredPlaybackLeases()
		f.assertProgress(t, 950, false, "server-a")
	})
}

func TestPhase3LimiterQueuesEveryExpiryPath(t *testing.T) {
	for _, tc := range []struct {
		name   string
		expire func(*PlaybackLimiter)
	}{
		{"cleanup", func(l *PlaybackLimiter) { l.Cleanup() }},
		{"count", func(l *PlaybackLimiter) { l.CountForServer("other") }},
		{"reserve", func(l *PlaybackLimiter) { l.Reserve("user", "server", "other", "other-item", "new") }},
		{"heartbeat", func(l *PlaybackLimiter) { l.Heartbeat("user", "server", "device") }},
		{"stop", func(l *PlaybackLimiter) { l.Stop("user", "server", "device", "session") }},
		{"revision", func(l *PlaybackLimiter) { l.PlaybackRevision("user", "server", "device", "item", "session") }},
		{"stop revision", func(l *PlaybackLimiter) { l.StopPlaybackRevision("user", "server", "device", "item", "session", 1) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := NewPlaybackLimiter()
			l.Reserve("user", "server", "device", "item", "session")
			l.mu.Lock()
			l.streams[streamKey{UserID: "user", ServerID: "server"}].LastHeartbeat = time.Now().Add(-4 * time.Minute)
			l.mu.Unlock()
			tc.expire(l)
			leases := l.takeExpired()
			if len(leases) != 1 || leases[0].Entry.ItemID != "item" || leases[0].Entry.PlaySessionID != "session" {
				t.Fatalf("expiry lost or duplicated: %+v", leases)
			}
			if len(l.takeExpired()) != 0 {
				t.Fatal("expired lease must be consumed once")
			}
			l.retryExpired(leases[0])
			if len(l.takeExpired()) != 1 || len(l.takeExpired()) != 0 {
				t.Fatal("failed database settlement must retry one expiry once")
			}
		})
	}
}

func TestPhase3LifecycleRemovalDropsPendingExpiration(t *testing.T) {
	l := NewPlaybackLimiter()
	l.Reserve("user", "server", "device", "item", "session")
	l.mu.Lock()
	l.streams[streamKey{UserID: "user", ServerID: "server"}].LastHeartbeat = time.Now().Add(-4 * time.Minute)
	l.mu.Unlock()
	l.Cleanup()
	l.RemoveLifecycle("user", "")
	if len(l.takeExpired()) != 0 {
		t.Fatal("deleted user has stale pending expiry")
	}
}
