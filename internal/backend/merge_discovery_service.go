package backend

import (
	"errors"
	"reflect"
)

// mergeDiscoveryService is the only App-layer gateway for work-association
// mutations. It has no upstream network or HTTP projection responsibilities.
// A scanner writer must acquire an explicit scan grant when Phase 3E provides
// source-generation fencing: absence of a request is never scan authority.
type mergeDiscoveryService struct {
	app *App
}

var errMergeDiscoveryGrantRequired = errors.New("merge discovery requires an authenticated request grant")

func (a *App) mergeDiscovery() mergeDiscoveryService {
	return mergeDiscoveryService{app: a}
}

// Caller holds watchLifecycleMu exclusively. Access is always checked after
// upstream I/O, immediately before observing its result.
func (d mergeDiscoveryService) authorizedLocked(reqCtx *RequestContext, sources ...string) error {
	if reqCtx == nil {
		return errMergeDiscoveryGrantRequired
	}
	scope := d.app.mediaAccessScopeLocked(reqCtx)
	for _, source := range sources {
		if source == "" || !scope.allows(source) || d.app.IDStore == nil || !d.app.IDStore.sourceGenerationMatches(reqCtx, source) {
			return errMediaAccessDenied
		}
	}
	return nil
}

func (d mergeDiscoveryService) before(candidates ...mergeCandidate) map[string]*mergeStoredGroup {
	a := d.app
	before := make(map[string]*mergeStoredGroup)
	for _, candidate := range candidates {
		for _, id := range a.IDStore.MergeGroupsForItem(candidate.ServerID, candidate.ItemID) {
			before[id] = a.IDStore.ResolveMergeGroup(id)
		}
	}
	return before
}

// publish invalidates client-held canonical/alias watch data and transient
// media-source membership hints without destroying negotiated leases.
func (d mergeDiscoveryService) publish(id string, before map[string]*mergeStoredGroup) {
	a := d.app
	after := a.IDStore.ResolveMergeGroup(id)
	if len(before) == 1 && reflect.DeepEqual(before[id], after) {
		return
	}
	changed := map[string]bool{}
	for oldID, oldGroup := range before {
		changed[oldID] = true
		if oldGroup != nil {
			changed[oldGroup.VirtualID] = true
			for _, alias := range oldGroup.Aliases {
				changed[alias] = true
			}
		}
	}
	for _, candidateID := range a.IDStore.MergeStateIDs(id) {
		changed[candidateID] = true
	}
	if after != nil {
		changed[after.VirtualID] = true
		for _, alias := range after.Aliases {
			changed[alias] = true
		}
	}
	ids := make([]string, 0, len(changed))
	for candidateID := range changed {
		ids = append(ids, candidateID)
	}
	a.invalidateMergedWatchAliases(ids)
	if a.playbackRoutes != nil {
		a.playbackRoutes.mu.Lock()
		for key, entry := range a.playbackRoutes.mediaSource {
			if changed[entry.ItemID] {
				entry.ItemID = ""
				a.playbackRoutes.mediaSource[key] = entry
			}
		}
		a.playbackRoutes.mu.Unlock()
	}
}

// associate is the sole App-level work-to-work association operation. Strict
// comparator and authoritative membership remain owned by MergeStore.
func (d mergeDiscoveryService) associate(reqCtx *RequestContext, left, right mergeCandidate, sourceLeft, sourceRight, target string) (string, error) {
	a := d.app
	a.watchLifecycleMu.Lock()
	defer a.watchLifecycleMu.Unlock()
	if err := d.authorizedLocked(reqCtx, left.ServerID, right.ServerID); err != nil {
		return "", err
	}
	before := d.before(left, right)
	id, err := a.IDStore.AssociateMergePair(left, right, sourceLeft, sourceRight, target)
	if err == nil {
		d.publish(id, before)
	}
	return id, err
}

