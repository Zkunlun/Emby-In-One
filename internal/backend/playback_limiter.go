package backend

import (
	"sync"
	"time"
)

const playbackHeartbeatTimeout = 3 * time.Minute

type streamKey struct {
	UserID   string
	ServerID string
}

type streamEntry struct {
	DeviceID      string
	ItemID        string
	PlaySessionID string
	LastHeartbeat time.Time
	Revision      uint64
}

func playbackLeaseExpired(now time.Time, entry *streamEntry) bool {
	return entry == nil || now.Sub(entry.LastHeartbeat) >= playbackHeartbeatTimeout
}

func (l *PlaybackLimiter) pruneExpiredLocked(now time.Time) {
	for key, entry := range l.streams {
		if playbackLeaseExpired(now, entry) {
			delete(l.streams, key)
		}
	}
}

// playbackLeaseResult reports whether a lease reservation was accepted, whether
// this call created/replaced the stored lease, and the revision written by this
// successful reservation. Created is false when the same device refreshes an
// existing live lease. Revision lets a creator roll back only if no later Reserve
// has refreshed, committed, or replaced that lease.
type playbackLeaseResult struct {
	Allowed  bool
	Created  bool
	Revision uint64
}

type PlaybackLimiter struct {
	mu           sync.Mutex
	streams      map[streamKey]*streamEntry
	nextRevision uint64
}

func NewPlaybackLimiter() *PlaybackLimiter {
	return &PlaybackLimiter{
		streams: make(map[streamKey]*streamEntry),
	}
}

func (l *PlaybackLimiter) nextRevisionLocked() uint64 {
	l.nextRevision++
	if l.nextRevision == 0 {
		// Zero is reserved for "no reservation" in result values. Wrapping would
		// require 2^64 successful reservations in one process lifetime, but keep the
		// invariant explicit anyway.
		l.nextRevision++
	}
	return l.nextRevision
}

// Reserve acquires or refreshes the single active playback-device lease for one
// regular user on one upstream. Different users never compete here: upstream
// authorization capacity is separate from active playback-device ownership.
//
// A live lease may only be refreshed by the same DeviceID. A different device is
// rejected until the current lease becomes stale, at which point the stale entry
// is replaced by the new device. DeviceID parsing/validation belongs to the HTTP
// layer; the limiter only compares the identity it is given.
func (l *PlaybackLimiter) Reserve(userID string, serverID string, deviceID string, itemID string, playSessionID string) playbackLeaseResult {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	key := streamKey{UserID: userID, ServerID: serverID}
	if existing, ok := l.streams[key]; ok {
		if playbackLeaseExpired(now, existing) {
			delete(l.streams, key)
		} else if existing.DeviceID != deviceID {
			return playbackLeaseResult{}
		} else {
			// A same-device reservation made before PlaybackInfo succeeds may not have
			// the next PlaySessionID yet. Treat that as a provisional refresh only so a
			// failed retry cannot overwrite the currently committed item/session. Once
			// the caller has an explicit PlaySessionID, commit the new playback state.
			if playSessionID != "" {
				existing.ItemID = itemID
				existing.PlaySessionID = playSessionID
			}
			existing.LastHeartbeat = now
			existing.Revision = l.nextRevisionLocked()
			return playbackLeaseResult{Allowed: true, Revision: existing.Revision}
		}
	}

	revision := l.nextRevisionLocked()
	l.streams[key] = &streamEntry{
		DeviceID:      deviceID,
		ItemID:        itemID,
		PlaySessionID: playSessionID,
		LastHeartbeat: now,
		Revision:      revision,
	}
	return playbackLeaseResult{Allowed: true, Created: true, Revision: revision}
}

// RollbackReservation removes a lease only when it is still exactly the revision
// created by the failed reservation. Any later successful Reserve advances the
// revision, so an older PlaybackInfo failure cannot delete newer same-device state
// or a lease that has since been taken over.
func (l *PlaybackLimiter) RollbackReservation(userID string, serverID string, deviceID string, revision uint64) bool {
	if revision == 0 {
		return false
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	key := streamKey{UserID: userID, ServerID: serverID}
	entry, ok := l.streams[key]
	if !ok || entry.DeviceID != deviceID || entry.Revision != revision {
		return false
	}
	delete(l.streams, key)
	return true
}

// Heartbeat refreshes a live playback lease only when deviceID still owns it.
// A stale lease is removed rather than revived, and a heartbeat from another
// device is ignored so it cannot keep the current owner's lease alive.
func (l *PlaybackLimiter) Heartbeat(userID string, serverID string, deviceID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	key := streamKey{UserID: userID, ServerID: serverID}
	entry, ok := l.streams[key]
	if !ok {
		return false
	}
	if playbackLeaseExpired(now, entry) {
		delete(l.streams, key)
		return false
	}
	if entry.DeviceID != deviceID {
		return false
	}
	entry.LastHeartbeat = now
	return true
}

// Stop releases a playback lease only when the caller owns the current device
// lease and does not present a stale PlaySessionID. When the current lease has a
// PlaySessionID, callers must present that exact session; this prevents a delayed
// Stopped event from an older playback on the same device from clearing a newer
// lease. A stale lease is removed but is reported as not released by the caller.
func (l *PlaybackLimiter) Stop(userID string, serverID string, deviceID string, playSessionID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	key := streamKey{UserID: userID, ServerID: serverID}
	entry, ok := l.streams[key]
	if !ok {
		return false
	}
	now := time.Now()
	if playbackLeaseExpired(now, entry) {
		delete(l.streams, key)
		return false
	}
	if entry.DeviceID != deviceID {
		return false
	}
	// Session ownership is exact. In particular, an empty current session means
	// the lease is still provisional/legacy; a delayed non-empty Stopped event
	// must not be allowed to claim and clear that lease.
	if entry.PlaySessionID != playSessionID {
		return false
	}
	delete(l.streams, key)
	return true
}

// CountForServer returns the number of active playback leases on a server. Counting
// is also a cleanup boundary: stale leases are removed first so read-only inspection
// cannot leave expired entries accumulating in the map.
func (l *PlaybackLimiter) CountForServer(serverID string) int {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.pruneExpiredLocked(time.Now())
	count := 0
	for k := range l.streams {
		if k.ServerID == serverID {
			count++
		}
	}
	return count
}

// Cleanup removes all expired stream entries using the same boundary as
// Reserve, Heartbeat, Stop, and CountForServer.
func (l *PlaybackLimiter) Cleanup() {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.pruneExpiredLocked(time.Now())
}
