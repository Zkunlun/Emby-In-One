package backend

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

// WorkIdentityIndex is only a persisted candidate locator derived from MergeStore.
// A returned candidate is never an association proof or an authorization grant.
type workIdentityCandidate struct {
	ServerID          string
	ItemID            string
	Identity          mergeIdentity
	CanonicalGroupIDs []string
}
type workItemKey struct{ ServerID, ItemID string }

func (s *IDStore) initWorkIdentitySchema() error {
	return s.db.withWriteTx(func() error {
		return s.db.exec(`
CREATE TABLE IF NOT EXISTS work_identity_items (
 server_id TEXT NOT NULL, item_id TEXT NOT NULL, media_type TEXT NOT NULL,
 identity_json TEXT NOT NULL, canonical_hint TEXT NOT NULL,
 PRIMARY KEY(server_id,item_id)
);
CREATE TABLE IF NOT EXISTS work_identity_keys (
 lookup_key TEXT NOT NULL, server_id TEXT NOT NULL, item_id TEXT NOT NULL,
 PRIMARY KEY(lookup_key,server_id,item_id)
);
CREATE INDEX IF NOT EXISTS idx_work_identity_source ON work_identity_keys(server_id,item_id);
`)
	})
}

func qualifiedWorkIdentity(value mergeIdentity) (mergeIdentity, []string, bool) {
	if value.Type != "Movie" && value.Type != "Series" {
		return mergeIdentity{}, nil, false
	}
	result := value
	result.ProviderIDs = map[string]string{}
	for _, namespace := range mergeProviders {
		id := strings.TrimSpace(value.ProviderIDs[namespace])
		switch strings.ToLower(id) {
		case "", "0", "null", "none", "unknown", "undefined", "n/a", "na", "?", "-":
			continue
		}
		result.ProviderIDs[namespace] = id
	}
	if strings.TrimSpace(result.Name) == "" || strings.EqualFold(strings.TrimSpace(result.Name), "unknown") || strings.EqualFold(strings.TrimSpace(result.Name), "n/a") {
		result.Name = ""
	}
	if result.Year <= 0 || result.Year > 9999 {
		result.YearValid = false
	}
	result.Name = mergeLowerEnglish(result.Name)
	keys := result.lookupKeys()
	return result, keys, len(keys) > 0
}

// Caller is already in the MergeStore SQLite transaction. No store lock may be re-acquired.
func (s *IDStore) workIndexAffectedGroupItemsSQL(groupIDs []string) (map[workItemKey]bool, error) {
	affected := map[workItemKey]bool{}
	for _, id := range groupIDs {
		stmt, err := s.db.prepare(`SELECT DISTINCT server_id,item_id FROM media_merge_members WHERE virtual_id=?`)
		if err != nil {
			return nil, err
		}
		if err = stmt.bindAll(id); err != nil {
			stmt.finalize()
			return nil, err
		}
		for {
			ok, stepErr := stmt.step()
			if stepErr != nil {
				stmt.finalize()
				return nil, stepErr
			}
			if !ok {
				break
			}
			affected[workItemKey{stmt.columnText(0), stmt.columnText(1)}] = true
		}
		stmt.finalize()
	}
	return affected, nil
}

func (s *IDStore) refreshWorkIdentityItemsSQL(items map[workItemKey]bool) error {
	keys := make([]workItemKey, 0, len(items))
	for k := range items {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].ServerID == keys[j].ServerID {
			return keys[i].ItemID < keys[j].ItemID
		}
		return keys[i].ServerID < keys[j].ServerID
	})
	for _, k := range keys {
		if err := s.refreshWorkIdentityItemSQL(k); err != nil {
			return err
		}
	}
	return nil
}

