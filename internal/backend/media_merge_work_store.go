package backend

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"time"
)

// RegisterMergeItem records the item's identity anchor and every qualified
// observed source. target may preserve a caller-held ID already owning this item.
func (s *IDStore) RegisterMergeItem(candidate mergeCandidate, target string) (string, error) {
	return s.registerMergeItem(candidate, target, false)
}

// RegisterMergeItemWithLegacyCapture atomically retains historical complete
// version scope alongside the new association. App callers must hold the
// lifecycle gate and validate their current request grant first.
func (s *IDStore) RegisterMergeItemWithLegacyCapture(candidate mergeCandidate, target string) (string, error) {
	return s.registerMergeItem(candidate, target, true)
}

func (s *IDStore) registerMergeItem(candidate mergeCandidate, target string, captureLegacy bool) (string, error) {
	members, err := mergeItemMembers(candidate)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.mergeStoreReadyLocked(); err != nil {
		return "", err
	}
	if !s.sourceAllowedForMergeLocked(candidate.ServerID) {
		return "", errors.New("merge source is not configured")
	}
	return s.mergeObservedItemsLocked([]mergeCandidate{candidate}, [][]mergeStoredMember{members}, target, nil, captureLegacy)
}

func mergeItemMembers(candidate mergeCandidate) ([]mergeStoredMember, error) {
	anchor, err := mergeMemberFromCandidate(candidate, "")
	if err != nil {
		return nil, err
	}
	result := []mergeStoredMember{anchor}
	for _, version := range candidate.Versions {
		if version.Ref.MediaSourceID == "" {
			continue
		}
		member, err := mergeMemberFromCandidate(candidate, version.Ref.MediaSourceID)
		if err == nil {
			result = append(result, member)
		}
	}
	return result, nil
}

// Only indexed, source-qualified owners of the encountered item are considered.
// No discovery scan is performed and unrelated saved groups are untouched.
func (s *IDStore) mergeItemOwnersLocked(candidate mergeCandidate) []string {
	var ids []string
	add := func(id string) {
		id = s.canonicalMergeIDLocked(id)
		if id != "" && !containsString(ids, id) {
			ids = append(ids, id)
		}
	}
	add(s.mergeState.Owners[mergeMemberKey{candidate.ServerID, candidate.ItemID, ""}])
	add(s.legacyMergeOwnerForItemLocked(candidate.ServerID, candidate.ItemID))
	for _, version := range candidate.Versions {
		if version.Ref.ServerID == candidate.ServerID && version.Ref.ItemID == candidate.ItemID {
			add(s.mergeState.Owners[mergeRefKey(version.Ref)])
		}
	}
	var remaining []string
	for id := range s.mergeState.Items[mergeItemKey{candidate.ServerID, candidate.ItemID}] {
		remaining = append(remaining, id)
	}
	sort.Strings(remaining)
	for _, id := range remaining {
		add(id)
	}
	return ids
}

func (s *IDStore) associateMergeItems(a, b mergeCandidate, sourceA, sourceB, target string) (string, error) {
	left, err := mergeMemberFromCandidate(a, sourceA)
	if err != nil {
		return "", err
	}
	right, err := mergeMemberFromCandidate(b, sourceB)
	if err != nil {
		return "", err
	}
	leftMembers, err := mergeItemMembers(a)
	if err != nil {
		return "", err
	}
	rightMembers, err := mergeItemMembers(b)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.mergeStoreReadyLocked(); err != nil {
		return "", err
	}
	if !s.sourceAllowedForMergeLocked(a.ServerID) || !s.sourceAllowedForMergeLocked(b.ServerID) {
		return "", errors.New("merge source is not configured")
	}
	if a.Identity.Type != b.Identity.Type {
		return "", errors.New("merge media type mismatch")
	}
	ownersA, ownersB := s.mergeItemOwnersLocked(a), s.mergeItemOwnersLocked(b)
	for _, owner := range append(append([]string(nil), ownersA...), ownersB...) {
		if current := s.storedMergeGroupLocked(owner); current != nil && current.TrustState == mergeTrustQuarantined {
			return "", errMergeGroupQuarantined
		}
	}
	existing := a.ServerID == b.ServerID && a.ItemID == b.ItemID
	for _, id := range ownersA {
		if containsString(ownersB, id) {
			existing = true
		}
	}
	var proof *mergeStoredProof
	if !existing {
		savedParent := s.sameMergeParentLocked(a.Parent, b.Parent)
		decision := compareMergeCandidatesWithSavedParent(a, b, savedParent)
		if !decision.Allowed {
			return "", fmt.Errorf("new merge association rejected: %s", decision.Reason)
		}
		parentID := ""
		if savedParent {
			parentID = s.legacyMergeOwnerForItemLocked(a.Parent.ServerID, a.Parent.SeriesID)
			if owner := s.mergeState.Owners[mergeMemberKey{a.Parent.ServerID, a.Parent.SeriesID, ""}]; owner != "" {
				parentID = owner
			}
		}
		proof = &mergeStoredProof{Left: left, Right: right, ParentGroupID: parentID, Policy: mergePolicyWork}
	}
	return s.mergeObservedItemsLocked([]mergeCandidate{a, b}, [][]mergeStoredMember{leftMembers, rightMembers}, target, proof, false)
}

