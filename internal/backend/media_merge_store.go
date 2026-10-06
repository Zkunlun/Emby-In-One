package backend

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

const mergePolicyExact = "exact-version-v1"
const mergePolicyLegacy = "legacy-preserved"
const mergePolicyUnresolved = "unresolved-item-v1"
const mergePolicyWork = "work-identity-v2"

var errMergeStoreUnavailable = errors.New("merge associations require an open persistent store")
var errMergeAssociationConflict = errors.New("merge member belongs to a preserved legacy identity")

type mergeItemKey struct{ ServerID, ItemID string }

type mergeMemberKey struct {
	ServerID string
	ItemID   string
	SourceID string
}

func mergeRefKey(ref mergeVersionRef) mergeMemberKey {
	return mergeMemberKey{ref.ServerID, ref.ItemID, ref.MediaSourceID}
}

type mergeStoredMember struct {
	Ref           mergeVersionRef
	Runtime       playbackTickValue
	RuntimeOrigin string
	Identity      mergeIdentity
	Parent        *mergeSeriesEvidence
	Season        playbackTickValue
	Episode       playbackTickValue
}

type mergeStoredProof struct {
	Left          mergeStoredMember
	Right         mergeStoredMember
	ParentGroupID string
	Policy        string
}

type mergeLegacyVersions struct {
	ServerID  string
	ItemID    string
	SourceIDs []string
}

type mergeStoredGroup struct {
	VirtualID      string
	MediaType      string
	Policy         string
	Members        []mergeStoredMember
	LegacyItems    []AdditionalInstance
	Aliases        []string
	Proofs         []mergeStoredProof
	LegacyVersions []mergeLegacyVersions
}

type mergeStoreState struct {
	Groups  map[string]*mergeStoredGroup
	Owners  map[mergeMemberKey]string
	Aliases map[string]string
	Items   map[mergeItemKey]map[string]bool
}

func emptyMergeStoreState() mergeStoreState {
	return mergeStoreState{Groups: map[string]*mergeStoredGroup{}, Owners: map[mergeMemberKey]string{}, Aliases: map[string]string{}, Items: map[mergeItemKey]map[string]bool{}}
}

func cloneMergeGroup(group *mergeStoredGroup) *mergeStoredGroup {
	data, _ := json.Marshal(group)
	var copy mergeStoredGroup
	_ = json.Unmarshal(data, &copy)
	return &copy
}

func (s *IDStore) initMergeSchema() error {
	return s.db.withWriteTx(func() error {
		return s.db.exec(`
CREATE TABLE IF NOT EXISTS media_merge_groups (
 virtual_id TEXT PRIMARY KEY, snapshot TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS media_merge_members (
 server_id TEXT NOT NULL, item_id TEXT NOT NULL, source_id TEXT NOT NULL,
 virtual_id TEXT NOT NULL,
 PRIMARY KEY(server_id, item_id, source_id)
);
CREATE INDEX IF NOT EXISTS idx_media_merge_group ON media_merge_members(virtual_id);
CREATE TABLE IF NOT EXISTS media_merge_aliases (
 alias_id TEXT PRIMARY KEY, virtual_id TEXT NOT NULL
);
`)
	})
}

