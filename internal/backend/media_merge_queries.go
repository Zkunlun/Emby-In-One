package backend

import (
	"net/url"
	"strings"
)

// An item selector names identities, even when several identities route to the
// same upstream item. Raw upstream IDs retain their historical item scope.
func (a *App) filterRequestedMergeItems(query url.Values, items []map[string]any) []map[string]any {
	wanted, raw := map[string]bool{}, map[string]bool{}
	hasSelector := false
	for key, values := range query {
		if !strings.EqualFold(key, "Ids") && !strings.EqualFold(key, "ItemIds") {
			continue
		}
		hasSelector = true
		for _, value := range values {
			for _, id := range strings.Split(value, ",") {
				id = strings.TrimSpace(id)
				if id == "" {
					continue
				}
				if a.IDStore.ResolveVirtualID(id) != nil {
					wanted[a.IDStore.CanonicalMergeID(id)] = true
				} else {
					raw[id] = true
				}
			}
		}
	}
	if !hasSelector {
		return items
	}
	kept := make([]map[string]any, 0, len(items))
	for _, item := range items {
		id, _ := item["Id"].(string)
		match := wanted[a.IDStore.CanonicalMergeID(id)]
		if resolved := a.IDStore.ResolveVirtualID(id); !match && resolved != nil {
			match = raw[resolved.OriginalID]
			for _, other := range resolved.OtherInstances {
				match = match || raw[other.OriginalID]
			}
		}
		if match {
			kept = append(kept, item)
		}
	}
	return kept
}

// A successful dedicated Shows request proves the type of its existing raw
// parent locator. It never creates a new cross-source parent relationship.
func (a *App) observeLegacyShowsParent(reqCtx *RequestContext, server, item string) error {
	a.watchLifecycleMu.Lock()
	defer a.watchLifecycleMu.Unlock()
	if reqCtx != nil && !a.mediaAccessScopeLocked(reqCtx).allows(server) {
		return errMediaAccessDenied
	}
	s := a.IDStore
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.mergeStoreReadyLocked(); err != nil {
		return err
	}
	if !s.sourceAllowedForMergeLocked(server) {
		return errMediaAccessDenied
	}
	id := s.legacyMergeOwnerForItemLocked(server, item)
	if id == "" {
		return nil
	}
	group := s.legacyMergeGroupLocked(id, "Series")
	if group == nil || group.Policy != mergePolicyLegacy || (group.MediaType != "" && group.MediaType != "Series") {
		return nil
	}
	for _, member := range group.Members {
		if member.Ref.ServerID == server && member.Ref.ItemID == item {
			return nil
		}
	}
	member, err := mergeMemberFromCandidate(newMergeCandidate(server, map[string]any{"Id": item, "Type": "Series"}, nil, false), "")
	if err != nil {
		return err
	}
	group.MediaType = "Series"
	group.Members = append(group.Members, member)
	if err := s.db.withWriteTx(func() error { return s.writeMergeGroupSQL(group, nil) }); err != nil {
		return err
	}
	s.publishMergeGroupLocked(group, nil)
	return nil
}
