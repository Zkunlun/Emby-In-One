package backend

import "encoding/json"

// These read-only status queries share scanner.mu with all transitions.
func (s *ScannerControl) checkpointsLocked(source string) ([]scanLibraryCheckpoint, error) {
	stmt, err := s.db.prepare(`SELECT payload FROM scanner_checkpoints WHERE source_id=? ORDER BY library_id`)
	if err != nil {
		return nil, err
	}
	defer stmt.finalize()
	if err := stmt.bindAll(source); err != nil {
		return nil, err
	}
	rows := []scanLibraryCheckpoint{}
	for {
		found, err := stmt.step()
		if err != nil {
			return nil, err
		}
		if !found {
			break
		}
		var point scanLibraryCheckpoint
		if err := json.Unmarshal([]byte(stmt.columnText(0)), &point); err != nil {
			return nil, err
		}
		rows = append(rows, point)
	}
	return rows, nil
}
func (s *ScannerControl) librariesLocked(source string) ([]map[string]any, error) {
	stmt, err := s.db.prepare(`SELECT library_id,committed_cursor,initial_completed,
 full_safe_watermark,capability,inactive FROM scanner_libraries
 WHERE source_id=? ORDER BY library_id`)
	if err != nil {
		return nil, err
	}
	defer stmt.finalize()
	if err := stmt.bindAll(source); err != nil {
		return nil, err
	}
	rows := []map[string]any{}
	for {
		found, err := stmt.step()
		if err != nil {
			return nil, err
		}
		if !found {
			break
		}
		rows = append(rows, map[string]any{"libraryId": stmt.columnText(0),
			"committedCursor": stmt.columnText(1), "initialCompleted": stmt.columnInt(2) == 1,
			"fullSafeWatermark": stmt.columnText(3), "capability": stmt.columnText(4),
			"inactive": stmt.columnInt(5) == 1})
	}
	return rows, nil
}
func (s *ScannerControl) circuitLocked(source string) (map[string]any, error) {
	stmt, err := s.db.prepare(`SELECT state,failure_count,retry_at,last_error
 FROM scanner_circuits WHERE source_id=?`)
	if err != nil {
		return nil, err
	}
	defer stmt.finalize()
	if err := stmt.bindAll(source); err != nil {
		return nil, err
	}
	found, err := stmt.step()
	if err != nil {
		return nil, err
	}
	if !found {
		return map[string]any{"state": "closed", "failureCount": 0}, nil
	}
	return map[string]any{"state": stmt.columnText(0), "failureCount": stmt.columnInt(1),
		"retryAt": stmt.columnText(2), "lastError": stmt.columnText(3)}, nil
}