func (s *IDStore) refreshWorkIdentityItemSQL(k workItemKey) error {
	if err := s.db.execParams(`DELETE FROM work_identity_keys WHERE server_id=? AND item_id=?`, k.ServerID, k.ItemID); err != nil {
		return err
	}
	if err := s.db.execParams(`DELETE FROM work_identity_items WHERE server_id=? AND item_id=?`, k.ServerID, k.ItemID); err != nil {
		return err
	}
	if strings.TrimSpace(k.ServerID) == "" || strings.TrimSpace(k.ItemID) == "" {
		return nil
	}
	stmt, err := s.db.prepare(`SELECT g.virtual_id,g.snapshot FROM media_merge_members m JOIN media_merge_groups g ON g.virtual_id=m.virtual_id
 WHERE m.server_id=? AND m.item_id=? AND m.source_id='' ORDER BY g.virtual_id`)
	if err != nil {
		return err
	}
	if err = stmt.bindAll(k.ServerID, k.ItemID); err != nil {
		stmt.finalize()
		return err
	}
	var identity mergeIdentity
	var hints []string
	var groups []string
	for {
		more, stepErr := stmt.step()
		if stepErr != nil {
			stmt.finalize()
			return stepErr
		}
		if !more {
			break
		}
		var group mergeStoredGroup
		if err = json.Unmarshal([]byte(stmt.columnText(1)), &group); err != nil {
			stmt.finalize()
			return fmt.Errorf("work index merge snapshot: %w", err)
		}
		if group.MediaType != "Movie" && group.MediaType != "Series" {
			continue
		}
		var found *mergeStoredMember
		for i := range group.Members {
			m := &group.Members[i]
			if m.Ref.ServerID == k.ServerID && m.Ref.ItemID == k.ItemID && m.Ref.MediaSourceID == "" && m.Ref.SourceIndex == -1 {
				found = m
				break
			}
		}
		if found == nil {
			continue
		}
		next, nextHints, valid := qualifiedWorkIdentity(found.Identity)
		if !valid {
			continue
		}
		if len(groups) > 0 && !reflect.DeepEqual(identity, next) {
			// Contradictory typed anchor observations must not be indexed arbitrarily.
			stmt.finalize()
			return nil
		}
		if len(groups) == 0 {
			identity = next
			hints = nextHints
		}
		groups = append(groups, group.VirtualID)
	}
	stmt.finalize()
	if len(groups) == 0 {
		return nil
	}
	sort.Strings(groups)
	canonical := ""
	if len(groups) == 1 {
		canonical = groups[0]
	}
	data, err := json.Marshal(identity)
	if err != nil {
		return err
	}
	if err = s.db.execParams(`INSERT INTO work_identity_items(server_id,item_id,media_type,identity_json,canonical_hint) VALUES(?,?,?,?,?)`,
		k.ServerID, k.ItemID, identity.Type, string(data), canonical); err != nil {
		return err
	}
	for _, key := range hints {
		if err = s.db.execParams(`INSERT INTO work_identity_keys(lookup_key,server_id,item_id) VALUES(?,?,?)`, key, k.ServerID, k.ItemID); err != nil {
			return err
		}
	}
	return nil
}

// Rebuild deterministically and atomically from authoritative in-memory MergeStore
// snapshots. It never changes the MergeStore or performs upstream network I/O.
func (s *IDStore) rebuildWorkIdentityIndexLocked() error {
	if s.db == nil || !s.persistent {
		return nil
	}
	return s.db.withWriteTx(func() error {
		items := map[workItemKey]bool{}
		for _, g := range s.mergeState.Groups {
			for _, m := range g.Members {
				items[workItemKey{m.Ref.ServerID, m.Ref.ItemID}] = true
			}
		}
		if err := s.db.exec(`DELETE FROM work_identity_keys; DELETE FROM work_identity_items;`); err != nil {
			return err
		}
		return s.refreshWorkIdentityItemsSQL(items)
	})
}

func (s *IDStore) RebuildWorkIdentityIndex() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.mergeStoreReadyLocked(); err != nil {
		return err
	}
	return s.rebuildWorkIdentityIndexLocked()
}

// Query is source-qualified and permission-agnostic by design. A caller MUST
// subsequently validate against MergeStore and the current access scope.
func (s *IDStore) FindWorkIdentityCandidates(identity mergeIdentity) ([]workIdentityCandidate, bool, error) {
	clean, keys, valid := qualifiedWorkIdentity(identity)
	if !valid {
		return nil, false, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if err := s.mergeStoreReadyLocked(); err != nil {
		return nil, false, err
	}
	found := map[workItemKey]workIdentityCandidate{}
	for _, hint := range keys {
		stmt, err := s.db.prepare(`SELECT i.server_id,i.item_id,i.identity_json FROM work_identity_keys k JOIN work_identity_items i
  ON i.server_id=k.server_id AND i.item_id=k.item_id
  WHERE k.lookup_key=? AND i.media_type=? ORDER BY i.server_id,i.item_id`)
		if err != nil {
			return nil, false, err
		}
		if err = stmt.bindAll(hint, clean.Type); err != nil {
			stmt.finalize()
			return nil, false, err
		}
		for {
			more, stepErr := stmt.step()
			if stepErr != nil {
				stmt.finalize()
				return nil, false, stepErr
			}
			if !more {
				break
			}
			k := workItemKey{stmt.columnText(0), stmt.columnText(1)}
			if _, exists := found[k]; exists {
				continue
			}
			var indexed mergeIdentity
			if err = json.Unmarshal([]byte(stmt.columnText(2)), &indexed); err != nil {
				stmt.finalize()
				return nil, false, err
			}
			owners := s.mergeState.Items[mergeItemKey{ServerID: k.ServerID, ItemID: k.ItemID}]
			var groupIDs []string
			for id := range owners {
				group := s.mergeState.Groups[id]
				if group == nil || group.MediaType != clean.Type {
					continue
				}
				groupIDs = append(groupIDs, s.canonicalMergeIDLocked(id))
			}
			sort.Strings(groupIDs)
			if len(groupIDs) == 0 {
				continue
			}
			found[k] = workIdentityCandidate{ServerID: k.ServerID, ItemID: k.ItemID, Identity: indexed, CanonicalGroupIDs: groupIDs}
		}
		stmt.finalize()
	}
	result := make([]workIdentityCandidate, 0, len(found))
	allGroups := map[string]bool{}
	for _, v := range found {
		result = append(result, v)
		for _, id := range v.CanonicalGroupIDs {
			allGroups[id] = true
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].ServerID == result[j].ServerID {
			return result[i].ItemID < result[j].ItemID
		}
		return result[i].ServerID < result[j].ServerID
	})
	return result, len(allGroups) > 1, nil
}