func (s *IDStore) loadMergeGroupsLocked() error {
	next := emptyMergeStoreState()
	stmt, err := s.db.prepare(`SELECT virtual_id, snapshot FROM media_merge_groups ORDER BY virtual_id`)
	if err != nil {
		return err
	}
	defer stmt.finalize()
	for {
		row, err := stmt.step()
		if err != nil {
			return err
		}
		if !row {
			break
		}
		var group mergeStoredGroup
		if err := json.Unmarshal([]byte(stmt.columnText(1)), &group); err != nil {
			return fmt.Errorf("merge snapshot: %w", err)
		}
		if group.VirtualID != stmt.columnText(0) || !validStoredMergeGroup(&group) {
			return errors.New("invalid merge group snapshot")
		}
		if s.virtualToOriginal[group.VirtualID] == nil {
			return errors.New("merge group has no virtual locator")
		}
		next.Groups[group.VirtualID] = &group
		for _, member := range group.Members {
			key := mergeRefKey(member.Ref)
			if _, duplicate := next.Owners[key]; duplicate {
				return errors.New("duplicate merge member ownership")
			}
			next.Owners[key] = group.VirtualID
			itemKey := mergeItemKey{key.ServerID, key.ItemID}
			if next.Items[itemKey] == nil {
				next.Items[itemKey] = map[string]bool{}
			}
			next.Items[itemKey][group.VirtualID] = true
		}
		for _, alias := range group.Aliases {
			if alias == group.VirtualID || alias == "" || s.virtualToOriginal[alias] == nil {
				return errors.New("invalid merge alias")
			}
			if _, exists := next.Aliases[alias]; exists {
				return errors.New("duplicate merge alias")
			}
			next.Aliases[alias] = group.VirtualID
		}
	}
	for alias := range next.Aliases {
		if next.Groups[alias] != nil {
			return errors.New("merge alias is a canonical group")
		}
	}
	// Cross-check the UNIQUE lookup tables against snapshots. Corruption is not
	// silently accepted as an unqualified whole-item association.
	memberStmt, err := s.db.prepare(`SELECT server_id, item_id, source_id, virtual_id FROM media_merge_members`)
	if err != nil {
		return err
	}
	defer memberStmt.finalize()
	memberCount := 0
	for {
		row, err := memberStmt.step()
		if err != nil {
			return err
		}
		if !row {
			break
		}
		key := mergeMemberKey{memberStmt.columnText(0), memberStmt.columnText(1), memberStmt.columnText(2)}
		if next.Owners[key] != memberStmt.columnText(3) {
			return errors.New("merge member index disagrees with snapshot")
		}
		memberCount++
	}
	if memberCount != len(next.Owners) {
		return errors.New("merge member index incomplete")
	}
	aliasStmt, err := s.db.prepare(`SELECT alias_id, virtual_id FROM media_merge_aliases`)
	if err != nil {
		return err
	}
	defer aliasStmt.finalize()
	aliasCount := 0
	for {
		row, err := aliasStmt.step()
		if err != nil {
			return err
		}
		if !row {
			break
		}
		if next.Aliases[aliasStmt.columnText(0)] != aliasStmt.columnText(1) {
			return errors.New("merge alias index disagrees with snapshot")
		}
		aliasCount++
	}
	if aliasCount != len(next.Aliases) {
		return errors.New("merge alias index incomplete")
	}
	s.mergeState = next
	for _, group := range next.Groups {
		s.projectMergeGroupLocked(group)
	}
	return nil
}

func validStoredMergeGroup(group *mergeStoredGroup) bool {
	if strings.TrimSpace(group.VirtualID) == "" ||
		(group.Policy != mergePolicyExact && group.Policy != mergePolicyLegacy && group.Policy != mergePolicyUnresolved && group.Policy != mergePolicyWork) ||
		(group.MediaType != "Movie" && group.MediaType != "Episode" && group.MediaType != "Series" && group.MediaType != "Season") ||
		(len(group.Members) == 0 && len(group.LegacyItems) == 0) {
		return false
	}
	if group.Policy == mergePolicyExact && len(group.LegacyItems) != 0 {
		return false
	}
	for _, item := range group.LegacyItems {
		if strings.TrimSpace(item.ServerID) == "" || strings.TrimSpace(item.OriginalID) == "" {
			return false
		}
	}
	for _, member := range group.Members {
		if strings.TrimSpace(member.Ref.ServerID) == "" || strings.TrimSpace(member.Ref.ItemID) == "" ||
			member.Identity.Type != group.MediaType {
			return false
		}
		if (group.MediaType == "Movie" || group.MediaType == "Episode") && strings.TrimSpace(member.Ref.MediaSourceID) == "" {
			if member.Ref.SourceIndex != -1 || (group.Policy != mergePolicyWork && (group.Policy != mergePolicyUnresolved || len(group.Members) != 1 || len(group.LegacyItems) != 0 || len(group.Proofs) != 0)) {
				return false
			}
		}
		if group.Policy == mergePolicyUnresolved && (member.Ref.MediaSourceID != "" || (group.MediaType != "Movie" && group.MediaType != "Episode")) {
			return false
		}
		if (group.MediaType == "Series" || group.MediaType == "Season") && member.Ref.MediaSourceID != "" {
			return false
		}
	}
	return true
}

