package backend

import (
	"errors"
	"sort"
)

var (
	errMediaAccessDenied      = errors.New("media access denied")
	errMediaMappingMissing    = errors.New("media mapping not found")
	errMediaSourceUnavailable = errors.New("authorized media source unavailable")
)

// mediaAccessScope is a request snapshot, not a long-lived authorization grant.
// serverIDs includes offline sources; onlineServerIDs is only for resource reads.
// Recreate the scope after network I/O before publishing state or forwarding again.
type mediaAccessScope struct {
	userID          string
	serverIDs       []string
	onlineServerIDs []string
	allowed         map[string]struct{}
}

// mediaAccessScopeLocked requires the lifecycle read or write gate; management
// publishes grant/source generations under the write side before readers resume.
func (a *App) mediaAccessScopeLocked(reqCtx *RequestContext) mediaAccessScope {
	scope := mediaAccessScope{allowed: make(map[string]struct{})}
	if !a.requestAuthorizationValidLocked(reqCtx) {
		return scope
	}
	if reqCtx == nil || reqCtx.ProxyUser == nil || reqCtx.ProxyUser.UserID == "" || a.ConfigStore == nil {
		return scope
	}
	scope.userID = reqCtx.ProxyUser.UserID
	admin := reqCtx.ProxyUser.Role == "admin"
	var grants map[string]struct{}
	if !admin {
		// Token grants can be stale after an in-flight binding change. Read the
		// current enabled user and intersect it with this token's permissions.
		if a.UserStore == nil {
			return scope
		}
		user := a.UserStore.Get(scope.userID)
		if user == nil || !user.Enabled {
			return scope
		}
		grants = make(map[string]struct{}, len(user.AllowedServers))
		for _, id := range user.AllowedServers {
			if containsString(reqCtx.ProxyUser.AllowedServers, id) {
				grants[id] = struct{}{}
			}
		}
	}
	cfg := a.ConfigStore.Snapshot()
	for _, server := range cfg.Upstream {
		if server.ID == "" {
			continue
		}
		if !admin {
			if _, ok := grants[server.ID]; !ok {
				continue
			}
		}
		if _, duplicate := scope.allowed[server.ID]; duplicate {
			continue
		}
		scope.allowed[server.ID] = struct{}{}
		scope.serverIDs = append(scope.serverIDs, server.ID)
		if a.Upstream != nil {
			client := a.Upstream.ClientByID(server.ID)
			if client != nil && client.IsOnline() {
				scope.onlineServerIDs = append(scope.onlineServerIDs, server.ID)
			}
		}
	}
	sort.Strings(scope.serverIDs)
	sort.Strings(scope.onlineServerIDs)
	return scope
}

func (scope mediaAccessScope) allows(serverID string) bool {
	_, ok := scope.allowed[serverID]
	return ok
}

// authorizedMediaInstances keeps the primary first but never retains an
// unauthorized source. Offline instances still establish history visibility.
func authorizedMediaInstances(scope mediaAccessScope, resolved *ResolvedID) []AdditionalInstance {
	if resolved == nil {
		return nil
	}
	candidates := make([]AdditionalInstance, 0, len(resolved.OtherInstances)+1)
	candidates = append(candidates, AdditionalInstance{OriginalID: resolved.OriginalID, ServerID: resolved.ServerID})
	candidates = append(candidates, resolved.OtherInstances...)
	seen := make(map[AdditionalInstance]struct{}, len(candidates))
	result := make([]AdditionalInstance, 0, len(candidates))
	for _, instance := range candidates {
		if instance.OriginalID == "" || !scope.allows(instance.ServerID) {
			continue
		}
		if _, duplicate := seen[instance]; duplicate {
			continue
		}
		seen[instance] = struct{}{}
		result = append(result, instance)
	}
	return result
}

// resolveAuthorizedRouteID resolves a stable virtual item, rather than an
// explicit source version. Phase 2 will replace client-facing resolver calls.
func (a *App) resolveAuthorizedRouteID(reqCtx *RequestContext, virtualID string) (*routeResolution, error) {
	a.watchLifecycleMu.RLock()
	defer a.watchLifecycleMu.RUnlock()
	scope := a.mediaAccessScopeLocked(reqCtx)
	if a.IDStore == nil {
		return nil, errMediaMappingMissing
	}
	return a.routeForAuthorizedMapping(scope, a.IDStore.ResolveVirtualID(virtualID))
}

