package backend

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// No credentials or full configuration are written to this journal.
type watchCleanupOperation struct {
	ID                    string
	Kind                  string
	Target                string
	Phase                 string
	Users                 []string `json:"users,omitempty"`
	Sources               []string `json:"sources,omitempty"`
	ClearUser             bool     `json:"clearUser,omitempty"`
	LegacyIdentityDigest  string   `json:"legacyIdentityDigest,omitempty"`
	CountsBindingsChanged bool     `json:"countsBindingsChanged,omitempty"`
}

func (a *App) lifecycleDB() (*sqliteDB, error) {
	if a.IDStore == nil || a.UserStore == nil || a.WatchStore == nil || a.HiddenLibraries == nil {
		return nil, fmt.Errorf("persistent lifecycle stores are unavailable")
	}
	db := a.IDStore.db
	if db == nil || a.UserStore.db != db || a.WatchStore.db != db || a.HiddenLibraries.db != db {
		return nil, fmt.Errorf("lifecycle stores do not share a persistent database")
	}
	return db, nil
}

func (a *App) initializeWatchLifecycle() error {
	db, err := a.lifecycleDB()
	if err != nil {
		return err
	}
	return db.writeParams(`CREATE TABLE IF NOT EXISTS pending_watch_cleanup (
  operation_id TEXT PRIMARY KEY, kind TEXT NOT NULL, target_id TEXT NOT NULL,
  phase TEXT NOT NULL, payload TEXT NOT NULL)`)
}

// Caller owns the lifecycle write gate. This matches every request-side writer;
// public store methods must never be called while these locks are held.
func (a *App) lockLifecycleStores() func() {
	a.watchPlayback.mu.Lock()
	a.UserStore.mu.Lock()
	a.IDStore.mu.Lock()
	a.WatchStore.mu.Lock()
	a.HiddenLibraries.writeMu.Lock()
	return func() {
		a.HiddenLibraries.writeMu.Unlock()
		a.WatchStore.mu.Unlock()
		a.IDStore.mu.Unlock()
		a.UserStore.mu.Unlock()
		a.watchPlayback.mu.Unlock()
	}
}

func writeCleanupOperation(db *sqliteDB, op watchCleanupOperation) error {
	payload, err := json.Marshal(op)
	if err != nil {
		return err
	}
	return db.execParams(`INSERT INTO pending_watch_cleanup
 (operation_id, kind, target_id, phase, payload) VALUES (?, ?, ?, ?, ?)
 ON CONFLICT(operation_id) DO UPDATE SET phase = excluded.phase, payload = excluded.payload`,
		op.ID, op.Kind, op.Target, op.Phase, string(payload))
}

func (a *App) pendingCleanupOperations() ([]watchCleanupOperation, error) {
	db, err := a.lifecycleDB()
	if err != nil {
		return nil, err
	}
	stmt, err := db.prepare(`SELECT operation_id, kind, target_id, phase, payload
 FROM pending_watch_cleanup ORDER BY operation_id`)
	if err != nil {
		return nil, err
	}
	defer stmt.finalize()
	var result []watchCleanupOperation
	for {
		row, err := stmt.step()
		if err != nil {
			return nil, err
		}
		if !row {
			break
		}
		var op watchCleanupOperation
		if err := json.Unmarshal([]byte(stmt.columnText(4)), &op); err != nil {
			return nil, err
		}
		op.ID, op.Kind, op.Target, op.Phase = stmt.columnText(0), stmt.columnText(1), stmt.columnText(2), stmt.columnText(3)
		if op.ID == "" || op.Target == "" ||
			(op.Kind != "server_delete" && op.Kind != "user_delete" && op.Kind != "user_update") ||
			(op.Phase != "prepared" && op.Phase != "committed") {
			return nil, fmt.Errorf("invalid pending lifecycle operation")
		}
		result = append(result, op)
	}
	return result, nil
}

func configuredSourceIDs(cfg Config) map[string]bool {
	result := make(map[string]bool, len(cfg.Upstream))
	for _, source := range cfg.Upstream {
		if source.ID != "" {
			result[source.ID] = true
		}
	}
	return result
}

// Rebuild only after commit, with all stores still excluded. On failure the
// pending operation remains and request authorization stays closed.
func (a *App) reloadLifecycleStoresLocked() error {
	if err := a.UserStore.loadAll(); err != nil {
		return err
	}
	a.IDStore.virtualToOriginal = map[string]*idEntry{}
	a.IDStore.originalToVirtual = map[string]string{}
	a.IDStore.originalIDToVirtual = map[string][]string{}
	if err := a.IDStore.load(); err != nil {
		return err
	}
	return a.HiddenLibraries.load()
}

