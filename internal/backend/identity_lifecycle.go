package backend

import "net/http"

// Source namespace follows the persistent server identity; the session pool uses
// a separate connection key so ordinary URL/profile changes still rebuild login.
func (s *ClientIdentityService) configureSources(sources []UpstreamConfig) {
 defer s.notifyCountsIdentity()
 s.mu.Lock()
 defer s.mu.Unlock()
 next := map[string]bool{}
 legacy := map[string]string{}
 for _, source := range sources {
  key := StableUpstreamKey(source)
  next[key] = true
  oldKey := legacyUpstreamKey(source)
  if _, duplicate := legacy[oldKey]; duplicate { legacy[oldKey] = "" } else { legacy[oldKey] = key }
 }
 s.configuredSources, s.legacySourceOwners = next, legacy
 s.lifecycleEpoch++
}

func (s *ClientIdentityService) captureEpoch() uint64 {
 if s == nil { return 0 }
 s.mu.RLock()
 defer s.mu.RUnlock()
 return s.lifecycleEpoch
}

// Serialize file publication with memory cleanup, and fence the network attempt
// by the epoch captured before it started. A removed source/user cannot return
// late and put its cached identity back.
func (s *ClientIdentityService) saveSuccessfulIdentity(key string, headers http.Header, promote bool, epoch uint64, owner string) {
 defer s.notifyCountsIdentity()
 if s == nil || key == "" { return }
 entry := s.newCapturedEntry(headers)
 s.mu.Lock()
 defer s.mu.Unlock()
 if epoch != s.lifecycleEpoch || (s.configuredSources != nil && !s.configuredSources[key]) { return }
 if owner == "" {
  if previous, ok := s.lastSuccessByServer[key]; ok { owner = previous.ownerUserID }
  if owner == "" && s.latestCaptured != nil { owner = s.latestCaptured.ownerUserID }
 }
 if owner != "" && s.ownerAllowed != nil && !s.ownerAllowed(owner) { return }
 entry.ownerUserID, entry.ownerServerID = owner, key
 s.lastSuccessByServer[key] = entry
 if promote { s.latestCaptured = cloneCapturedEntry(entry) }
 if s.persistence == nil { return }
 _ = s.persistence.updateSnapshot(func(snapshot *identityPersistenceSnapshot) {
  snapshot.LastSuccessByServer[key] = persistedIdentityEntry(entry)
  if promote { latest := persistedIdentityEntry(entry); snapshot.LatestCaptured = &latest }
 })
}

func persistedIdentityEntry(entry capturedEntry) persistedCapturedHeaders {
 return persistedCapturedHeaders{Headers: normalizeCapturedHeaders(entry.headers), CapturedAt: entry.capturedAt,
  OwnerUserID: entry.ownerUserID, OwnerServerID: entry.ownerServerID}
}

func (p *IdentityPersistence) updateSnapshot(update func(*identityPersistenceSnapshot)) error {
 p.mu.Lock()
 defer p.mu.Unlock()
 snapshot, err := p.loadLocked()
 if err != nil { return err }
 update(&snapshot)
 snapshot.Version = 2
 return p.saveLocked(snapshot)
}

// Migration is performed after pending deletions have purged their legacy keys.
// Ambiguous/unowned address keys are discarded, never imported into a new ID.
func (s *ClientIdentityService) migrateSourceOwnership(sources []UpstreamConfig) error {
 if s == nil { return nil }
 owners := map[string][]string{}
 live := map[string]bool{}
 for _, source := range sources {
  key := StableUpstreamKey(source)
  live[key] = true
  legacy := legacyUpstreamKey(source)
  owners[legacy] = append(owners[legacy], key)
 }
 s.mu.Lock()
 defer s.mu.Unlock()
 migrate := func(old map[string]persistedCapturedHeaders) map[string]persistedCapturedHeaders {
  next := map[string]persistedCapturedHeaders{}
  for key, entry := range old {
   if live[key] { entry.OwnerServerID = key; next[key] = entry }
  }
  for key, entry := range old {
   matches := owners[key]
   if len(matches) != 1 { continue }
   target := matches[0]
   if _, exists := next[target]; exists { continue }
   entry.OwnerServerID = target
   next[target] = entry
  }
  return next
 }
 if s.persistence != nil {
  if err := s.persistence.updateSnapshot(func(snapshot *identityPersistenceSnapshot) {
   snapshot.LastSuccessByServer = migrate(snapshot.LastSuccessByServer)
  }); err != nil { return err }
  snapshot, err := s.persistence.Load()
  if err != nil { return err }
  s.lastSuccessByServer = map[string]capturedEntry{}
  s.latestCaptured, s.latestInfo = nil, nil
  s.applyPersistenceSnapshot(snapshot)
 } else {
  old := map[string]persistedCapturedHeaders{}
  for key, entry := range s.lastSuccessByServer { old[key] = persistedIdentityEntry(entry) }
  s.lastSuccessByServer = map[string]capturedEntry{}
  for key, entry := range migrate(old) {
   captured := s.hydrateCapturedEntry(entry.Headers, entry.CapturedAt)
   captured.ownerUserID, captured.ownerServerID = entry.OwnerUserID, entry.OwnerServerID
   s.lastSuccessByServer[key] = captured
  }
 }
 return nil
}

