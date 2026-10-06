package backend

import (
	"context"
	"net/http"
	"time"
)

// Shared state is keyed by the real local user and merged virtual film, never
// by the last upstream locator or the spoofed device identity.
type playbackWatchSharedKey struct {
	UserID string
	ItemID string
}

type playbackWatchSharedState struct {
	Generation          uint64
	ResetRevision       uint64
	NextSequence        uint64
	StartedSequence     uint64
	LastCommitted       uint64
	TerminalSequence    uint64
	TerminalGeneration  uint64
	TerminalSession     playbackWatchSession
	Session             playbackWatchSession
	Active              bool
	Detached            bool
	RebasedFromRevision int64
	RebasedRevision     int64
	RebasedTokens       map[string]bool // Digests of tokens still issued at the deletion boundary.
	Seen                time.Time
}

type playbackWatchAdmission struct {
	SharedKey            playbackWatchSharedKey
	State                *playbackWatchSharedState
	ResetRevision        uint64
	Generation           uint64
	ConfirmedGeneration  uint64
	Sequence             uint64
	Kind                 playbackWatchEventKind
	Session              playbackWatchSession
	Snapshot             playbackWatchSnapshot
	UserRevision         int64
	Token                string
	RestartedSameContext bool
}

type playbackWatchAdmissionContextKey struct{}

type playbackWatchLeaseContextKey struct{}

type playbackWatchLeaseSnapshot struct {
	UserID        string
	ItemID        string
	ServerID      string
	DeviceID      string
	PlaySessionID string
	Revision      uint64
}

func samePlaybackWatchIdentity(a, b playbackWatchSession) bool {
	return a.ProxyUserID == b.ProxyUserID &&
		a.PlaybackDeviceID == b.PlaybackDeviceID && a.PlaySessionID == b.PlaySessionID &&
		a.Source.ServerID == b.Source.ServerID && a.Source.OriginalItemID == b.Source.OriginalItemID
}

// All shared-state helpers run under cache.mu. Eviction invalidates outstanding
// admissions by pointer identity; a cache miss cannot recreate a Progress owner.
func (cache *playbackWatchCache) pruneShared(now time.Time) {
	for key, state := range cache.shared {
		if now.Sub(state.Seen) > activeStreamTTL {
			delete(cache.shared, key)
		}
	}
}

func (cache *playbackWatchCache) sharedState(key playbackWatchSharedKey, now time.Time) *playbackWatchSharedState {
	cache.pruneShared(now)
	if cache.shared == nil {
		cache.shared = make(map[playbackWatchSharedKey]*playbackWatchSharedState)
	}
	if state := cache.shared[key]; state != nil {
		return state
	}
	if len(cache.shared) >= playbackWatchCacheLimit {
		var oldestKey playbackWatchSharedKey
		var oldest *playbackWatchSharedState
		for candidate, state := range cache.shared {
			if oldest == nil || state.Seen.Before(oldest.Seen) {
				oldestKey, oldest = candidate, state
			}
		}
		delete(cache.shared, oldestKey)
	}
	state := &playbackWatchSharedState{Seen: now}
	cache.shared[key] = state
	return state
}