func (a *App) applyServerCleanupSQL(db *sqliteDB, op watchCleanupOperation) error {
	allowed := configuredSourceIDs(a.ConfigStore.Snapshot())
	affected := make(map[string]bool)
	// The in-memory mappings are the committed snapshot while these locks are held.
	for virtualID, entry := range a.IDStore.virtualToOriginal {
		instances := append([]AdditionalInstance{{OriginalID: entry.OriginalID, ServerID: entry.ServerID}}, entry.OtherInstances...)
		remaining := make([]AdditionalInstance, 0, len(instances))
		touched := false
		for _, instance := range instances {
			if instance.ServerID == op.Target {
				touched = true
				continue
			}
			if allowed[instance.ServerID] && instance.OriginalID != "" {
				remaining = appendIfMissing(remaining, instance)
			}
		}
		if !touched {
			continue
		}
		affected[virtualID] = true
		if len(remaining) == 0 {
			for _, query := range []string{
				`DELETE FROM user_watch_progress WHERE virtual_item_id = ?`,
				`DELETE FROM id_additional_instances WHERE virtual_id = ?`,
				`DELETE FROM id_mappings WHERE virtual_id = ?`,
			} {
				if err := db.execParams(query, virtualID); err != nil {
					return err
				}
			}
			continue
		}
		primary := remaining[0]
		if err := db.execParams(`UPDATE id_mappings SET original_id = ?, server_id = ? WHERE virtual_id = ?`,
			primary.OriginalID, primary.ServerID, virtualID); err != nil {
			return err
		}
		if err := db.execParams(`DELETE FROM id_additional_instances WHERE virtual_id = ?`, virtualID); err != nil {
			return err
		}
		for _, instance := range remaining[1:] {
			if err := db.execParams(`INSERT INTO id_additional_instances (virtual_id, original_id, server_id) VALUES (?, ?, ?)`,
				virtualID, instance.OriginalID, instance.ServerID); err != nil {
				return err
			}
		}
	}
	// Include rows whose last playback used A even when A was a secondary instance.
	if err := db.execParams(`DELETE FROM user_watch_progress WHERE server_id = ?
 AND NOT EXISTS (SELECT 1 FROM id_mappings m WHERE m.virtual_id = virtual_item_id)`, op.Target); err != nil {
		return err
	}
	if err := db.execParams(`UPDATE user_watch_progress SET
 server_id = (SELECT m.server_id FROM id_mappings m WHERE m.virtual_id = virtual_item_id),
 original_item_id = (SELECT m.original_id FROM id_mappings m WHERE m.virtual_id = virtual_item_id),
 runtime_ticks = 0, series_original_id = ''
 WHERE server_id = ?`, op.Target); err != nil {
		return err
	}
	for virtualID := range affected {
		// Both a migrated film and a removed/promoted series can change a locator.
		if err := db.execParams(`UPDATE user_watch_progress SET series_original_id = COALESCE(
   (SELECT original_id FROM (
    SELECT virtual_id, server_id, original_id FROM id_mappings
    UNION ALL SELECT virtual_id, server_id, original_id FROM id_additional_instances
   ) i WHERE i.virtual_id = user_watch_progress.series_virtual_id
    AND i.server_id = user_watch_progress.server_id LIMIT 1), ''),
   series_virtual_id = CASE WHEN EXISTS(SELECT 1 FROM id_mappings m WHERE m.virtual_id = user_watch_progress.series_virtual_id)
    THEN series_virtual_id ELSE '' END
   WHERE virtual_item_id = ? OR series_virtual_id = ?`, virtualID, virtualID); err != nil {
			return err
		}
	}
	if err := db.execParams(`UPDATE users SET auth_revision = auth_revision + 1
 WHERE id IN (SELECT user_id FROM user_servers WHERE server_id = ?)`, op.Target); err != nil {
		return err
	}
	for _, query := range []string{
		`DELETE FROM user_servers WHERE server_id = ?`,
		`DELETE FROM id_additional_instances WHERE server_id = ?`,
		`DELETE FROM user_hidden_libraries WHERE server_id = ?`,
	} {
		if err := db.execParams(query, op.Target); err != nil {
			return err
		}
	}
	if err := a.IDStore.writeMergeRemovalSQL(a.IDStore.planMergeSourceRemovalLocked(op.Target, allowed)); err != nil {
		return err
	}
	// Durable last line of defense against stale derived rows.
	if err := db.execParams(`DELETE FROM work_identity_keys WHERE server_id = ?`, op.Target); err != nil {
		return err
	}
	if err := db.execParams(`DELETE FROM work_identity_items WHERE server_id = ?`, op.Target); err != nil {
		return err
	}
	if err := removeScannerSourceSQL(db, op.Target); err != nil {
		return err
	}
	return a.IDStore.advanceSourceGenerationSQL(op.Target)
}