func (s *ClientIdentityService) cleanupLifecycle(op watchCleanupOperation) error {
 defer s.notifyCountsIdentity()
 if s == nil { return nil }
 if op.Kind == "user_update" && len(op.Users) == 0 { return nil }
 s.mu.Lock()
 defer s.mu.Unlock()
 s.lifecycleEpoch++ // Every outstanding capture/login publication is now stale.
 drop := func(user, source string) bool {
  if op.Kind == "server_delete" {
   if source == "" { return user == "" || containsString(op.Users, user) }
   return source == "id:"+op.Target
  }
  if op.Kind == "user_delete" || op.ClearUser { return user == "" || user == op.Target }
  // A binding change clears captures that cannot be attributed to a retained source.
  if user != "" && user != op.Target { return false }
  if source == "" { return true }
  for _, removed := range op.Sources { if source == "id:"+removed { return true } }
  return false
 }
 for token, entry := range s.entries {
  if drop(entry.ownerUserID, entry.ownerServerID) { delete(s.entries, token) }
 }
 if s.latestInfo != nil && drop(s.latestInfo.ownerUserID, s.latestInfo.ownerServerID) { s.latestInfo = nil }
 if s.latestCaptured != nil && drop(s.latestCaptured.ownerUserID, s.latestCaptured.ownerServerID) { s.latestCaptured = nil }
 for key, entry := range s.lastSuccessByServer {
  source := entry.ownerServerID
  if source == "" && len(key) > 3 && key[:3] == "id:" { source = key }
  if source == "" { source = s.legacySourceOwners[key] }
  if drop(entry.ownerUserID, source) || (op.Kind == "server_delete" && legacyIdentityDigest(key) == op.LegacyIdentityDigest) {
   delete(s.lastSuccessByServer, key)
  }
 }
 if s.persistence == nil { return nil }
 return s.persistence.updateSnapshot(func(snapshot *identityPersistenceSnapshot) {
  if snapshot.LatestCaptured != nil && drop(snapshot.LatestCaptured.OwnerUserID, snapshot.LatestCaptured.OwnerServerID) {
   snapshot.LatestCaptured = nil
  }
  for key, entry := range snapshot.LastSuccessByServer {
   source := entry.OwnerServerID
   if source == "" && len(key) > 3 && key[:3] == "id:" { source = key }
   if source == "" { source = s.legacySourceOwners[key] }
   if drop(entry.OwnerUserID, source) || (op.Kind == "server_delete" && legacyIdentityDigest(key) == op.LegacyIdentityDigest) {
    delete(snapshot.LastSuccessByServer, key)
   }
  }
 })
}

func (a *App) installIdentityLifecycle() {
 if a.Identity == nil { return }
 adminID := a.Auth.ProxyUserID()
 a.Identity.ownerAllowed = func(id string) bool {
  if id == adminID { return true }
  if a.UserStore == nil { return false }
  user := a.UserStore.Get(id)
  return user != nil && user.Enabled
 }
 a.Identity.captureRequest = func(token string, headers http.Header) *RequestContext {
  a.watchLifecycleMu.RLock()
  defer a.watchLifecycleMu.RUnlock()
  info := a.Auth.ValidateToken(token)
  if info == nil || a.lifecyclePending { return nil }
  var epochs map[string]int64
  if a.IDStore!=nil && a.ConfigStore!=nil {
   epochs=a.IDStore.snapshotSourceGenerations(configuredSourceIDs(a.ConfigStore.Snapshot()))
  }
  return &RequestContext{Headers: cloneHeader(headers), ProxyToken: token, ProxyUser: info,
   PlaybackDeviceID: resolvePlaybackDeviceID(headers, info.DeviceID),
   SourceGenerations:epochs, SourceGenerationCaptured:true}
 }
 a.Identity.capturePublish = func(token string, publish func(string)) bool {
  a.watchLifecycleMu.RLock()
  defer a.watchLifecycleMu.RUnlock()
  info := a.Auth.ValidateToken(token)
  if info == nil || a.lifecyclePending { return false }
  publish(info.UserID)
  return true
 }
}
