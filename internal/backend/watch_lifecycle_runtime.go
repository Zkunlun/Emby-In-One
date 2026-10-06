package backend

import "time"

func (c *UpstreamClient) isRetired() bool {
 c.mu.RLock()
 defer c.mu.RUnlock()
 return c.retired
}

func (s *playbackRouteStore) RegisterOwner(owner, userID string) {
 if s == nil || owner == "" || userID == "" { return }
 s.mu.Lock()
 defer s.mu.Unlock()
 if s.ownerUsers == nil { s.ownerUsers = map[string]string{} }
 s.ownerUsers[owner] = userID
}

func (s *playbackRouteStore) RemoveLifecycle(userID, serverID string) {
 if s == nil { return }
 s.mu.Lock()
 defer s.mu.Unlock()
 matches := func(owner, source string) bool {
  return (userID == "" || s.ownerUsers[owner] == userID || owner == "user:"+userID) &&
   (serverID == "" || source == serverID)
 }
 for key, entry := range s.active { if matches(key.Owner, entry.ServerID) { delete(s.active, key) } }
 for key, entry := range s.mediaSource { if matches(key.Owner, entry.ServerID) { delete(s.mediaSource, key) } }
 used := map[string]bool{}
 for key := range s.active { used[key.Owner] = true }
 for key := range s.mediaSource { used[key.Owner] = true }
 for owner := range s.ownerUsers { if !used[owner] { delete(s.ownerUsers, owner) } }
 if serverID == "" {
  for owner, user := range s.ownerUsers { if userID == "" || user == userID { delete(s.ownerUsers, owner) } }
 }
}

func (l *PlaybackLimiter) RemoveLifecycle(userID, serverID string) {
 if l == nil { return }
 l.mu.Lock()
 defer l.mu.Unlock()
 for key := range l.streams {
  if (userID == "" || key.UserID == userID) && (serverID == "" || key.ServerID == serverID) {
   delete(l.streams, key)
  }
 }
}

// Detach removes all source identity from the coordinator, retaining only the
// short-lived user/film/generation ordering proof consumed by existing requests.
func (cache *playbackWatchCache) cleanLifecycle(userID, serverID string, detach bool) {
 cache.mu.Lock()
 defer cache.mu.Unlock()
 for key := range cache.entries {
  if (userID == "" || key.Session.ProxyUserID == userID) &&
   (serverID == "" || key.Session.Source.ServerID == serverID) { delete(cache.entries, key) }
 }
 for key, state := range cache.shared {
  if userID != "" && key.UserID != userID { continue }
  if serverID != "" && state.TerminalSession.Source.ServerID == serverID {
   state.TerminalSession = playbackWatchSession{}
  }
  if serverID != "" && state.Session.Source.ServerID != serverID { continue }
  if detach {
   state.Session = playbackWatchSession{}
   state.TerminalSession = playbackWatchSession{}
   state.Detached = true
   state.Seen = time.Now()
  } else {
   state.Generation++
   state.ResetRevision++
   state.Active = false
   state.Detached = false
   state.Session = playbackWatchSession{}
   state.TerminalSession = playbackWatchSession{}
   state.RebasedFromRevision, state.RebasedRevision = -1, -1
   state.RebasedTokens = nil
   if serverID == "" { delete(cache.shared, key) }
  }
 }
}

func (a *App) dropMissingWatchOwners() {
 a.watchPlayback.mu.Lock()
 defer a.watchPlayback.mu.Unlock()
 for key := range a.watchPlayback.shared {
  if a.IDStore.ResolveVirtualID(key.ItemID) == nil { delete(a.watchPlayback.shared, key) }
 }
}

// Server-grant removal is the only transition that can preserve already
// authenticated, admitted requests. Later password/disable/binding revisions
// cannot match this expected revision; newly arriving revoked tokens still fail.
func (a *App) markCleanupRevisions(userIDs []string) {
 users := make(map[string]*User, len(userIDs))
 for _, id := range userIDs { users[id] = a.UserStore.Get(id) }
 issued := map[string]map[string]bool{}
 if a.Auth != nil {
  a.Auth.mu.RLock()
  for token, info := range a.Auth.tokens {
   user := users[info.UserID]
   if user == nil || info.AuthRevision != user.AuthRevision - 1 { continue }
   if issued[info.UserID] == nil { issued[info.UserID] = map[string]bool{} }
   issued[info.UserID][legacyIdentityDigest(token)] = true
  }
  a.Auth.mu.RUnlock()
 }
 a.watchPlayback.mu.Lock()
 defer a.watchPlayback.mu.Unlock()
 for key, state := range a.watchPlayback.shared {
  user := users[key.UserID]
  if user == nil { continue }
  previous := user.AuthRevision - 1
  if state.RebasedRevision == user.AuthRevision { continue }
  if state.RebasedRevision == previous && state.RebasedFromRevision >= 0 {
   state.RebasedRevision = user.AuthRevision
  } else {
   state.RebasedFromRevision, state.RebasedRevision = previous, user.AuthRevision
   state.RebasedTokens = issued[key.UserID]
  }
 }
}