// register retains the previous passive legacy-version capture semantics.
// Registration + derived WorkIdentityIndex changes share MergeStore's
// transaction; evidence revalidation is a separate Phase 3D operation.
func (d mergeDiscoveryService) register(reqCtx *RequestContext, candidate mergeCandidate, target string, captureLegacy bool) (string, error) {
	a := d.app
	a.watchLifecycleMu.Lock()
	defer a.watchLifecycleMu.Unlock()
	if err := d.authorizedLocked(reqCtx, candidate.ServerID); err != nil {
		return "", err
	}
	before := d.before(candidate)
	var id string
	var err error
	if captureLegacy {
		id, err = a.IDStore.RegisterMergeItemWithLegacyCapture(independentMergeCandidate(candidate), target)
	} else {
		id, err = a.IDStore.RegisterMergeItem(independentMergeCandidate(candidate), target)
	}
	if err == nil {
		d.publish(id, before)
	}
	return id, err
}

// observeLate handles the independent mappings from an already dispatched
// request. The original user context must remain valid NOW, not merely at
// dispatch. Nil contexts cannot use this endpoint as an implicit scanner grant.
func (d mergeDiscoveryService) observeLate(result upstreamItemsResult) {
	if result.RequestScope == nil || result.ServerID == "" || result.Err != nil {
		return
	}
	for _, raw := range result.Items {
		candidate := mergeResultCandidate(result, raw)
		switch candidate.Identity.Type {
		case "Movie", "Episode", "Series", "Season":
			if _, err := d.register(result.RequestScope, candidate, "", false); err != nil {
				d.app.logMergeHTTPError(err)
			}
		default:
			if candidate.ItemID != "" {
				d.registerRawLocator(result.RequestScope, candidate.ServerID, candidate.ItemID)
			}
		}
	}
}

func (d mergeDiscoveryService) registerRawLocator(reqCtx *RequestContext, server, original string) {
	a := d.app
	a.watchLifecycleMu.Lock()
	defer a.watchLifecycleMu.Unlock()
	if err := d.authorizedLocked(reqCtx, server); err != nil || original == "" {
		return
	}
	a.IDStore.GetOrCreateVirtualID(original, server)
}

// observeLegacySeriesParent upgrades only a proven historical raw parent
// locator after an authorized Shows response. It never creates a cross-server
// work match and uses the exact same publication gate as other observations.
func (d mergeDiscoveryService) observeLegacySeriesParent(reqCtx *RequestContext, server, item string) error {
	a := d.app
	a.watchLifecycleMu.Lock()
	defer a.watchLifecycleMu.Unlock()
	if err := d.authorizedLocked(reqCtx, server); err != nil {
		return err
	}
	store := a.IDStore
	store.mu.Lock()
	if err := store.mergeStoreReadyLocked(); err != nil {
		store.mu.Unlock()
		return err
	}
	if !store.sourceAllowedForMergeLocked(server) {
		store.mu.Unlock()
		return errMediaAccessDenied
	}
	id := store.legacyMergeOwnerForItemLocked(server, item)
	if id == "" {
		store.mu.Unlock()
		return nil
	}
	group := store.legacyMergeGroupLocked(id, "Series")
	if group == nil || group.Policy != mergePolicyLegacy || (group.MediaType != "" && group.MediaType != "Series") {
		store.mu.Unlock()
		return nil
	}
	for _, member := range group.Members {
		if member.Ref.ServerID == server && member.Ref.ItemID == item {
			store.mu.Unlock()
			return nil
		}
	}
	candidate := newMergeCandidate(server, map[string]any{"Id": item, "Type": "Series"}, nil, false)
	member, err := mergeMemberFromCandidate(candidate, "")
	if err != nil {
		store.mu.Unlock()
		return err
	}
	// Snapshot from the current canonical identity before publishing.
	before := cloneMergeGroup(group)
	group.MediaType = "Series"
	group.Members = append(group.Members, member)
	err = store.db.withWriteTx(func() error { return store.writeMergeGroupSQL(group, nil) })
	if err == nil {
		store.publishMergeGroupLocked(group, nil)
	}
	store.mu.Unlock()
	if err == nil {
		d.publish(id, map[string]*mergeStoredGroup{id: before})
	}
	return err
}