// Build a detached union, commit all lookup tables together, then publish.
// Existing member metadata/proofs remain historical evidence; partial observations
// add locators without pruning any previously known version or watch row.
func (s *IDStore) mergeObservedItemsLocked(candidates []mergeCandidate, observations [][]mergeStoredMember, target string, proof *mergeStoredProof, captureLegacy bool) (string, error) {
	var owners []string
	for _, candidate := range candidates {
		for _, owner := range s.mergeItemOwnersLocked(candidate) {
			if !containsString(owners, owner) {
				owners = append(owners, owner)
			}
		}
	}
	target = s.canonicalMergeIDLocked(target)
	if target != "" && !containsString(owners, target) {
		return "", errors.New("target does not own the observed item")
	}
	if target == "" && len(owners) > 0 {
		target = owners[0]
	}
	kind := candidates[0].Identity.Type
	var group *mergeStoredGroup
	if target == "" {
		group = &mergeStoredGroup{VirtualID: s.nextMergeIDLocked(), MediaType: kind, Policy: mergePolicyWork}
	} else {
		group = s.legacyMergeGroupLocked(target, kind)
		if group == nil || group.MediaType != kind {
			return "", errors.New("target media type mismatch")
		}
	}
	before := cloneMergeGroup(group)
	group.Policy = mergePolicyWork
	var absorbed []string
	for _, owner := range owners {
		if owner == group.VirtualID {
			continue
		}
		other := s.legacyMergeGroupLocked(owner, kind)
		if other == nil || other.MediaType != kind {
			return "", errors.New("absorbed media type mismatch")
		}
		for _, member := range other.Members {
			if !mergeGroupHasMember(group, mergeRefKey(member.Ref)) {
				group.Members = append(group.Members, member)
			}
		}
		for _, item := range other.LegacyItems {
			group.LegacyItems = appendIfMissing(group.LegacyItems, item)
		}
		group.LegacyVersions = append(group.LegacyVersions, other.LegacyVersions...)
		group.Proofs = append(group.Proofs, other.Proofs...)
		group.Aliases = append(group.Aliases, other.VirtualID)
		group.Aliases = append(group.Aliases, other.Aliases...)
		absorbed = append(absorbed, owner)
	}
	// Passive registration snapshots an old WHOLE-item locator's previously
	// complete version list in the SAME SQLite transaction as group creation,
	// derived WorkIdentityIndex updates and alias absorption. A failure must
	// roll back both the capture and the new work association.
	if captureLegacy {
		for _, candidate := range candidates {
			if !candidate.SourcesComplete || (candidate.Identity.Type != "Movie" && candidate.Identity.Type != "Episode") {
				continue
			}
			legacyID := s.legacyMergeOwnerForItemLocked(candidate.ServerID, candidate.ItemID)
			if legacyID == "" {
				continue
			}
			legacy := s.legacyMergeGroupLocked(legacyID, candidate.Identity.Type)
			if legacy == nil || legacy.Policy != mergePolicyLegacy || legacy.MediaType != candidate.Identity.Type {
				continue
			}
			preserved := false
			for _, existing := range group.LegacyVersions {
				if existing.ServerID == candidate.ServerID && existing.ItemID == candidate.ItemID {
					preserved = true
					break
				}
			}
			if preserved {
				continue
			}
			capture := mergeLegacyVersions{ServerID: candidate.ServerID, ItemID: candidate.ItemID}
			for _, version := range candidate.Versions {
				if version.Ref.ServerID != candidate.ServerID || version.Ref.ItemID != candidate.ItemID ||
					version.Ref.MediaSourceID == "" || containsString(capture.SourceIDs, version.Ref.MediaSourceID) {
					return "", errors.New("legacy version list is ambiguous")
				}
				capture.SourceIDs = append(capture.SourceIDs, version.Ref.MediaSourceID)
			}
			if len(capture.SourceIDs) != 0 {
				group.LegacyVersions = append(group.LegacyVersions, capture)
			}
		}
	}
	for index, members := range observations {
		candidate := candidates[index]
		for _, member := range members {
			matched := false
			for i := range group.Members {
				if mergeRefKey(group.Members[i].Ref) == mergeRefKey(member.Ref) {
					mergeMemberObservation(&group.Members[i], member, candidate)
					matched = true
					break
				}
			}
			if !matched {
				evidence := evidenceForObservation(candidate, time.Now().UTC().Format(time.RFC3339Nano))
				member.Evidence = &evidence
				group.Members = append(group.Members, member)
			}
		}
	}
	if proof != nil {
		group.Proofs = append(group.Proofs, *proof)
	}
	aliases := make([]string, 0, len(group.Aliases))
	for _, alias := range group.Aliases {
		if alias != group.VirtualID && !containsString(aliases, alias) {
			aliases = append(aliases, alias)
		}
	}
	sort.Strings(aliases)
	if len(aliases) > 0 {
		group.Aliases = aliases
	} else {
		group.Aliases = nil
	}
	evaluateGroupEvidence(group)
	if !validStoredMergeGroup(group) {
		return "", errors.New("invalid observed merge group")
	}
	if len(absorbed) == 0 && reflect.DeepEqual(before, group) && s.mergeState.Groups[group.VirtualID] != nil {
		return group.VirtualID, nil
	}
	if err := s.db.withWriteTx(func() error { return s.writeMergeGroupSQL(group, absorbed) }); err != nil {
		return "", err
	}
	s.publishMergeGroupLocked(group, absorbed)
	return group.VirtualID, nil
}