func (s *IDStore) canonicalMergeIDLocked(id string) string {
	if canonical := s.mergeState.Aliases[id]; canonical != "" {
		return canonical
	}
	return id
}

func (s *IDStore) storedMergeGroupLocked(id string) *mergeStoredGroup {
	return s.mergeState.Groups[s.canonicalMergeIDLocked(id)]
}

func mergeGroupInstances(group *mergeStoredGroup) []AdditionalInstance {
	result := append([]AdditionalInstance(nil), group.LegacyItems...)
	for _, member := range group.Members {
		result = appendIfMissing(result, AdditionalInstance{OriginalID: member.Ref.ItemID, ServerID: member.Ref.ServerID})
	}
	return result
}

func (s *IDStore) projectMergeGroupLocked(group *mergeStoredGroup) {
	instances := mergeGroupInstances(group)
	for _, id := range append([]string{group.VirtualID}, group.Aliases...) {
		if len(instances) == 0 {
			delete(s.virtualToOriginal, id)
			continue
		}
		s.virtualToOriginal[id] = &idEntry{OriginalID: instances[0].OriginalID, ServerID: instances[0].ServerID,
			OtherInstances: append([]AdditionalInstance(nil), instances[1:]...)}
	}
}

// ResolveMergeGroup returns a detached precise snapshot. Old maps without a
// sidecar remain legacy, and are never implicitly converted to strict policy.
func (s *IDStore) ResolveMergeGroup(id string) *mergeStoredGroup {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if group := s.storedMergeGroupLocked(id); group != nil {
		return cloneMergeGroup(group)
	}
	if entry := s.virtualToOriginal[id]; entry != nil {
		return &mergeStoredGroup{VirtualID: id, Policy: mergePolicyLegacy,
			LegacyItems: append([]AdditionalInstance{{OriginalID: entry.OriginalID, ServerID: entry.ServerID}}, entry.OtherInstances...)}
	}
	return nil
}

func (s *IDStore) ResolveMergeMember(serverID, itemID, sourceID string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if id := s.mergeState.Owners[mergeMemberKey{serverID, itemID, sourceID}]; id != "" {
		return id
	}
	// Only historical WHOLE items fall back here. Precise groups do not appear
	// in originalToVirtual, including their projected raw routing locators.
	id := s.legacyMergeOwnerForItemLocked(serverID, itemID)
	if !legacyMergeSourceAllowed(s.storedMergeGroupLocked(id), mergeMemberKey{serverID, itemID, sourceID}) {
		return ""
	}
	return id
}

// Return every precise identity for a source-qualified raw item, rather than
// silently selecting one when it has multiple incompatible versions.
func (s *IDStore) MergeGroupsForItem(serverID, itemID string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := make(map[string]bool)
	for id := range s.mergeState.Items[mergeItemKey{serverID, itemID}] {
		ids[id] = true
	}
	if id := s.legacyMergeOwnerForItemLocked(serverID, itemID); id != "" {
		ids[id] = true
	}
	result := make([]string, 0, len(ids))
	for id := range ids {
		result = append(result, id)
	}
	sort.Strings(result)
	return result
}

