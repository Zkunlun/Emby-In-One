package backend

import (
	"sync"
	"time"
)

type playbackRouteKey struct {
	Owner  string
	ItemID string
}

type playbackMediaRouteKey struct {
	Owner         string
	MediaSourceID string
}

type playbackRouteEntry struct {
	ItemID              string
	ServerID            string
	PlaySessionID       string
	ClientPlaySessionID string
	UpdatedAt           time.Time
}

type playbackRouteStore struct {
	mu          sync.RWMutex
	active      map[playbackRouteKey]playbackRouteEntry
	mediaSource map[playbackMediaRouteKey]playbackRouteEntry
	ownerUsers map[string]string // Real local user for exact cleanup of token-owned routes.
}

func newPlaybackRouteStore() *playbackRouteStore {
	return &playbackRouteStore{
		active:      map[playbackRouteKey]playbackRouteEntry{},
		mediaSource: map[playbackMediaRouteKey]playbackRouteEntry{},
	}
}

func playbackRouteOwner(reqCtx *RequestContext) string {
	if reqCtx == nil {
		return ""
	}
	if reqCtx.ProxyToken != "" {
		return "token:" + reqCtx.ProxyToken
	}
	if reqCtx.ProxyUser != nil && reqCtx.ProxyUser.UserID != "" {
		return "user:" + reqCtx.ProxyUser.UserID
	}
	return ""
}

func playbackRouteFresh(entry playbackRouteEntry, now time.Time) bool {
	return entry.ServerID != "" && now.Sub(entry.UpdatedAt) <= activeStreamTTL
}

func (s *playbackRouteStore) RememberMediaSource(owner, mediaSourceID, serverID, playSessionID, clientPlaySessionID string) {
	if s == nil || owner == "" || mediaSourceID == "" || serverID == "" {
		return
	}
	s.mu.Lock()
	s.mediaSource[playbackMediaRouteKey{Owner: owner, MediaSourceID: mediaSourceID}] = playbackRouteEntry{
		ServerID: serverID, PlaySessionID: playSessionID,
		ClientPlaySessionID: clientPlaySessionID, UpdatedAt: time.Now(),
	}
	s.mu.Unlock()
}

// RememberMediaSourceItem records authoritative source-to-item membership without
// replacing this source's already remembered PlaybackInfo session.
func (s *playbackRouteStore) RememberMediaSourceItem(owner, mediaSourceID, itemID, serverID string) {
	if s == nil || owner == "" || mediaSourceID == "" || itemID == "" || serverID == "" {
		return
	}
	key := playbackMediaRouteKey{Owner: owner, MediaSourceID: mediaSourceID}
	s.mu.Lock()
	entry := s.mediaSource[key]
	if !playbackRouteFresh(entry, time.Now()) || entry.ServerID != serverID || (entry.ItemID != "" && entry.ItemID != itemID) {
		entry = playbackRouteEntry{}
	}
	entry.ItemID = itemID
	entry.ServerID = serverID
	entry.UpdatedAt = time.Now()
	s.mediaSource[key] = entry
	s.mu.Unlock()
}

func (s *playbackRouteStore) MediaSource(owner, mediaSourceID string) (playbackRouteEntry, bool) {
	if s == nil || owner == "" || mediaSourceID == "" {
		return playbackRouteEntry{}, false
	}
	key := playbackMediaRouteKey{Owner: owner, MediaSourceID: mediaSourceID}
	now := time.Now()
	s.mu.RLock()
	entry, ok := s.mediaSource[key]
	s.mu.RUnlock()
	if !ok || !playbackRouteFresh(entry, now) {
		if ok {
			s.mu.Lock()
			delete(s.mediaSource, key)
			s.mu.Unlock()
		}
		return playbackRouteEntry{}, false
	}
	return entry, true
}

func (s *playbackRouteStore) Activate(owner, itemID, serverID, playSessionID, clientPlaySessionID string) {
	if s == nil || owner == "" || itemID == "" || serverID == "" {
		return
	}
	s.mu.Lock()
	s.active[playbackRouteKey{Owner: owner, ItemID: itemID}] = playbackRouteEntry{
		ServerID: serverID, PlaySessionID: playSessionID,
		ClientPlaySessionID: clientPlaySessionID, UpdatedAt: time.Now(),
	}
	s.mu.Unlock()
}

func (s *playbackRouteStore) Active(owner, itemID string) (playbackRouteEntry, bool) {
	if s == nil || owner == "" || itemID == "" {
		return playbackRouteEntry{}, false
	}
	key := playbackRouteKey{Owner: owner, ItemID: itemID}
	now := time.Now()
	s.mu.RLock()
	entry, ok := s.active[key]
	s.mu.RUnlock()
	if !ok || !playbackRouteFresh(entry, now) {
		if ok {
			s.mu.Lock()
			delete(s.active, key)
			s.mu.Unlock()
		}
		return playbackRouteEntry{}, false
	}
	return entry, true
}