// Config absence is the durable commit intent. A failed database cleanup never
// restores that config or reopens the removed source.
func (a *App) finishCleanupLocked(op watchCleanupOperation) error {
	db, err := a.lifecycleDB()
	if err != nil {
		return err
	}
	a.lifecyclePending = true
	if op.Kind != "user_update" || len(op.Users) != 0 {
		a.suspendCountsWorkLocked()
	}
	if op.Kind == "server_delete" && configuredSourceIDs(a.ConfigStore.Snapshot())[op.Target] {
		if op.Phase != "prepared" {
			return fmt.Errorf("committed deletion conflicts with configuration")
		}
		if err := db.writeParams(`DELETE FROM pending_watch_cleanup WHERE operation_id = ?`, op.ID); err != nil {
			return err
		}
		return nil
	}
	unlock := a.lockLifecycleStores()
	if op.Phase == "prepared" {
		if op.Kind != "server_delete" {
			unlock()
			return fmt.Errorf("invalid prepared user operation")
		}
		err = db.withWriteTx(func() error {
			if err := a.applyServerCleanupSQL(db, op); err != nil {
				return err
			}
			op.Phase = "committed"
			return writeCleanupOperation(db, op)
		})
	}
	if err == nil {
		err = a.reloadLifecycleStoresLocked()
	}
	unlock()
	if err != nil {
		return err
	}
	// Store/DB locks are released before Auth/routes/limiter/identity file work.
	if op.Kind == "server_delete" {
		a.watchPlayback.cleanLifecycle("", op.Target, true)
		a.dropMissingWatchOwners()
		a.markCleanupRevisions(op.Users)
		a.playbackRoutes.RemoveLifecycle("", op.Target)
		a.PlaybackLimiter.RemoveLifecycle("", op.Target)
		a.IDStore.removeActiveSource(op.Target)
		a.invalidateUpstreamLibraryCache(op.Target)
	} else if op.Kind == "user_delete" || op.ClearUser {
		a.watchPlayback.cleanLifecycle(op.Target, "", false)
		a.playbackRoutes.RemoveLifecycle(op.Target, "")
		a.PlaybackLimiter.RemoveLifecycle(op.Target, "")
	} else {
		for _, source := range op.Sources {
			a.watchPlayback.cleanLifecycle(op.Target, source, false)
			a.playbackRoutes.RemoveLifecycle(op.Target, source)
			a.PlaybackLimiter.RemoveLifecycle(op.Target, source)
		}
	}
	if a.Auth == nil {
		return fmt.Errorf("token cleanup service is unavailable")
	}
	if err := a.Auth.revokeUsersDurably(op.Users); err != nil {
		return err
	}
	if a.Identity != nil {
		if err := a.Identity.cleanupLifecycle(op); err != nil {
			return err
		}
	}
	if err := db.writeParams(`DELETE FROM pending_watch_cleanup WHERE operation_id = ?`, op.ID); err != nil {
		return err
	}
	return nil
}

func (a *App) recoverWatchLifecycleLocked() error {
	if err := a.initializeWatchLifecycle(); err != nil {
		return err
	}
	ops, err := a.pendingCleanupOperations()
	if err != nil {
		a.lifecyclePending = true
		return err
	}
	for _, op := range ops {
		if err := a.finishCleanupLocked(op); err != nil {
			a.lifecyclePending = true
			return err
		}
	}
	a.lifecyclePending = false
	a.resumeCountsSourcesLocked()
	// A committed binding update may have returned cleanupPending earlier.
	// Recovery publishes its one refresh demand only after cleanup succeeds.
	for _, op := range ops {
		if !op.CountsBindingsChanged {
			continue
		}
		if user := a.UserStore.Get(op.Target); user != nil && user.Enabled {
			a.requestBoundCountsLocked(user.AllowedServers)
		}
	}
	return nil
}