// MergeStateIDs provides canonical plus client-held aliases for Phase3's state
// read/write integration. This store does not migrate or erase watch rows.
func (s *IDStore) MergeStateIDs(id string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if group := s.storedMergeGroupLocked(id); group != nil {
		return append([]string{group.VirtualID}, group.Aliases...)
	}
	if s.virtualToOriginal[id] != nil {
		return []string{id}
	}
	return nil
}

func (s *IDStore) mergeStoreReadyLocked() error {
	if s.closed || s.db == nil || !s.persistent {
		return errMergeStoreUnavailable
	}
	return nil
}

func (s *IDStore) sourceAllowedForMergeLocked(server string) bool {
	return strings.TrimSpace(server) != "" && (s.configuredSources == nil || s.configuredSources[server])
}

func mergeMemberFromCandidate(candidate mergeCandidate, sourceID string) (mergeStoredMember, error) {
	base := mergeStoredMember{Identity: candidate.Identity, Parent: candidate.Parent, Season: candidate.Season, Episode: candidate.Episode,
		Ref: mergeVersionRef{ServerID: candidate.ServerID, ItemID: candidate.ItemID, SourceIndex: -1}}
	switch candidate.Identity.Type {
	case "Series", "Season":
		if sourceID != "" {
			return base, errors.New("nonplayable identity cannot bind a media source")
		}
	case "Movie", "Episode":
		if sourceID == "" {
			break // Identity anchor; never a concrete playable route.
		}
		found := false
		for _, version := range candidate.Versions {
			if version.Ref.MediaSourceID != sourceID {
				continue
			}
			if found || version.Blocked != "" || version.Ref.SourceIndex < 0 || version.Ref.SourceIndex >= len(candidate.Versions) || version.Ref.ServerID != candidate.ServerID || version.Ref.ItemID != candidate.ItemID {
				return base, errors.New("ambiguous or unbound playback version")
			}
			found = true
			base.Ref, base.Runtime, base.RuntimeOrigin = version.Ref, version.Runtime, version.RuntimeOrigin
		}
		if !found {
			return base, errors.New("playback version is absent")
		}
	default:
		return base, errors.New("unsupported merge media type")
	}
	if strings.TrimSpace(base.Ref.ServerID) == "" || strings.TrimSpace(base.Ref.ItemID) == "" {
		return base, errors.New("unqualified member")
	}
	return base, nil
}

func (s *IDStore) legacyMergeGroupLocked(id, kind string) *mergeStoredGroup {
	if group := s.storedMergeGroupLocked(id); group != nil {
		return cloneMergeGroup(group)
	}
	entry := s.virtualToOriginal[id]
	if entry == nil {
		return nil
	}
	return &mergeStoredGroup{VirtualID: id, MediaType: kind, Policy: mergePolicyLegacy,
		LegacyItems: append([]AdditionalInstance{{OriginalID: entry.OriginalID, ServerID: entry.ServerID}}, entry.OtherInstances...)}
}

// Historical composite indices are only hints: verify the actual source/item
// locator before inheriting their whole-item compatibility scope.
func (s *IDStore) legacyMergeOwnerForItemLocked(serverID, itemID string) string {
	id := s.canonicalMergeIDLocked(s.originalToVirtual[compositeKey(itemID, serverID)])
	if group := s.storedMergeGroupLocked(id); group != nil {
		for _, item := range group.LegacyItems {
			if item.ServerID == serverID && item.OriginalID == itemID {
				return id
			}
		}
		return ""
	}
	if entry := s.virtualToOriginal[id]; entry != nil {
		if entry.ServerID == serverID && entry.OriginalID == itemID {
			return id
		}
		for _, item := range entry.OtherInstances {
			if item.ServerID == serverID && item.OriginalID == itemID {
				return id
			}
		}
	}
	return ""
}

