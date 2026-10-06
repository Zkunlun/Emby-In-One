package backend

import (
	"sync"
	"time"
)

const (
	playbackWatchCacheLimit = 1024
	playbackWatchFetchRetry = 30 * time.Second
)

type playbackWatchCacheKey struct {
	Session       playbackWatchSession
	VirtualItemID string
}

type playbackWatchCacheEntry struct {
	Metadata      WatchProgress
	Runtime       playbackRuntimeCandidate
	Position      playbackPositionCandidate
	Live          bool
	Closed        bool
	Active        bool
	FetchAttempts int
	Fetching      bool
	LastFetch     time.Time
	Seen          time.Time
	NextSequence  uint64
	LastCommitted uint64
}

type playbackWatchSnapshot struct {
	Key      playbackWatchCacheKey
	Entry    *playbackWatchCacheEntry
	Value    playbackWatchCacheEntry
	Sequence uint64
}

type playbackWatchCache struct {
	mu      sync.Mutex
	entries map[playbackWatchCacheKey]*playbackWatchCacheEntry
	shared map[playbackWatchSharedKey]*playbackWatchSharedState
}

func (cache *playbackWatchCache) prune(now time.Time) {
	cache.pruneShared(now)
	for key, entry := range cache.entries {
		if now.Sub(entry.Seen) > activeStreamTTL {
			delete(cache.entries, key)
		}
	}
}

func (cache *playbackWatchCache) room(now time.Time) {
	cache.prune(now)
	if cache.entries == nil {
		cache.entries = make(map[playbackWatchCacheKey]*playbackWatchCacheEntry)
	}
	if len(cache.entries) < playbackWatchCacheLimit {
		return
	}
	var oldestKey playbackWatchCacheKey
	var oldest *playbackWatchCacheEntry
	for key, entry := range cache.entries {
		if oldest == nil || entry.Seen.Before(oldest.Seen) {
			oldestKey, oldest = key, entry
		}
	}
	delete(cache.entries, oldestKey)
}

func samePlaybackWatchScope(a, b playbackWatchCacheKey) bool {
	return a.VirtualItemID == b.VirtualItemID &&
		a.Session.ProxyUserID == b.Session.ProxyUserID &&
		a.Session.PlaybackDeviceID == b.Session.PlaybackDeviceID &&
		a.Session.PlaySessionID == b.Session.PlaySessionID &&
		a.Session.Source.ServerID == b.Session.Source.ServerID &&
		a.Session.Source.OriginalItemID == b.Session.Source.OriginalItemID
}

func cachedWatchMediaSourceID(key playbackWatchCacheKey, entry *playbackWatchCacheEntry) string {
	if key.Session.Source.MediaSourceID != "" {
		return key.Session.Source.MediaSourceID
	}
	if entry.Runtime.SingleMediaSource {
		return entry.Runtime.Source.MediaSourceID
	}
	return ""
}

// PlaybackInfo preloads all versions. Only an actually observed active source,
// or an independently qualified single-source entry, may bridge an omitted ID.
func (cache *playbackWatchCache) activeKey(key playbackWatchCacheKey) playbackWatchCacheKey {
	if key.Session.PlaybackDeviceID == "" || key.Session.PlaySessionID == "" {
		return key
	}
	var selected playbackWatchCacheKey
	var latest *playbackWatchCacheEntry
	var mediaID string
	for candidate, entry := range cache.entries {
		id := cachedWatchMediaSourceID(candidate, entry)
		if !entry.Active || entry.Closed || id == "" || !samePlaybackWatchScope(key, candidate) ||
			(key.Session.Source.MediaSourceID != "" && key.Session.Source.MediaSourceID != id) {
			continue
		}
		if latest != nil && id != mediaID {
			return key // Multiple observed versions: the omitted ID is ambiguous.
		}
		if latest == nil || entry.Seen.After(latest.Seen) {
			selected, latest, mediaID = candidate, entry, id
		}
	}
	if latest != nil {
		return selected
	}
	return key
}