func (a *App) deleteUpstreamLifecycleLocked(next Config, source UpstreamConfig) error {
	if err := a.recoverWatchLifecycleLocked(); err != nil {
		return err
	}
	db, _ := a.lifecycleDB()
	users := []string{}
	for _, user := range a.UserStore.List() {
		if containsString(user.AllowedServers, source.ID) {
			users = append(users, user.ID)
		}
	}
	sort.Strings(users)
	op := watchCleanupOperation{ID: randomHex(16), Kind: "server_delete", Target: source.ID,
		Phase: "prepared", Users: users, LegacyIdentityDigest: legacyIdentityDigest(legacyUpstreamKey(source))}
	if err := db.withWriteTx(func() error { return writeCleanupOperation(db, op) }); err != nil {
		return err
	}
	previous := a.ConfigStore.Snapshot()
	a.ConfigStore.Replace(next)
	if err := a.ConfigStore.Save(); err != nil {
		a.ConfigStore.Replace(previous)
		if cancelErr := db.writeParams(`DELETE FROM pending_watch_cleanup WHERE operation_id = ?`, op.ID); cancelErr != nil {
			a.lifecyclePending = true
			return fmt.Errorf("configuration save failed; prepared cleanup cancellation is pending")
		}
		return err
	}
	a.lifecyclePending = true
	a.publishConfiguredSourcesLocked(next)
	if a.Upstream != nil {
		a.Upstream.Reload(next)
	}
	if err := a.finishCleanupLocked(op); err != nil {
		return fmt.Errorf("configuration removal committed; cleanup pending: %w", err)
	}
	a.lifecyclePending = false
	a.resumeCountsSourcesLocked()
	return nil
}

func (a *App) deleteUserLifecycleLocked(id string) error {
	if err := a.recoverWatchLifecycleLocked(); err != nil {
		return err
	}
	db, _ := a.lifecycleDB()
	unlock := a.lockLifecycleStores()
	user := a.UserStore.users[id]
	if user == nil {
		unlock()
		return fmt.Errorf("user not found")
	}
	op := watchCleanupOperation{ID: randomHex(16), Kind: "user_delete", Target: id, Phase: "committed", Users: []string{id}, ClearUser: true}
	err := db.withWriteTx(func() error {
		for _, query := range []string{
			`DELETE FROM user_watch_progress WHERE proxy_user_id = ?`,
			`DELETE FROM user_hidden_libraries WHERE user_id = ?`,
			`DELETE FROM user_servers WHERE user_id = ?`,
			`DELETE FROM users WHERE id = ?`,
		} {
			if err := db.execParams(query, id); err != nil {
				return err
			}
		}
		return writeCleanupOperation(db, op)
	})
	unlock()
	if err != nil {
		return err
	}
	if err := a.finishCleanupLocked(op); err != nil {
		return fmt.Errorf("user deletion committed; cleanup pending: %w", err)
	}
	a.lifecyclePending = false
	a.resumeCountsSourcesLocked()
	return nil
}

func (a *App) updateUserLifecycleLocked(id string, username, password *string, enabled *bool, allowed *[]string,
	limits ServerGrantLimits, hidden map[string]*[]string) error {
	if err := a.recoverWatchLifecycleLocked(); err != nil {
		return err
	}
	db, _ := a.lifecycleDB()
	unlock := a.lockLifecycleStores()
	current := a.UserStore.users[id]
	if current == nil {
		unlock()
		return fmt.Errorf("user not found")
	}
	nextAllowed := current.AllowedServers
	if allowed != nil {
		nextAllowed = *allowed
	}
	bindingsChanged := allowed != nil && !countsSameBindings(current.AllowedServers, nextAllowed)
	sourceSet := configuredSourceIDs(a.ConfigStore.Snapshot())
	for source, libraries := range hidden {
		if source != "" && libraries != nil && len(*libraries) > 0 && !sourceSet[source] {
			unlock()
			return fmt.Errorf("hidden library source is no longer configured")
		}
	}
	revoke := password != nil || (enabled != nil && *enabled != current.Enabled) || bindingsChanged
	removed := []string{}
	for _, source := range current.AllowedServers {
		if !containsString(nextAllowed, source) {
			removed = append(removed, source)
		}
	}
	op := watchCleanupOperation{ID: randomHex(16), Kind: "user_update", Target: id, Phase: "committed",
		Sources: removed, ClearUser: password != nil || (enabled != nil && !*enabled), CountsBindingsChanged: bindingsChanged}
	if revoke {
		op.Users = []string{id}
	}
	err := a.UserStore.updateWithServerLimitsLocked(id, username, password, enabled, allowed, limits, func() error {
		for source, libraries := range hidden {
			if source == "" || libraries == nil {
				continue
			}
			if err := db.execParams(`DELETE FROM user_hidden_libraries WHERE user_id = ? AND server_id = ?`, id, source); err != nil {
				return err
			}
			for _, library := range *libraries {
				if library == "" {
					continue
				}
				if err := db.execParams(`INSERT OR IGNORE INTO user_hidden_libraries (user_id, server_id, library_id) VALUES (?, ?, ?)`,
					id, source, library); err != nil {
					return err
				}
			}
		}
		if allowed != nil {
			// The newly committed grant set also removes preexisting stale hidden rows.
			// Watch history is retained for future rebinding.
			if err := db.execParams(`DELETE FROM user_hidden_libraries WHERE user_id = ?
    AND server_id NOT IN (SELECT server_id FROM user_servers WHERE user_id = ?)`, id, id); err != nil {
				return err
			}
		}
		return writeCleanupOperation(db, op)
	})
	unlock()
	if err != nil {
		return err
	}
	if err := a.finishCleanupLocked(op); err != nil {
		return fmt.Errorf("user update committed; cleanup pending: %w", err)
	}
	a.lifecyclePending = false
	a.resumeCountsSourcesLocked()
	if user := a.UserStore.Get(id); bindingsChanged && user != nil && user.Enabled {
		a.requestBoundCountsLocked(user.AllowedServers)
	}
	return nil
}