func (s *IDStore) mergeOwnerLocked(member mergeStoredMember) string {
	if owner := s.mergeState.Owners[mergeRefKey(member.Ref)]; owner != "" {
		return owner
	}
	id := s.legacyMergeOwnerForItemLocked(member.Ref.ServerID, member.Ref.ItemID)
	if !legacyMergeSourceAllowed(s.storedMergeGroupLocked(id), mergeRefKey(member.Ref)) {
		return ""
	}
	return id
}

func legacyMergeSourceAllowed(group *mergeStoredGroup, key mergeMemberKey) bool {
	if group == nil || key.SourceID == "" {
		return true
	}
	for _, capture := range group.LegacyVersions {
		if capture.ServerID == key.ServerID && capture.ItemID == key.ItemID {
			return containsString(capture.SourceIDs, key.SourceID)
		}
	}
	return true // historical item scope, until a COMPLETE first observation
}

// First complete observation records the old item's existing version scope.
// Later additions are not silently absorbed by its preserved whole-item locator.
// No startup/upstream scan is performed and incomplete lists cannot freeze it.
func (s *IDStore) CaptureLegacyMergeVersions(candidate mergeCandidate) (bool, error) {
	if !candidate.SourcesComplete || (candidate.Identity.Type != "Movie" && candidate.Identity.Type != "Episode") {
		return false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.mergeStoreReadyLocked(); err != nil {
		return false, err
	}
	if !s.sourceAllowedForMergeLocked(candidate.ServerID) {
		return false, errors.New("merge source is not configured")
	}
	id := s.legacyMergeOwnerForItemLocked(candidate.ServerID, candidate.ItemID)
	if id == "" {
		return false, nil
	}
	group := s.legacyMergeGroupLocked(id, candidate.Identity.Type)
	if group == nil || group.Policy != mergePolicyLegacy || group.MediaType != candidate.Identity.Type {
		return false, nil
	}
	for _, capture := range group.LegacyVersions {
		if capture.ServerID == candidate.ServerID && capture.ItemID == candidate.ItemID {
			return true, nil
		}
	}
	capture := mergeLegacyVersions{ServerID: candidate.ServerID, ItemID: candidate.ItemID}
	for _, version := range candidate.Versions {
		if version.Ref.ServerID != candidate.ServerID || version.Ref.ItemID != candidate.ItemID ||
			strings.TrimSpace(version.Ref.MediaSourceID) == "" || containsString(capture.SourceIDs, version.Ref.MediaSourceID) {
			return false, errors.New("legacy version list is ambiguous")
		}
		capture.SourceIDs = append(capture.SourceIDs, version.Ref.MediaSourceID)
	}
	if len(capture.SourceIDs) == 0 {
		return false, nil
	}
	group.LegacyVersions = append(group.LegacyVersions, capture)
	if err := s.db.withWriteTx(func() error { return s.writeMergeGroupSQL(group, nil) }); err != nil {
		return false, err
	}
	s.publishMergeGroupLocked(group, nil)
	return true, nil
}

func (s *IDStore) nextMergeIDLocked() string {
	for {
		id := randomHex(16)
		if s.virtualToOriginal[id] == nil && s.mergeState.Groups[id] == nil && s.mergeState.Aliases[id] == "" {
			return id
		}
	}
}

// RegisterMergeCandidate validates the requested route, then retains all known versions.
func (s *IDStore) RegisterMergeCandidate(candidate mergeCandidate, sourceID string) (string, error) {
	if _, err := mergeMemberFromCandidate(candidate, sourceID); err != nil {
		return "", err
	}
	return s.RegisterMergeItem(candidate, "")
}

// A confirmed parent comes from persisted source-qualified membership, not a
// caller-supplied group string. Existing parent relations survive metadata edits.
func (s *IDStore) sameMergeParentLocked(a, b *mergeSeriesEvidence) bool {
	if a == nil || b == nil || a.Identity.Type != "Series" || b.Identity.Type != "Series" {
		return false
	}
	owner := func(parent *mergeSeriesEvidence) string {
		if id := s.mergeState.Owners[mergeMemberKey{parent.ServerID, parent.SeriesID, ""}]; id != "" {
			if group := s.mergeState.Groups[id]; group != nil && group.MediaType == "Series" {
				return id
			}
			return ""
		}
		id := s.legacyMergeOwnerForItemLocked(parent.ServerID, parent.SeriesID)
		if group := s.storedMergeGroupLocked(id); group != nil && group.MediaType != "Series" {
			return ""
		}
		return id
	}
	left, right := owner(a), owner(b)
	return left != "" && left == right
}

func (s *IDStore) AssociateMergePair(a, b mergeCandidate, sourceA, sourceB, target string) (string, error) {
	return s.associateMergeItems(a, b, sourceA, sourceB, target)
}

func mergeGroupHasMember(group *mergeStoredGroup, key mergeMemberKey) bool {
	for _, member := range group.Members {
		if mergeRefKey(member.Ref) == key {
			return true
		}
	}
	return false
}

// All writes here run under the existing connection transaction lock. Do not
// call any public store or publish memory inside this callback.
func (s *IDStore) writeMergeGroupSQL(group *mergeStoredGroup, absorbed []string) error {
	for _, id := range append(append([]string(nil), absorbed...), group.VirtualID) {
		for _, query := range []string{
			`DELETE FROM media_merge_members WHERE virtual_id = ?`,
			`DELETE FROM media_merge_aliases WHERE virtual_id = ?`,
			`DELETE FROM media_merge_groups WHERE virtual_id = ?`,
		} {
			if err := s.db.execParams(query, id); err != nil {
				return err
			}
		}
	}
	data, err := json.Marshal(group)
	if err != nil {
		return err
	}
	if err := s.db.execParams(`INSERT INTO media_merge_groups(virtual_id, snapshot) VALUES(?, ?)`, group.VirtualID, string(data)); err != nil {
		return err
	}
	for _, member := range group.Members {
		if err := s.db.execParams(`INSERT INTO media_merge_members(server_id, item_id, source_id, virtual_id) VALUES(?, ?, ?, ?)`,
			member.Ref.ServerID, member.Ref.ItemID, member.Ref.MediaSourceID, group.VirtualID); err != nil {
			return err
		}
	}
	for _, alias := range group.Aliases {
		if err := s.db.execParams(`INSERT INTO media_merge_aliases(alias_id, virtual_id) VALUES(?, ?)`, alias, group.VirtualID); err != nil {
			return err
		}
	}
	instances := mergeGroupInstances(group)
	if len(instances) == 0 {
		return errors.New("merge group has no routing locator")
	}
	for _, id := range append([]string{group.VirtualID}, group.Aliases...) {
		if err := s.db.execParams(`INSERT INTO id_mappings(virtual_id, original_id, server_id) VALUES(?, ?, ?)
 ON CONFLICT(virtual_id) DO UPDATE SET original_id=excluded.original_id, server_id=excluded.server_id`,
			id, instances[0].OriginalID, instances[0].ServerID); err != nil {
			return err
		}
		if err := s.db.execParams(`DELETE FROM id_additional_instances WHERE virtual_id = ?`, id); err != nil {
			return err
		}
		for _, instance := range instances[1:] {
			if err := s.db.execParams(`INSERT INTO id_additional_instances(virtual_id, original_id, server_id) VALUES(?, ?, ?)`,
				id, instance.OriginalID, instance.ServerID); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *IDStore) publishMergeGroupLocked(group *mergeStoredGroup, absorbed []string) {
	for _, id := range append([]string{group.VirtualID}, absorbed...) {
		if old := s.mergeState.Groups[id]; old != nil {
			for _, member := range old.Members {
				key := mergeRefKey(member.Ref)
				delete(s.mergeState.Owners, key)
				item := mergeItemKey{key.ServerID, key.ItemID}
				delete(s.mergeState.Items[item], id)
				if len(s.mergeState.Items[item]) == 0 {
					delete(s.mergeState.Items, item)
				}
			}
		}
	}
	for _, id := range absorbed {
		delete(s.mergeState.Groups, id)
	}
	copy := cloneMergeGroup(group)
	s.mergeState.Groups[copy.VirtualID] = copy
	for _, member := range copy.Members {
		key := mergeRefKey(member.Ref)
		itemKey := mergeItemKey{key.ServerID, key.ItemID}
		if s.mergeState.Items[itemKey] == nil {
			s.mergeState.Items[itemKey] = map[string]bool{}
		}
		delete(s.mergeState.Items[itemKey], s.mergeState.Owners[key])
		s.mergeState.Items[itemKey][copy.VirtualID] = true
		s.mergeState.Owners[key] = copy.VirtualID
	}
	for _, alias := range copy.Aliases {
		s.mergeState.Aliases[alias] = copy.VirtualID
	}
	s.projectMergeGroupLocked(copy)
	// Exact members have no unqualified raw index; legacy indices are unchanged.
	// Full rebuilding is needed only at load/removal, not per discovered version.
}

// A deletion plan changes only snapshots touched by this source. It is written
// inside the SAME lifecycle transaction as mappings/watch cleanup, then loaded
// or published only after that transaction succeeds.
func (s *IDStore) planMergeSourceRemovalLocked(serverID string, allowed map[string]bool) []*mergeStoredGroup {
	var changes []*mergeStoredGroup
	for _, existing := range s.mergeState.Groups {
		group := cloneMergeGroup(existing)
		touched := false
		keep := func(server string) bool {
			if server == serverID {
				touched = true
				return false
			}
			return allowed == nil || allowed[server]
		}
		group.Members = nil
		for _, member := range existing.Members {
			if keep(member.Ref.ServerID) {
				group.Members = append(group.Members, member)
			}
		}
		group.LegacyItems = nil
		group.LegacyVersions = nil
		for _, capture := range existing.LegacyVersions {
			if keep(capture.ServerID) {
				group.LegacyVersions = append(group.LegacyVersions, capture)
			}
		}
		for _, item := range existing.LegacyItems {
			if keep(item.ServerID) {
				group.LegacyItems = append(group.LegacyItems, item)
			}
		}
		if touched {
			changes = append(changes, group)
		}
	}
	return changes
}

func (s *IDStore) writeMergeRemovalSQL(changes []*mergeStoredGroup) error {
	for _, group := range changes {
		if len(mergeGroupInstances(group)) != 0 {
			if err := s.writeMergeGroupSQL(group, nil); err != nil {
				return err
			}
			continue
		}
		for _, query := range []string{`DELETE FROM media_merge_members WHERE virtual_id = ?`,
			`DELETE FROM media_merge_aliases WHERE virtual_id = ?`, `DELETE FROM media_merge_groups WHERE virtual_id = ?`} {
			if err := s.db.execParams(query, group.VirtualID); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *IDStore) publishMergeRemovalLocked(changes []*mergeStoredGroup) {
	for _, group := range changes {
		old := s.mergeState.Groups[group.VirtualID]
		for _, member := range old.Members {
			key := mergeRefKey(member.Ref)
			delete(s.mergeState.Owners, key)
			delete(s.mergeState.Items[mergeItemKey{key.ServerID, key.ItemID}], old.VirtualID)
		}
		for _, alias := range old.Aliases {
			delete(s.mergeState.Aliases, alias)
		}
		delete(s.mergeState.Groups, old.VirtualID)
		if len(mergeGroupInstances(group)) != 0 {
			s.publishMergeGroupLocked(group, nil)
		}
	}
	s.rebuildIndexesLocked()
}
