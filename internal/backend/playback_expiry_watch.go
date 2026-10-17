package backend

import "time"

// settleExpiredPlaybackLeases runs from the existing server maintenance goroutine.
// All limiter entry-points enqueue expiry snapshots. Drain them outside its mutex
// and never issue an artificial Stopped request to an upstream Emby server.
func (a *App) settleExpiredPlaybackLeases() {
	if a == nil || a.PlaybackLimiter == nil {
		return
	}
	a.PlaybackLimiter.Cleanup()
	expired := a.PlaybackLimiter.takeExpired()
	for _, lease := range expired {
		a.settleExpiredPlaybackLease(lease)
	}
}

// Only already-confirmed, version-qualified watch progress may close a session.
// The existing coordinator lock fences all in-flight Progress/Stopped commits.
func (a *App) settleExpiredPlaybackLease(lease expiredPlaybackLease) {
	if a.WatchStore == nil || a.IDStore == nil || a.UserStore == nil || a.ConfigStore == nil {
		return
	}
	if lease.Key.UserID == "" || lease.Key.ServerID == "" ||
		lease.Entry.DeviceID == "" || lease.Entry.ItemID == "" || lease.Entry.PlaySessionID == "" {
		return
	}
	a.watchLifecycleMu.RLock()
	defer a.watchLifecycleMu.RUnlock()
	if a.lifecyclePending {
		return
	}
	user := a.UserStore.Get(lease.Key.UserID)
	if user == nil || !user.Enabled ||
		!containsString(user.AllowedServers, lease.Key.ServerID) ||
		!configuredSourceIDs(a.ConfigStore.Snapshot())[lease.Key.ServerID] {
		return
	}
	itemID, originalID := a.watchItemIdentity(lease.Entry.ItemID, lease.Key.ServerID)
	if itemID == "" || originalID == "" {
		return
	}

	cache := &a.watchPlayback
	cache.mu.Lock()
	defer cache.mu.Unlock()
	state := cache.shared[playbackWatchSharedKey{UserID: lease.Key.UserID, ItemID: itemID}]
	if state == nil || !state.Active || state.Detached || state.PendingStopped {
		return
	}
	// A newer lease by itself does not mean playback has started: settle the
	// expired owner unless an incoming new event is still awaiting confirmation.
	// A newer confirmed session is rejected by the identity checks below.
	if a.PlaybackLimiter.hasCurrentLease(lease.Key) && state.NextSequence > state.LastCommitted {
		return
	}
	session := state.Session
	if !session.identified() || session.ProxyUserID != lease.Key.UserID ||
		session.PlaybackDeviceID != lease.Entry.DeviceID ||
		session.PlaySessionID != lease.Entry.PlaySessionID ||
		session.Source.ServerID != lease.Key.ServerID ||
		session.Source.OriginalItemID != originalID ||
		!a.IDStore.MergeMemberAllowed(itemID, lease.Key.ServerID, originalID, session.Source.MediaSourceID) {
		return
	}
	// Locate precisely the last committed media-source version. A different
	// version, outdated session or unqualified duration is never a completion.
	key := playbackWatchCacheKey{Session: session, VirtualItemID: itemID}
	entry := cache.entries[key]
	if entry == nil || !entry.Active || entry.Closed {
		// The cache may have evicted the source proof. It is still the expired
		// generation, so fence any later events without guessing a position.
		cache.closeExpiredWatchSession(state, key)
		return
	}
	if entry.Position.Session != session || !entry.Position.Position.valid() ||
		!entry.Runtime.matches(session.Source) || entry.Runtime.Ticks <= 0 ||
		entry.Live || entry.Metadata.ProxyUserID != lease.Key.UserID ||
		entry.Metadata.VirtualItemID != itemID ||
		entry.Metadata.ServerID != lease.Key.ServerID ||
		entry.Metadata.OriginalItemID != originalID {
		// No reliable completion evidence. Preserve existing progress, but
		// close the expired generation so late replies cannot write into it.
		cache.closeExpiredWatchSession(state, key)
		return
	}
	event := playbackWatchEvent{
		Kind:     playbackWatchStopped,
		Source:   session.Source,
		Position: entry.Position.Position,
		Runtime:  validPlaybackTicks(entry.Runtime.Ticks),
		Live:     entry.Live,
	}
	metadata := entry.Metadata
	if event.completionCandidate() && autoPlayedItemType(metadata.ItemType) {
		if err := a.WatchStore.SeedMergeState(lease.Key.UserID, itemID); err != nil {
			if a.Logger != nil {
				a.Logger.Warnf("watch lease expiry seed error: %s", redactURLInError(err))
			}
			a.PlaybackLimiter.retryExpired(lease)
			return
		}
		if err := a.WatchStore.RecordPlaybackProgress(&metadata, event); err != nil {
			if a.Logger != nil {
				a.Logger.Warnf("watch lease expiry record error: %s", redactURLInError(err))
			}
			a.PlaybackLimiter.retryExpired(lease)
			return
		}
	}
	cache.closeExpiredWatchSession(state, key)
}

func (cache *playbackWatchCache) closeExpiredWatchSession(state *playbackWatchSharedState, key playbackWatchCacheKey) {
	state.Active = false
	state.PendingStopped = false
	state.TerminalSequence, state.TerminalSession = state.NextSequence, state.Session
	state.TerminalGeneration = state.Generation
	state.Seen = time.Now()
	for candidate, entry := range cache.entries {
		if samePlaybackWatchScope(key, candidate) {
			entry.Closed = true
			entry.Position = playbackPositionCandidate{}
		}
	}
}