// Capture ordering and the old generation before any lifecycle request leaves.
// A pending/failed Started does not replace the current owner or clear positions.
func (cache *playbackWatchCache) admit(key playbackWatchCacheKey, kind playbackWatchEventKind) *playbackWatchAdmission {
	session := key.Session
	if session.ProxyUserID == "" || key.VirtualItemID == "" ||
		session.PlaybackDeviceID == "" || session.PlaySessionID == "" || !session.Source.valid() {
		return nil
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	now := time.Now()
	cache.prune(now)
	sharedKey := playbackWatchSharedKey{UserID: session.ProxyUserID, ItemID: key.VirtualItemID}
	state := cache.shared[sharedKey]
	if kind == playbackWatchStarted {
		state = cache.sharedState(sharedKey, now)
	} else if state == nil {
		return nil
	}
	if kind != playbackWatchStarted {
		if !state.Active || !samePlaybackWatchIdentity(session, state.Session) {
			return nil
		}
		if session.Source.MediaSourceID == "" {
			// The complete explicit device/session/source matches. This is an
			// omitted version ID, not permission to guess the latest session.
			session.Source.MediaSourceID = state.Session.Source.MediaSourceID
			key.Session = session
		}
		if session != state.Session {
			return nil
		}
		key = cache.activeKey(key)
	}
	entry := cache.entries[key]
	if entry != nil && entry.Closed && kind != playbackWatchStarted {
		return nil
	}
	if entry == nil {
		cache.room(now)
		entry = &playbackWatchCacheEntry{}
		cache.entries[key] = entry
	}
	value := *entry
	if kind == playbackWatchStarted {
		value.Position = playbackPositionCandidate{}
		// An authoritative single-version preload may qualify an omitted ID.
		if session.Source.MediaSourceID == "" && value.Runtime.matches(session.Source) &&
			value.Runtime.SingleMediaSource {
			session.Source.MediaSourceID = value.Runtime.Source.MediaSourceID
		}
	}
	entry.Seen = now
	entry.NextSequence++
	state.Seen = now
	state.NextSequence++
	return &playbackWatchAdmission{
		SharedKey: sharedKey, State: state, ResetRevision: state.ResetRevision,
		Generation: state.Generation, Sequence: state.NextSequence, Kind: kind, Session: session,
		RestartedSameContext: kind == playbackWatchStarted && state.Active && samePlaybackWatchIdentity(session, state.Session),
		Snapshot:             playbackWatchSnapshot{Key: key, Entry: entry, Value: value, Sequence: entry.NextSequence},
	}
}

// A terminal event may close only its current generation. A stale terminal's
// position must not overwrite newer Progress, but its exact lease may still end.
func (cache *playbackWatchCache) commitAdmitted(admission *playbackWatchAdmission, metadata WatchProgress, event playbackWatchEvent, session playbackWatchSession, write func() error) error {
	if admission == nil || !session.identified() || event.Kind != admission.Kind ||
		!samePlaybackWatchIdentity(session, admission.Session) ||
		(admission.Session.Source.MediaSourceID != "" && admission.Session.Source.MediaSourceID != session.Source.MediaSourceID) {
		return nil
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	state := cache.shared[admission.SharedKey]
	snapshot := admission.Snapshot
	entry := cache.entries[snapshot.Key]
	if state != admission.State || state.ResetRevision != admission.ResetRevision || entry != snapshot.Entry {
		return nil
	}
	if event.Kind == playbackWatchStarted {
		if event.Failed {
			return nil
		}
		if admission.ConfirmedGeneration == 0 && !cache.claimStartedLocked(admission, state, entry, session) {
			return nil
		}
		if !state.Active || state.StartedSequence != admission.Sequence ||
			state.Generation != admission.ConfirmedGeneration || state.Session != session {
			return nil
		}
	} else if !state.Active || state.Generation != admission.Generation || state.Session != session {
		return nil
	}
	state.Seen = time.Now()
	stale := admission.Sequence < state.LastCommitted ||
		(event.Kind != playbackWatchStarted && admission.Sequence == state.LastCommitted)
	var err error
	if !stale {
		err = write()
		if err == nil {
			state.LastCommitted = admission.Sequence
			entry.LastCommitted = snapshot.Sequence
			entry.Active = true
			if metadata.ItemType != "" {
				entry.Metadata = metadata
			}
			if event.Runtime.valid() && event.Runtime.Ticks > 0 {
				entry.Runtime = playbackRuntimeCandidate{
					Source: event.Source, Ticks: event.Runtime.Ticks,
					SingleMediaSource: entry.Runtime.SingleMediaSource,
				}
			}
			entry.Live = entry.Live || event.Live
			if event.Position.valid() {
				entry.Position = playbackPositionCandidate{Session: session, Position: event.Position}
			}
		}
	}
	entry.Seen = time.Now()
	if event.Kind == playbackWatchStopped {
		state.TerminalSequence, state.TerminalSession = admission.Sequence, session
		state.TerminalGeneration = state.Generation
		state.Active = false
		for key, peer := range cache.entries {
			if samePlaybackWatchScope(snapshot.Key, key) {
				peer.Closed = true
				peer.Position = playbackPositionCandidate{}
			}
		}
	}
	return err
}

// Claim a proven successful Started before duration/metadata network work. This
// keeps its predecessor from writing while the new context is being enriched.
func (cache *playbackWatchCache) claimStartedLocked(admission *playbackWatchAdmission, state *playbackWatchSharedState, entry *playbackWatchCacheEntry, session playbackWatchSession) bool {
	if admission.Sequence <= state.StartedSequence ||
		(admission.Sequence <= state.TerminalSequence && samePlaybackWatchIdentity(session, state.TerminalSession)) {
		return false
	}
	state.Generation++
	state.StartedSequence = admission.Sequence
	state.LastCommitted = admission.Sequence
	state.Session, state.Active = session, true
	state.Detached = false
	state.RebasedFromRevision, state.RebasedRevision = -1, -1
	state.RebasedTokens = nil
	state.Seen = time.Now()
	admission.ConfirmedGeneration = state.Generation
	entry.Position = playbackPositionCandidate{}
	entry.Closed, entry.Active = false, true
	entry.LastCommitted = 0
	return true
}

func (cache *playbackWatchCache) confirmStarted(admission *playbackWatchAdmission, session playbackWatchSession) bool {
	if admission == nil || admission.Kind != playbackWatchStarted || !session.identified() {
		return false
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	state := cache.shared[admission.SharedKey]
	entry := cache.entries[admission.Snapshot.Key]
	if state != admission.State || state.ResetRevision != admission.ResetRevision || entry != admission.Snapshot.Entry {
		return false
	}
	return cache.claimStartedLocked(admission, state, entry, session)
}

func (a *App) confirmPlaybackWatchStarted(reqCtx *RequestContext, admission *playbackWatchAdmission, session playbackWatchSession) bool {
	a.watchLifecycleMu.RLock()
	defer a.watchLifecycleMu.RUnlock()
	if !a.mediaAccessScopeLocked(reqCtx).allows(session.Source.ServerID) ||
		a.watchItemOriginalID(admission.SharedKey.ItemID, session.Source.ServerID) != session.Source.OriginalItemID {
		return false
	}
	return a.watchPlayback.confirmStarted(admission, session)
}

func (cache *playbackWatchCache) invalidateShared(userID, itemID string) {
	key := playbackWatchSharedKey{UserID: userID, ItemID: itemID}
	state := cache.sharedState(key, time.Now())
	state.Generation++
	state.ResetRevision++
	state.Active = false
	state.Seen = time.Now()
}

// Production event writers consume only a pre-network admission. Direct calls
// without one cannot retroactively choose an owner after an asynchronous response.
func (a *App) admitPlaybackWatchRequest(r *http.Request, virtualItemID, playSessionID string, event playbackWatchEvent) *http.Request {
	if a.IDStore != nil && a.IDStore.ResolveVirtualID(virtualItemID) == nil && event.Source.MediaSourceID != "" {
		if id := a.IDStore.ResolveMergeMember(event.Source.ServerID, event.Source.OriginalItemID, event.Source.MediaSourceID); id != "" {
			virtualItemID = id
		}
	}
	reqCtx := requestContextFrom(r.Context())
	var admission *playbackWatchAdmission
	var lease *playbackWatchLeaseSnapshot
	if reqCtx != nil && reqCtx.ProxyUser != nil && reqCtx.ProxyUser.Role != "admin" {
		a.watchLifecycleMu.RLock()
		scope := a.mediaAccessScopeLocked(reqCtx)
		stableID, originalID := a.watchItemIdentity(virtualItemID, event.Source.ServerID)
		if scope.allows(event.Source.ServerID) && originalID != "" && originalID == event.Source.OriginalItemID &&
			(event.Source.MediaSourceID == "" || a.IDStore.MergeMemberAllowed(stableID, event.Source.ServerID, originalID, event.Source.MediaSourceID)) {
			key := playbackWatchCacheKey{Session: watchSession(reqCtx, event.Source, playSessionID), VirtualItemID: stableID}
			if a.WatchStore != nil {
				admission = a.watchPlayback.admit(key, event.Kind)
				if admission != nil {
					admission.UserRevision = reqCtx.ProxyUser.AuthRevision
					admission.Token = reqCtx.ProxyToken
				}
			}
			if event.Kind == playbackWatchStopped && a.PlaybackLimiter != nil {
				revision := a.PlaybackLimiter.PlaybackRevision(reqCtx.ProxyUser.UserID, event.Source.ServerID,
					playbackDeviceID(reqCtx), virtualItemID, playSessionID)
				if revision != 0 {
					lease = &playbackWatchLeaseSnapshot{UserID: reqCtx.ProxyUser.UserID, ItemID: virtualItemID,
						ServerID: event.Source.ServerID, DeviceID: playbackDeviceID(reqCtx),
						PlaySessionID: playSessionID, Revision: revision}
				}
			}
		}
		a.watchLifecycleMu.RUnlock()
	}
	ctx := context.WithValue(r.Context(), playbackWatchAdmissionContextKey{}, admission)
	ctx = context.WithValue(ctx, playbackWatchLeaseContextKey{}, lease)
	return r.WithContext(ctx)
}

func playbackWatchAdmissionFrom(r *http.Request) *playbackWatchAdmission {
	admission, _ := r.Context().Value(playbackWatchAdmissionContextKey{}).(*playbackWatchAdmission)
	return admission
}

// Hold the shared coordinator while ending the exact pre-network lease revision.
// An old A terminal may end A after B takes over; a new same-source generation or
// a later PlaybackInfo reservation cannot be ended by the preceding request.
func (a *App) stopAdmittedPlaybackLease(r *http.Request, serverID, playSessionID string) bool {
	lease, _ := r.Context().Value(playbackWatchLeaseContextKey{}).(*playbackWatchLeaseSnapshot)
	if lease == nil || lease.ServerID != serverID || lease.PlaySessionID != playSessionID ||
		a.PlaybackLimiter == nil {
		return false
	}
	admission := playbackWatchAdmissionFrom(r)
	a.watchLifecycleMu.RLock()
	defer a.watchLifecycleMu.RUnlock()
	a.watchPlayback.mu.Lock()
	defer a.watchPlayback.mu.Unlock()
	state := a.watchPlayback.shared[playbackWatchSharedKey{UserID: lease.UserID, ItemID: a.IDStore.CanonicalMergeID(lease.ItemID)}]
	if state != nil && state.Active && state.Session.Source.ServerID == serverID {
		if admission == nil || admission.State != state || admission.ResetRevision != state.ResetRevision ||
			admission.Generation != state.Generation || admission.Session.PlaySessionID != playSessionID {
			return false
		}
	}
	return a.PlaybackLimiter.StopPlaybackRevision(lease.UserID, serverID, lease.DeviceID,
		lease.ItemID, playSessionID, lease.Revision)
}

func sessionWatchEvent(body map[string]any, serverID string, kind playbackWatchEventKind) (playbackWatchEvent, string) {
	event := playbackWatchEventFromBody(kind, body)
	event.Source.ServerID = serverID
	event.Source.OriginalItemID, _ = body["ItemId"].(string)
	event.Source.MediaSourceID, _ = body["MediaSourceId"].(string)
	playSessionID, _ := body["PlaySessionId"].(string)
	return event, playSessionID
}

func (a *App) admitSessionWatchRequest(r *http.Request, virtualItemID string, body map[string]any, serverID string, kind playbackWatchEventKind) *http.Request {
	event, sessionID := sessionWatchEvent(body, serverID, kind)
	return a.admitPlaybackWatchRequest(r, virtualItemID, sessionID, event)
}