func (cache *playbackWatchCache) begin(key playbackWatchCacheKey, kind playbackWatchEventKind) (playbackWatchSnapshot, bool) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	now := time.Now()
	cache.prune(now)
	if kind != playbackWatchStarted {
		key = cache.activeKey(key)
	}
	entry := cache.entries[key]
	if entry != nil && entry.Closed && kind != playbackWatchStarted {
		return playbackWatchSnapshot{}, false
	}
	if entry != nil && kind == playbackWatchStarted {
		// Every new start clears old positions and supersedes in-flight work,
		// even if the client reused an ID after failing to report a prior stop.
		entry = &playbackWatchCacheEntry{
			Metadata: entry.Metadata, Runtime: entry.Runtime, Live: entry.Live,
			LastFetch: entry.LastFetch, FetchAttempts: entry.FetchAttempts,
		}
		cache.entries[key] = entry
	}
	if entry == nil {
		cache.room(now)
		entry = &playbackWatchCacheEntry{}
		cache.entries[key] = entry
	}
	entry.Seen = now
	entry.NextSequence++
	return playbackWatchSnapshot{Key: key, Entry: entry, Value: *entry, Sequence: entry.NextSequence}, true
}

func (cache *playbackWatchCache) claimFetch(snapshot playbackWatchSnapshot, kind playbackWatchEventKind) bool {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	entry := cache.entries[snapshot.Key]
	if entry != snapshot.Entry || (entry.Closed && kind != playbackWatchStarted) || entry.Fetching {
		return false
	}
	now := time.Now()
	if !entry.LastFetch.IsZero() &&
		(now.Sub(entry.LastFetch) < playbackWatchFetchRetry ||
			(kind == playbackWatchProgress && entry.FetchAttempts >= 3)) {
		return false
	}
	entry.Fetching, entry.LastFetch = true, now
	entry.FetchAttempts++
	return true
}

func (cache *playbackWatchCache) finishFetch(snapshot playbackWatchSnapshot, metadata WatchProgress, runtime playbackRuntimeCandidate, live, success bool) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	entry := cache.entries[snapshot.Key]
	if entry != snapshot.Entry {
		return
	}
	entry.Fetching = false
	if !success || entry.Closed {
		return
	}
	entry.Metadata = metadata
	if runtime.Source.valid() {
		entry.Runtime = runtime
	}
	entry.Live = entry.Live || live
}

// No network access is allowed inside commit/mutate. Both take cache.mu before
// WatchStore.mu, so an explicit reset cannot race an old snapshot's database write.
func (cache *playbackWatchCache) commit(snapshot playbackWatchSnapshot, metadata WatchProgress, event playbackWatchEvent, session playbackWatchSession, write func() error) error {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	entry := cache.entries[snapshot.Key]
	if entry != snapshot.Entry || entry.Closed ||
		(snapshot.Sequence < entry.LastCommitted && event.Kind != playbackWatchStopped) {
		return nil
	}
	if err := write(); err != nil {
		return err
	}
	// A terminal event must still close its own entry if a later progress write
	// finished while metadata was being fetched. A new start has a new pointer.
	if snapshot.Sequence > entry.LastCommitted {
		entry.LastCommitted = snapshot.Sequence
	}
	entry.Active = true
	entry.Seen = time.Now()
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
	if event.Position.valid() && session.identified() {
		entry.Position = playbackPositionCandidate{Session: session, Position: event.Position}
	}
	if event.Kind == playbackWatchStopped {
		entry.Position = playbackPositionCandidate{}
		// Keep a bounded tombstone so delayed progress cannot revive this session.
		// Without a session ID, subsequent explicit progress cannot be classified
		// as belonging to an old playback and must remain usable.
		entry.Closed = session.PlaySessionID != ""
		if entry.Closed && session.PlaybackDeviceID != "" {
			for key, peer := range cache.entries {
				if samePlaybackWatchScope(snapshot.Key, key) {
					peer.Closed = true
					peer.Position = playbackPositionCandidate{}
				}
			}
		}
	}
	return nil
}

func (cache *playbackWatchCache) mutate(userID, virtualItemID string, reset bool, write func() error) error {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if reset {
		cache.invalidateShared(userID, virtualItemID)
		for key := range cache.entries {
			if key.Session.ProxyUserID == userID && key.VirtualItemID == virtualItemID {
				delete(cache.entries, key)
			}
		}
	}
	return write()
}

func (cache *playbackWatchCache) remember(key playbackWatchCacheKey, metadata WatchProgress, runtime playbackRuntimeCandidate, live bool) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	now := time.Now()
	cache.prune(now)
	entry := cache.entries[key]
	if entry == nil || entry.Closed {
		cache.room(now)
		entry = &playbackWatchCacheEntry{}
		cache.entries[key] = entry
	}
	entry.Seen = now
	if metadata.ItemType != "" {
		entry.Metadata = metadata
	}
	if runtime.Source.valid() {
		entry.Runtime = runtime
	}
	entry.Live = entry.Live || live
}
