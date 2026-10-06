package backend

import (
	"encoding/json"
	"errors"
	"strings"
)

// CanonicalMergeID preserves caller-held aliases without recreating raw mappings.
func (s *IDStore) CanonicalMergeID(id string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.canonicalMergeIDLocked(id)
}

// Missing playback metadata does not prevent recording the item identity.
func (s *IDStore) RegisterMergePlaceholder(candidate mergeCandidate) (string, error) {
	if candidate.Identity.Type != "Movie" && candidate.Identity.Type != "Episode" {
		return "", errors.New("placeholder requires playable item")
	}
	return s.RegisterMergeItem(candidate, "")
}

func independentMergeCandidate(candidate mergeCandidate) mergeCandidate {
	candidate.Versions = append([]mergeVersionCandidate(nil), candidate.Versions...)
	for i := range candidate.Versions {
		if candidate.Versions[i].Blocked == mergeLiveVersion {
			candidate.Versions[i].Blocked = ""
		}
	}
	return candidate
}

// Bind all qualified observed versions while preserving the caller-held item ID.
// Partial responses only add members and cannot prune earlier observations.
func (s *IDStore) BindMergePlaceholder(id string, candidate mergeCandidate) error {
	if id == "" {
		return errors.New("binding requires an existing item ID")
	}
	_, err := s.RegisterMergeItem(independentMergeCandidate(candidate), id)
	return err
}

func (s *IDStore) MergeMemberAllowed(id, server, item, source string) bool {
	owner := s.ResolveMergeMember(server, item, source)
	return owner != "" && s.CanonicalMergeID(owner) == s.CanonicalMergeID(id)
}

const mergeSourceRoutePrefix = "\x1eEIO:media-source:v1:"

func mergeSourceRouteKey(item, source string) string {
	data, _ := json.Marshal([2]string{item, source})
	return mergeSourceRoutePrefix + string(data)
}

func decodeMergeSourceRoute(original string) (item, source string, ok bool) {
	if !strings.HasPrefix(original, mergeSourceRoutePrefix) {
		return "", "", false
	}
	var parts []string
	if json.Unmarshal([]byte(strings.TrimPrefix(original, mergeSourceRoutePrefix)), &parts) != nil ||
		len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// Persist source IDs in a disjoint, item-qualified namespace. These are routing
// handles, not merge members, and are removed with the existing source lifecycle.
func (s *IDStore) GetMergeMediaSourceID(server, item, source string) (string, error) {
	if server == "" || item == "" || source == "" {
		return "", errMediaMappingMissing
	}
	encoded := mergeSourceRouteKey(item, source)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.mergeStoreReadyLocked(); err != nil {
		return "", err
	}
	if !s.sourceAllowedForMergeLocked(server) {
		return "", errMediaAccessDenied
	}
	if id := s.originalToVirtual[compositeKey(encoded, server)]; id != "" {
		if entry := s.virtualToOriginal[id]; entry != nil && entry.ServerID == server && entry.OriginalID == encoded {
			return id, nil
		}
		return "", errMediaMappingMissing
	}
	id := s.nextMergeIDLocked()
	if err := s.db.withWriteTx(func() error {
		return s.db.execParams(
			"INSERT INTO id_mappings (virtual_id,original_id,server_id) VALUES (?,?,?)", id, encoded, server)
	}); err != nil {
		return "", err
	}
	s.virtualToOriginal[id] = &idEntry{OriginalID: encoded, ServerID: server}
	s.originalToVirtual[compositeKey(encoded, server)] = id
	s.originalIDToVirtual[encoded] = append(s.originalIDToVirtual[encoded], id)
	return id, nil
}

func (s *IDStore) ResolveMergeMediaSourceForItem(source, server, item string) (string, *ResolvedID, bool) {
	if mapped := s.ResolveVirtualID(source); mapped != nil {
		if mapped.MediaItemID != "" && mapped.MediaItemID != item {
			return "", nil, false
		}
		if mapped.ServerID != server {
			return "", nil, false
		}
		return source, mapped, true
	}
	id, mapped, ok := s.ResolveOriginalIDForServer(mergeSourceRouteKey(item, source), server)
	if ok {
		mapped.OriginalID = source
		mapped.MediaItemID = item
		return id, mapped, true
	}
	// Historical issued source IDs retain the old namespace, but still need
	// authoritative item membership at the HTTP boundary.
	return s.ResolveOriginalIDForServer(source, server)
}
