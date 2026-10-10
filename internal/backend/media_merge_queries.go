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
	return a.mergeDiscovery().observeLegacySeriesParent(reqCtx, server, item)
}
