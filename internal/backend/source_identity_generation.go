package backend

import "fmt"

// Epochs survive source deletion; Reload does not change a source identity.
const initialSourceGeneration int64 = 1

func (s *IDStore) initSourceGenerationSchema() error {
	if s.db == nil {
		return nil
	}
	return s.db.writeParams(`CREATE TABLE IF NOT EXISTS source_identity_generations (
 server_id TEXT PRIMARY KEY NOT NULL,
 generation INTEGER NOT NULL CHECK(generation > 0)
)`)
}

func (s *IDStore) loadSourceGenerationsLocked() error {
	next := map[string]int64{}
	if s.db == nil {
		s.sourceGenerations = next
		return nil
	}
	stmt, err := s.db.prepare(`SELECT server_id,generation FROM source_identity_generations`)
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
		server, version := stmt.columnText(0), stmt.columnInt64(1)
		if server == "" || version < initialSourceGeneration {
			return fmt.Errorf("invalid durable source generation")
		}
		next[server] = version
	}
	s.sourceGenerations = next
	return nil
}
func (s *IDStore) sourceGenerationLocked(source string) int64 {
	if source == "" {
		return 0
	}
	if v := s.sourceGenerations[source]; v > 0 {
		return v
	}
	return initialSourceGeneration
}

// Immutable snapshot of source epochs before any upstream work starts.
func (s *IDStore) snapshotSourceGenerations(available map[string]bool) map[string]int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make(map[string]int64, len(available))
	for source := range available {
		result[source] = s.sourceGenerationLocked(source)
	}
	return result
}

// Production HTTP contexts are always stamped at request admission. Legacy
// direct in-process/test contexts are not an implicit scanner grant.
func (s *IDStore) sourceGenerationMatches(reqCtx *RequestContext, source string) bool {
	if reqCtx == nil || source == "" {
		return false
	}
	if !reqCtx.SourceGenerationCaptured {
		return true
	}
	expected, ok := reqCtx.SourceGenerations[source]
	if !ok || expected < initialSourceGeneration {
		return false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return expected == s.sourceGenerationLocked(source)
}

// Within the atomic source cleanup transaction. A failed delete transaction
// does not advance the epoch. Retain rows forever to prevent ID reuse ABA.
func (s *IDStore) advanceSourceGenerationSQL(source string) error {
	if source == "" {
		return fmt.Errorf("empty deleted source identity")
	}
	return s.db.execParams(`INSERT INTO source_identity_generations(server_id,generation)
 VALUES (?,2) ON CONFLICT(server_id) DO UPDATE SET
 generation=source_identity_generations.generation+1`, source)
}