// Caller owns the lifecycle read gate. No network or new source identity is
// created here; the B locator comes solely from the surviving film mapping.
func (a *App) inheritedWatchTargetLocked(reqCtx *RequestContext, admission *playbackWatchAdmission, metadata WatchProgress, event playbackWatchEvent) (WatchProgress, bool) {
 if admission == nil || reqCtx == nil || reqCtx.ProxyUser == nil || a.lifecyclePending ||
  admission.UserRevision != reqCtx.ProxyUser.AuthRevision || admission.Token != reqCtx.ProxyToken ||
  configuredSourceIDs(a.ConfigStore.Snapshot())[event.Source.ServerID] { return metadata, false }
 a.watchPlayback.mu.Lock()
 state := a.watchPlayback.shared[admission.SharedKey]
 eligible := state == admission.State && state != nil && state.ResetRevision == admission.ResetRevision &&
  state.RebasedFromRevision == admission.UserRevision && state.RebasedRevision > admission.UserRevision &&
  admission.Token != "" && state.RebasedTokens[legacyIdentityDigest(admission.Token)]
 expectedRevision := int64(-1)
 if eligible { expectedRevision = state.RebasedRevision }
 a.watchPlayback.mu.Unlock()
 if !eligible { return metadata, false }
 user := a.UserStore.Get(admission.SharedKey.UserID)
 if user == nil || !user.Enabled || user.AuthRevision != expectedRevision { return metadata, false }
 resolved := a.IDStore.ResolveVirtualID(admission.SharedKey.ItemID)
 if resolved == nil { return metadata, false }
 scope := mediaAccessScope{allowed: map[string]struct{}{}}
 for _, source := range a.ConfigStore.Snapshot().Upstream {
  if containsString(user.AllowedServers, source.ID) && containsString(reqCtx.ProxyUser.AllowedServers, source.ID) {
   scope.allowed[source.ID] = struct{}{}
  }
 }
 instances := authorizedMediaInstances(scope, resolved)
 if len(instances) == 0 { return metadata, false }
 metadata.ServerID, metadata.OriginalItemID = instances[0].ServerID, instances[0].OriginalID
 metadata.RuntimeTicks = 0 // A's reliable event duration must not become B's duration.
 metadata.SeriesOriginalID = ""
 if metadata.SeriesVirtualID != "" {
  series := a.IDStore.ResolveVirtualID(metadata.SeriesVirtualID)
  if series == nil { metadata.SeriesVirtualID = "" } else {
   metadata.SeriesOriginalID = resolvedOriginalIDForServer(series, metadata.ServerID)
  }
 }
 return metadata, true
}

// Inheritance consumes only an existing admission; it never retroactively admits
// a raw A event or recreates a vanished film. The caller has checked current user,
// the deletion revision, surviving authorized mapping and A version evidence.
func (cache *playbackWatchCache) commitInherited(admission *playbackWatchAdmission, event playbackWatchEvent, session playbackWatchSession, write func() error) error {
 if admission == nil || !session.identified() || event.Kind != admission.Kind ||
  !samePlaybackWatchIdentity(session, admission.Session) ||
  (admission.Session.Source.MediaSourceID != "" && admission.Session.Source.MediaSourceID != session.Source.MediaSourceID) { return nil }
 cache.mu.Lock()
 defer cache.mu.Unlock()
 state := cache.shared[admission.SharedKey]
 if state == nil || state != admission.State || state.ResetRevision != admission.ResetRevision ||
  state.RebasedFromRevision != admission.UserRevision { return nil }
 if event.Kind == playbackWatchStarted {
  if event.Failed || admission.Sequence < state.StartedSequence { return nil }
  if admission.ConfirmedGeneration != 0 {
   if state.Generation != admission.ConfirmedGeneration || !state.Active || !state.Detached { return nil }
  } else {
   if admission.Sequence <= state.StartedSequence ||
    (admission.Sequence <= state.TerminalSequence && admission.RestartedSameContext && state.TerminalGeneration == admission.Generation) { return nil }
   state.Generation++
   state.StartedSequence, state.LastCommitted = admission.Sequence, admission.Sequence
   state.Session = playbackWatchSession{}
   state.TerminalSession = playbackWatchSession{}
   state.Active, state.Detached = true, true
   admission.ConfirmedGeneration = state.Generation
  }
 } else if !state.Active || !state.Detached || state.Generation != admission.Generation { return nil }
 stale := admission.Sequence < state.LastCommitted ||
  (event.Kind != playbackWatchStarted && admission.Sequence == state.LastCommitted)
 var err error
 if !stale {
  err = write()
  if err == nil { state.LastCommitted = admission.Sequence }
 }
 state.Seen = time.Now()
 if event.Kind == playbackWatchStopped {
  state.Active = false
  state.TerminalSequence = admission.Sequence
  state.TerminalGeneration = state.Generation
 }
 return err
}
