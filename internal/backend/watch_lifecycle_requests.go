package backend

import "fmt"

func (a *App) publishPlaybackState(reqCtx *RequestContext, serverID string, publish func()) bool {
	a.watchLifecycleMu.RLock()
	defer a.watchLifecycleMu.RUnlock()
	if !a.mediaAccessScopeLocked(reqCtx).allows(serverID) {
		return false
	}
	publish()
	return true
}

func (a *App) rememberSourceRoute(reqCtx *RequestContext, sourceID, itemID, serverID, sessionID, clientSessionID string) {
	a.publishPlaybackState(reqCtx, serverID, func() {
		owner := playbackRouteOwner(reqCtx)
		a.playbackRoutes.RegisterOwner(owner, reqCtx.ProxyUser.UserID)
		a.playbackRoutes.RememberMediaSource(owner, sourceID, serverID, sessionID, clientSessionID)
		a.playbackRoutes.RememberMediaSourceItem(owner, sourceID, itemID, serverID)
	})
}

func (a *App) rememberMediaMembership(reqCtx *RequestContext, sourceID, itemID, serverID string) {
	a.publishPlaybackState(reqCtx, serverID, func() {
		owner := playbackRouteOwner(reqCtx)
		a.playbackRoutes.RegisterOwner(owner, reqCtx.ProxyUser.UserID)
		a.playbackRoutes.RememberMediaSourceItem(owner, sourceID, itemID, serverID)
	})
}

func (a *App) publishPlaybackInfoLease(reqCtx *RequestContext, lease *playbackInfoLeaseReservation, itemID, serverID, sessionID string) bool {
	committed := false
	a.publishPlaybackState(reqCtx, serverID, func() {
		committed = a.commitPlaybackInfoLease(lease, itemID, serverID, sessionID)
	})
	return committed
}

func (a *App) activatePlaybackRoute(reqCtx *RequestContext, itemID, serverID, sessionID, clientSessionID string) {
	a.publishPlaybackState(reqCtx, serverID, func() {
		owner := playbackRouteOwner(reqCtx)
		a.playbackRoutes.RegisterOwner(owner, reqCtx.ProxyUser.UserID)
		a.IDStore.SetActiveStream(itemID, serverID)
		a.playbackRoutes.Activate(owner, itemID, serverID, sessionID, clientSessionID)
	})
}

func (a *App) authenticatedWatchMutation(reqCtx *RequestContext, itemID string, write func() error) error {
	a.watchLifecycleMu.RLock()
	defer a.watchLifecycleMu.RUnlock()
	scope := a.mediaAccessScopeLocked(reqCtx)
	if !a.requestAuthorizationValidLocked(reqCtx) ||
		len(authorizedMediaInstances(scope, a.IDStore.ResolveVirtualID(itemID))) == 0 {
		return fmt.Errorf("watch authorization changed")
	}
	return write()
}

func (a *App) mutateLocalFavorite(reqCtx *RequestContext, itemID string, favorite bool) error {
	itemID = a.IDStore.CanonicalMergeID(itemID)
	return a.authenticatedWatchMutation(reqCtx, itemID, func() error {
		itemID = a.IDStore.CanonicalMergeID(itemID)
		return a.watchPlayback.mutate(reqCtx.ProxyUser.UserID, itemID, false, func() error {
			if err := a.WatchStore.SeedMergeState(reqCtx.ProxyUser.UserID, itemID); err != nil {
				return err
			}
			return a.WatchStore.SetFavorite(reqCtx.ProxyUser.UserID, itemID, favorite)
		})
	})
}

func (a *App) authenticateCurrentUser(user *User, hasPassword bool, deviceID string) (map[string]any, string, error) {
	a.watchLifecycleMu.RLock()
	defer a.watchLifecycleMu.RUnlock()
	if a.lifecyclePending {
		return nil, "", fmt.Errorf("authorization cleanup is pending")
	}
	return a.Auth.AuthenticateUser(user, hasPassword, deviceID)
}
