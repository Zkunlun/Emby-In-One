package backend

import "testing"

func TestPhase5BRollbackReservationOwnsExactRevision(t *testing.T) {
	t.Run("creator can roll back unchanged lease", func(t *testing.T) {
		limiter := NewPlaybackLimiter()
		created := limiter.Reserve("user-a", "server-a", "xbox-001", "item-a", "")
		if !created.Allowed || !created.Created || created.Revision == 0 {
			t.Fatalf("created reservation = %+v, want allowed+created with revision", created)
		}
		if !limiter.RollbackReservation("user-a", "server-a", "xbox-001", created.Revision) {
			t.Fatal("unchanged creator reservation should roll back")
		}
		if got := limiter.CountForServer("server-a"); got != 0 {
			t.Fatalf("lease remained after rollback: count=%d", got)
		}
	})

	t.Run("same-device later reserve invalidates old rollback", func(t *testing.T) {
		limiter := NewPlaybackLimiter()
		first := limiter.Reserve("user-a", "server-a", "xbox-001", "item-a", "")
		later := limiter.Reserve("user-a", "server-a", "xbox-001", "item-a", "")
		if !first.Created || later.Created || first.Revision == 0 || later.Revision == 0 || first.Revision == later.Revision {
			t.Fatalf("unexpected revisions: first=%+v later=%+v", first, later)
		}
		if limiter.RollbackReservation("user-a", "server-a", "xbox-001", first.Revision) {
			t.Fatal("old creator rollback deleted a lease refreshed by a later reserve")
		}
		if got := limiter.CountForServer("server-a"); got != 1 {
			t.Fatalf("later lease lost after stale rollback: count=%d", got)
		}
	})

	t.Run("session commit invalidates provisional rollback", func(t *testing.T) {
		limiter := NewPlaybackLimiter()
		provisional := limiter.Reserve("user-a", "server-a", "xbox-001", "item-a", "")
		committed := limiter.Reserve("user-a", "server-a", "xbox-001", "item-a", "session-a")
		if !provisional.Created || committed.Created || provisional.Revision == committed.Revision {
			t.Fatalf("unexpected provisional/commit revisions: provisional=%+v committed=%+v", provisional, committed)
		}
		if limiter.RollbackReservation("user-a", "server-a", "xbox-001", provisional.Revision) {
			t.Fatal("provisional rollback deleted committed playback")
		}

		key := streamKey{UserID: "user-a", ServerID: "server-a"}
		limiter.mu.Lock()
		entry := limiter.streams[key]
		limiter.mu.Unlock()
		if entry == nil || entry.PlaySessionID != "session-a" || entry.Revision != committed.Revision {
			t.Fatalf("committed lease changed after stale rollback: %+v", entry)
		}
	})

	t.Run("wrong device or revision cannot roll back", func(t *testing.T) {
		limiter := NewPlaybackLimiter()
		created := limiter.Reserve("user-a", "server-a", "xbox-001", "item-a", "")
		if limiter.RollbackReservation("user-a", "server-a", "phone-001", created.Revision) {
			t.Fatal("wrong device unexpectedly rolled back lease")
		}
		if limiter.RollbackReservation("user-a", "server-a", "xbox-001", created.Revision+1) {
			t.Fatal("wrong revision unexpectedly rolled back lease")
		}
		if got := limiter.CountForServer("server-a"); got != 1 {
			t.Fatalf("lease changed after rejected rollbacks: count=%d", got)
		}
	})
}
