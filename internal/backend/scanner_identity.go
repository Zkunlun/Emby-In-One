package backend

import (
	"errors"
	"fmt"
	"sort"
)

// scanWriteGrant is NOT a user session. It can be obtained only from the
// authorized scanner dispatcher and is revalidated after EVERY outbound page.
// Scanner never manufactures an admin RequestContext or bypasses the passive
// merge service's authenticated-user checks.
type scanWriteGrant struct {
	Source     string
	RunID      string
	Generation int64
}

func (d mergeDiscoveryService) scannerAuthorizedLocked(grant scanWriteGrant) error {
	a := d.app
	if a.Scanner == nil || a.IDStore == nil || a.lifecyclePending || grant.Source == "" || grant.RunID == "" {
		return errScanStale
	}
	if !configuredSourceIDs(a.ConfigStore.Snapshot())[grant.Source] {
		return errScanStale
	}
	a.Scanner.mu.Lock()
	defer a.Scanner.mu.Unlock()
	run, err := a.Scanner.runLocked(grant.Source, true)
	if err != nil {
		return err
	}
	if run == nil || run.ID != grant.RunID || run.Generation != grant.Generation {
		return errScanStale
	}
	switch run.State {
	case scanScanning, scanPausedActivity, scanPausedAdmin, scanPausedPermission:
		// A granted in-flight HTTP page is allowed to finish when the admin pauses,
		// but STOP/deletion/ABA never receives commit authority.
	default:
		return errScanStale
	}
	a.IDStore.mu.RLock()
	matches := a.IDStore.sourceGenerationLocked(grant.Source) == grant.Generation
	a.IDStore.mu.RUnlock()
	if !matches {
		return errScanStale
	}
	return nil
}

// registerScannerObservation uses existing MergeStore and WorkIdentityIndex:
// lookup narrows candidates, compareMergeCandidates proves compatible evidence,
// MergeStore remains the canonical mutation authority, quarantine remains live.
// All writes happen before the worker advances its durable page checkpoint.
func (d mergeDiscoveryService) registerScannerObservation(grant scanWriteGrant, raw map[string]any) (string, error) {
	a := d.app
	if raw == nil {
		return "", errors.New("empty scanner observation")
	}
	// Scanner must never attach versions or infer runtime completeness from an
	// upstream listing. It only saves lightweight work identity evidence.
	item := map[string]any{}
	for _, key := range []string{"Id", "Type", "Name", "ProductionYear", "ProviderIds"} {
		if val, ok := raw[key]; ok {
			item[key] = val
		}
	}
	candidate := newMergeCandidate(grant.Source, item, nil, false)
	candidate.ObservationOrigin = "scanner_list"
	if candidate.ItemID == "" || (candidate.Identity.Type != "Movie" && candidate.Identity.Type != "Series") {
		return "", fmt.Errorf("invalid scanner candidate type/id")
	}
	a.watchLifecycleMu.Lock()
	defer a.watchLifecycleMu.Unlock()
	if err := d.scannerAuthorizedLocked(grant); err != nil {
		return "", err
	}
	hints, _, err := a.IDStore.FindWorkIdentityCandidates(candidate.Identity)
	if err != nil {
		return "", err
	}
	before := d.before(candidate)
	id, err := a.IDStore.RegisterMergeItem(independentMergeCandidate(candidate), "")
	if err != nil {
		return "", err
	}
	d.publish(id, before)
	// A key is only a hint. If several distinct canonical groups compare as
	// matching, never arbitrarily choose one.
	matching := map[string]mergeCandidate{}
	ambiguous := false
	for _, hit := range hints {
		if hit.ServerID == candidate.ServerID && hit.ItemID == candidate.ItemID {
			continue
		}
		prior := mergeCandidate{ServerID: hit.ServerID, ItemID: hit.ItemID, Identity: hit.Identity, ObservationOrigin: "scanner_list"}
		if !compareMergeCandidates(prior, candidate).Allowed {
			continue
		}
		if len(hit.CanonicalGroupIDs) != 1 {
			ambiguous = true
			continue
		}
		canonical := a.IDStore.CanonicalMergeID(hit.CanonicalGroupIDs[0])
		if canonical == "" {
			ambiguous = true
			continue
		}
		matching[canonical] = prior
	}
	if ambiguous || len(matching) != 1 {
		return id, nil
	}
	ordered := make([]string, 0, len(matching))
	for key := range matching {
		ordered = append(ordered, key)
	}
	sort.Strings(ordered)
	peer := matching[ordered[0]]
	snapshot := d.before(candidate, peer)
	merged, err := a.IDStore.AssociateMergePair(peer, candidate, "", "", ordered[0])
	if err != nil {
		// Compatibility or quarantine can have changed since the hint was taken:
		// leave independently registered evidence available for later revalidation.
		if errors.Is(err, errMergeGroupQuarantined) {
			return id, nil
		}
		return "", err
	}
	d.publish(merged, snapshot)
	return merged, nil
}