func (a *App) routeForAuthorizedMapping(scope mediaAccessScope, resolved *ResolvedID) (*routeResolution, error) {
	if resolved == nil {
		return nil, errMediaMappingMissing
	}
	instances := authorizedMediaInstances(scope, resolved)
	if len(instances) == 0 {
		return nil, errMediaAccessDenied
	}
	for index, instance := range instances {
		if a.Upstream == nil {
			break
		}
		client := a.Upstream.ClientByID(instance.ServerID)
		if client == nil || !client.IsOnline() {
			continue
		}
		remaining := make([]AdditionalInstance, 0, len(instances)-1)
		remaining = append(remaining, instances[:index]...)
		remaining = append(remaining, instances[index+1:]...)
		return &routeResolution{OriginalID: instance.OriginalID, ServerID: instance.ServerID,
			Client: client, OtherInstances: remaining}, nil
	}
	return nil, errMediaSourceUnavailable
}

// Session control still needs a proven source while that source is offline.
// Resource reads use routeForAuthorizedMapping and continue to require online.
func (a *App) resolveAuthorizedSessionRouteID(reqCtx *RequestContext, virtualID string) (*routeResolution, error) {
	a.watchLifecycleMu.RLock()
	defer a.watchLifecycleMu.RUnlock()
	if a.IDStore == nil {
		return nil, errMediaMappingMissing
	}
	scope := a.mediaAccessScopeLocked(reqCtx)
	resolved := a.IDStore.ResolveVirtualID(virtualID)
	route, err := a.routeForAuthorizedMapping(scope, resolved)
	if !errors.Is(err, errMediaSourceUnavailable) {
		return route, err
	}
	instances := authorizedMediaInstances(scope, resolved)
	if len(instances) == 0 {
		return nil, errMediaAccessDenied
	}
	selected := instances[0]
	return &routeResolution{OriginalID: selected.OriginalID, ServerID: selected.ServerID}, nil
}

// Deny a known forbidden item/version before translation loses its identity.
func (a *App) sessionTargetAccessDenied(reqCtx *RequestContext, itemID, mediaID string) bool {
	a.watchLifecycleMu.RLock()
	defer a.watchLifecycleMu.RUnlock()
	if a.IDStore == nil {
		return false
	}
	scope := a.mediaAccessScopeLocked(reqCtx)
	if item := a.IDStore.ResolveVirtualID(itemID); item != nil &&
		len(authorizedMediaInstances(scope, item)) == 0 {
		return true
	}
	if media := a.IDStore.ResolveVirtualID(mediaID); media != nil && !scope.allows(media.ServerID) {
		return true
	}
	return false
}

// authorizedMediaVisibilityBatch evaluates known virtual IDs without reading
// watch-row locators or issuing upstream requests. Unknown mappings stay hidden.
func (a *App) authorizedMediaVisibilityBatch(reqCtx *RequestContext, virtualIDs []string) map[string]bool {
	a.watchLifecycleMu.RLock()
	defer a.watchLifecycleMu.RUnlock()
	result := make(map[string]bool, len(virtualIDs))
	scope := a.mediaAccessScopeLocked(reqCtx)
	if a.IDStore == nil || len(scope.serverIDs) == 0 {
		return result
	}
	for _, virtualID := range virtualIDs {
		if _, visited := result[virtualID]; visited {
			continue
		}
		result[virtualID] = len(authorizedMediaInstances(scope, a.IDStore.ResolveVirtualID(virtualID))) > 0
	}
	return result
}

// withVisibleWatchScope keeps authorization snapshot and SQL read under the
// lifecycle gate. The callback must only do bounded local reads, never network
// I/O, mutations, or call a helper that takes the gate again.
func (a *App) withVisibleWatchScope(reqCtx *RequestContext, onlineOnly bool, read func(mediaAccessScope) error) error {
	a.watchLifecycleMu.RLock()
	defer a.watchLifecycleMu.RUnlock()
	scope := a.mediaAccessScopeLocked(reqCtx)
	if onlineOnly {
		// This temporary restriction only omits unavailable Resume/NextUp
		// candidates. It does not change the user's persistent grants/history.
		scope.serverIDs = scope.onlineServerIDs
	}
	return read(scope)
}