func (a *App) publishConfiguredSourcesLocked(cfg Config) {
	a.pruneCountsSourcesLocked(cfg)
	if a.IDStore != nil {
		a.IDStore.mu.Lock()
		a.IDStore.configuredSources = configuredSourceIDs(cfg)
		a.IDStore.mu.Unlock()
	}
	if a.Identity != nil {
		a.Identity.configureSources(cfg.Upstream)
	}
}

// Called under the lifecycle gate; an in-flight token must still be issued and
// belong to this durable user generation. Synthetic internal contexts use revision.
func (a *App) requestAuthorizationValidLocked(reqCtx *RequestContext) bool {
	return !a.lifecyclePending && a.requestIdentityValidLocked(reqCtx)
}

// Separate identity validity from resource readiness so Counts can return
// authenticated cleanupPending=503. All existing routes still deny pending.
func (a *App) requestIdentityValidLocked(reqCtx *RequestContext) bool {
	if reqCtx == nil || reqCtx.ProxyUser == nil {
		return false
	}
	info := reqCtx.ProxyUser
	if reqCtx.ProxyToken != "" && a.Auth != nil {
		current := a.Auth.ValidateToken(reqCtx.ProxyToken)
		if current == nil || current.UserID != info.UserID || current.Role != info.Role || current.AuthRevision != info.AuthRevision {
			return false
		}
	}
	if info.Role == "admin" {
		return true
	}
	if a.UserStore == nil {
		return false
	}
	user := a.UserStore.Get(info.UserID)
	return user != nil && user.Enabled && user.AuthRevision == info.AuthRevision
}

// Administrative authentication remains available to retry a pending cleanup.
func (a *App) currentRequestAuthorized(reqCtx *RequestContext) bool {
	a.watchLifecycleMu.RLock()
	defer a.watchLifecycleMu.RUnlock()
	if reqCtx != nil && reqCtx.ProxyUser != nil && reqCtx.ProxyUser.Role == "admin" {
		if a.Auth == nil || reqCtx.ProxyToken == "" {
			return true
		}
		return a.Auth.ValidateToken(reqCtx.ProxyToken) != nil
	}
	return a.requestAuthorizationValidLocked(reqCtx)
}

func (m *AuthManager) revokeUsersDurably(userIDs []string) error {
	if len(userIDs) == 0 {
		return nil
	}
	selected := make(map[string]bool, len(userIDs))
	for _, id := range userIDs {
		selected[id] = true
	}
	var removed []string
	m.mu.Lock()
	for token, info := range m.tokens {
		if selected[info.UserID] {
			removed = append(removed, token)
			delete(m.tokens, token)
		}
	}
	m.mu.Unlock()
	for _, token := range removed {
		if m.identity != nil {
			m.identity.DeleteCaptured(token)
		}
	}
	return m.save()
}

func (s *IDStore) removeActiveSource(source string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, active := range s.activeStreamServer {
		if active.ServerID == source {
			delete(s.activeStreamServer, id)
		}
	}
}

func legacyIdentityDigest(key string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(key)))
}

// String comparisons are exact stable IDs; no address/name identity reuse.
func legacyUpstreamKey(source UpstreamConfig) string {
	spoof := strings.TrimSpace(source.SpoofClient)
	if spoof == "" {
		spoof = "none"
	}
	return strings.TrimRight(strings.TrimSpace(source.URL), "/") + "|" + strings.TrimSpace(source.Name) + "|" + spoof
}
