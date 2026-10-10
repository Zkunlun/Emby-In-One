package backend

import "errors"

var errMergeGroupQuarantined = errors.New("merge group requires source-qualified selection")

// This method never consults a global canonical primary instance: the exact
// item-qualified media-source handle alone proves one source identity.
func (a *App) resolveAuthorizedExplicitSourceRouteID(reqCtx *RequestContext, itemID, sourceID string) (*routeResolution, error) {
	a.watchLifecycleMu.RLock()
	defer a.watchLifecycleMu.RUnlock()
	if a.IDStore == nil || a.Upstream == nil || itemID == "" || sourceID == "" {
		return nil, errMediaMappingMissing
	}
	scope := a.mediaAccessScopeLocked(reqCtx)
	mapped := a.IDStore.ResolveVirtualID(sourceID)
	if mapped == nil || mapped.MediaItemID == "" || mapped.ServerID == "" {
		return nil, errMediaMappingMissing
	}
	if !scope.allows(mapped.ServerID) {
		return nil, errMediaAccessDenied
	}
	if !a.IDStore.MergeMemberAllowed(itemID, mapped.ServerID, mapped.MediaItemID, mapped.OriginalID) ||
		!a.IDStore.MergeGroupRouteAllowed(itemID, mapped.ServerID, mapped.MediaItemID, mapped.OriginalID) {
		return nil, errMediaMappingMissing
	}
	client := a.Upstream.ClientByID(mapped.ServerID)
	if client == nil || !client.IsOnline() {
		return nil, errMediaSourceUnavailable
	}
	return &routeResolution{OriginalID: mapped.MediaItemID, ServerID: mapped.ServerID, Client: client}, nil
}
