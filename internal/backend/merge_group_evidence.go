package backend

import (
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Persisted dimensions are deliberately independent: MediaSourceCompleteness
// cannot upgrade identity strength. Historic snapshots are lazily seeded from
// validated saved identity, without inventing observation provenance.
const (
	mergeTrustTrusted     = "trusted"
	mergeTrustReview      = "needs_revalidate"
	mergeTrustQuarantined = "conflict_quarantined"
)

type mergeEvidenceNameYear struct {
	Name       string
	Year       int64
	Valid      bool
	Provenance string
	ObservedAt string
}

type mergeEvidenceProvider struct {
	Namespace  string
	Value      string
	Provenance string
	ObservedAt string
}

type mergeEvidenceParent struct {
	ServerID   string
	SeriesID   string
	Provenance string
	ObservedAt string
}

type mergeEvidenceCompleteness struct {
	Complete    bool
	SourceCount int
	Provenance  string
	ObservedAt  string
}

type mergeMemberEvidence struct {
	NameYears    []mergeEvidenceNameYear
	Providers    []mergeEvidenceProvider
	Parents      []mergeEvidenceParent
	MediaSources []mergeEvidenceCompleteness
}

type mergeEvidenceConflict struct {
	Left        mergeVersionRef
	Right       mergeVersionRef
	Namespace   string
	LeftValue   string
	RightValue  string
	Reason      string
	ProofStatus string
}

func mergeEvidenceOrigin(candidate mergeCandidate) string {
	if origin := strings.TrimSpace(candidate.ObservationOrigin); origin != "" {
		return origin
	}
	return "passive_unknown"
}

func validObservedProvider(value string) string {
	value = strings.TrimSpace(value)
	switch strings.ToLower(value) {
	case "", "0", "null", "none", "unknown", "undefined", "n/a", "na", "?", "-":
		return ""
	}
	return value
}

func evidenceForObservation(candidate mergeCandidate, at string) mergeMemberEvidence {
	e := mergeMemberEvidence{}
	identity := candidate.Identity
	origin := mergeEvidenceOrigin(candidate)
	if strings.TrimSpace(identity.Name) != "" || identity.YearValid {
		e.NameYears = append(e.NameYears, mergeEvidenceNameYear{
			Name: identity.Name, Year: identity.Year, Valid: identity.YearValid,
			Provenance: origin, ObservedAt: at,
		})
	}
	for _, namespace := range mergeProviders {
		value := validObservedProvider(identity.ProviderIDs[namespace])
		if value == "" {
			continue
		}
		e.Providers = append(e.Providers, mergeEvidenceProvider{
			Namespace: namespace, Value: value, Provenance: origin, ObservedAt: at,
		})
	}
	if candidate.Parent != nil {
		e.Parents = append(e.Parents, mergeEvidenceParent{
			ServerID: candidate.Parent.ServerID, SeriesID: candidate.Parent.SeriesID,
			Provenance: origin, ObservedAt: at,
		})
	}
	if (identity.Type == "Movie" || identity.Type == "Episode") && (candidate.SourcesComplete || len(candidate.Versions) > 0) {
		e.MediaSources = append(e.MediaSources, mergeEvidenceCompleteness{
			Complete: candidate.SourcesComplete, SourceCount: len(candidate.Versions), Provenance: origin, ObservedAt: at,
		})
	}
	return e
}

func appendDistinctNameYears(dst []mergeEvidenceNameYear, incoming []mergeEvidenceNameYear) ([]mergeEvidenceNameYear, bool) {
	changed := false
	for _, n := range incoming {
		exists := false
		for _, v := range dst {
			if v.Name == n.Name && v.Year == n.Year && v.Valid == n.Valid {
				exists = true
				break
			}
		}
		if !exists {
			dst = append(dst, n)
			changed = true
		}
	}
	return dst, changed
}
func appendDistinctProviders(dst []mergeEvidenceProvider, incoming []mergeEvidenceProvider) ([]mergeEvidenceProvider, bool) {
	changed := false
	for _, n := range incoming {
		exists := false
		for _, v := range dst {
			if v.Namespace == n.Namespace && v.Value == n.Value {
				exists = true
				break
			}
		}
		if !exists {
			dst = append(dst, n)
			changed = true
		}
	}
	return dst, changed
}
func appendDistinctParent(dst []mergeEvidenceParent, incoming []mergeEvidenceParent) ([]mergeEvidenceParent, bool) {
	changed := false
	for _, n := range incoming {
		exists := false
		for _, v := range dst {
			if v.ServerID == n.ServerID && v.SeriesID == n.SeriesID {
				exists = true
				break
			}
		}
		if !exists {
			dst = append(dst, n)
			changed = true
		}
	}
	return dst, changed
}
func appendDistinctCompleteness(dst []mergeEvidenceCompleteness, incoming []mergeEvidenceCompleteness) ([]mergeEvidenceCompleteness, bool) {
	changed := false
	for _, n := range incoming {
		exists := false
		for _, v := range dst {
			if v.Complete == n.Complete && v.SourceCount == n.SourceCount {
				exists = true
				break
			}
		}
		if !exists {
			dst = append(dst, n)
			changed = true
		}
	}
	return dst, changed
}

func mergeMemberObservation(saved *mergeStoredMember, observed mergeStoredMember, candidate mergeCandidate) bool {
	at := time.Now().UTC().Format(time.RFC3339Nano)
	if saved.Evidence == nil {
		legacy := candidate
		legacy.Identity = saved.Identity
		legacy.Parent = saved.Parent
		legacy.SourcesComplete = false
		legacy.ObservationOrigin = "legacy_unknown"
		base := evidenceForObservation(legacy, "")
		saved.Evidence = &base
	}
	current := evidenceForObservation(candidate, at)
	changed := false
	if next, ok := appendDistinctNameYears(saved.Evidence.NameYears, current.NameYears); ok {
		saved.Evidence.NameYears = next
		changed = true
	}
	if next, ok := appendDistinctProviders(saved.Evidence.Providers, current.Providers); ok {
		saved.Evidence.Providers = next
		changed = true
	}
	if next, ok := appendDistinctParent(saved.Evidence.Parents, current.Parents); ok {
		saved.Evidence.Parents = next
		changed = true
	}
	if next, ok := appendDistinctCompleteness(saved.Evidence.MediaSources, current.MediaSources); ok {
		saved.Evidence.MediaSources = next
		changed = true
	}
	// Missing/partial provider fields cannot erase previously established IDs.
	if saved.Identity.ProviderIDs == nil {
		saved.Identity.ProviderIDs = map[string]string{}
	}
	for _, name := range mergeProviders {
		newValue := validObservedProvider(observed.Identity.ProviderIDs[name])
		if saved.Identity.ProviderIDs[name] == "" && newValue != "" {
			saved.Identity.ProviderIDs[name] = newValue
			changed = true
		}
	}
	if saved.Identity.Name == "" && observed.Identity.Name != "" {
		saved.Identity.Name = observed.Identity.Name
		changed = true
	}
	if !saved.Identity.YearValid && observed.Identity.YearValid {
		saved.Identity.Year = observed.Identity.Year
		saved.Identity.YearValid = true
		changed = true
	}
	if saved.Parent == nil && observed.Parent != nil {
		saved.Parent = observed.Parent
		changed = true
	}
	if !saved.Season.valid() && observed.Season.valid() {
		saved.Season = observed.Season
		changed = true
	}
	if !saved.Episode.valid() && observed.Episode.valid() {
		saved.Episode = observed.Episode
		changed = true
	}
	if !saved.Runtime.valid() && observed.Runtime.valid() {
		saved.Runtime = observed.Runtime
		saved.RuntimeOrigin = observed.RuntimeOrigin
		changed = true
	}
	return changed
}

type workEvidenceAnchor struct {
	Ref       mergeVersionRef
	Providers map[string]map[string]bool
	NameYears map[string]bool
	Parents   map[string]bool
}

func groupAnchors(group *mergeStoredGroup) []workEvidenceAnchor {
	// One work identity per source-qualified item; MediaSource versions are NOT
	// additional identity votes.
	works := map[mergeItemKey]*workEvidenceAnchor{}
	for _, m := range group.Members {
		if group.MediaType != "Movie" && group.MediaType != "Series" && group.MediaType != "Episode" && group.MediaType != "Season" {
			break
		}
		key := mergeItemKey{m.Ref.ServerID, m.Ref.ItemID}
		a := works[key]
		if a == nil {
			a = &workEvidenceAnchor{Ref: m.Ref, Providers: map[string]map[string]bool{}, NameYears: map[string]bool{}, Parents: map[string]bool{}}
			works[key] = a
		}
		if m.Identity.YearValid && strings.TrimSpace(m.Identity.Name) != "" {
			a.NameYears[m.Identity.Name+"\x00"+strconv.FormatInt(m.Identity.Year, 10)] = true
		}
		if m.Parent != nil && m.Parent.SeriesID != "" {
			a.Parents[m.Parent.ServerID+"\x00"+m.Parent.SeriesID] = true
		}
		if m.Evidence != nil {
			for _, ob := range m.Evidence.NameYears {
				if ob.Valid && strings.TrimSpace(ob.Name) != "" {
					a.NameYears[ob.Name+"\x00"+strconv.FormatInt(ob.Year, 10)] = true
				}
			}
			for _, ob := range m.Evidence.Parents {
				if ob.ServerID != "" && ob.SeriesID != "" {
					a.Parents[ob.ServerID+"\x00"+ob.SeriesID] = true
				}
			}
		}
		for _, ns := range mergeProviders {
			if a.Providers[ns] == nil {
				a.Providers[ns] = map[string]bool{}
			}
			if raw := validObservedProvider(m.Identity.ProviderIDs[ns]); raw != "" {
				a.Providers[ns][raw] = true
			}
		}
		if m.Evidence != nil {
			for _, ob := range m.Evidence.Providers {
				if a.Providers[ob.Namespace] == nil {
					a.Providers[ob.Namespace] = map[string]bool{}
				}
				if value := validObservedProvider(ob.Value); value != "" {
					a.Providers[ob.Namespace][value] = true
				}
			}
		}
	}
	keys := make([]mergeItemKey, 0, len(works))
	for key := range works {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].ServerID != keys[j].ServerID {
			return keys[i].ServerID < keys[j].ServerID
		}
		return keys[i].ItemID < keys[j].ItemID
	})
	out := make([]workEvidenceAnchor, 0, len(keys))
	for _, key := range keys {
		out = append(out, *works[key])
	}
	return out
}

func evaluateGroupEvidence(group *mergeStoredGroup) {
	if group == nil {
		return
	}
	previousTrust := group.TrustState
	anchors := groupAnchors(group)
	var conflicts []mergeEvidenceConflict
	hasConflict := false
	needsReview := false
	for i := range anchors {
		for j := i; j < len(anchors); j++ {
			left, right := anchors[i], anchors[j]
			if i == j {
				if len(left.NameYears) > 1 || len(left.Parents) > 1 {
					needsReview = true
				}
			} else {
				sharedStrong := false
				for _, ns := range mergeProviders {
					for value := range left.Providers[ns] {
						if right.Providers[ns][value] {
							sharedStrong = true
						}
					}
				}
				if !sharedStrong && len(left.NameYears) > 0 && len(right.NameYears) > 0 {
					matches := false
					for value := range left.NameYears {
						if right.NameYears[value] {
							matches = true
						}
					}
					if !matches {
						needsReview = true
					}
				}
			}
			for _, ns := range mergeProviders {
				for lv := range left.Providers[ns] {
					for rv := range right.Providers[ns] {
						if lv == rv {
							continue
						}
						hasConflict = true
						status := "legacy_unknown"
						for _, proof := range group.Proofs {
							if (proof.Left.Ref.ServerID == left.Ref.ServerID && proof.Left.Ref.ItemID == left.Ref.ItemID && proof.Right.Ref.ServerID == right.Ref.ServerID && proof.Right.Ref.ItemID == right.Ref.ItemID) ||
								(proof.Left.Ref.ServerID == right.Ref.ServerID && proof.Left.Ref.ItemID == right.Ref.ItemID && proof.Right.Ref.ServerID == left.Ref.ServerID && proof.Right.Ref.ItemID == left.Ref.ItemID) {
								status = "recorded_edge"
								break
							}
						}
						conflicts = append(conflicts, mergeEvidenceConflict{
							Left: left.Ref, Right: right.Ref, Namespace: ns, LeftValue: lv, RightValue: rv,
							Reason: "provider_conflict", ProofStatus: status,
						})
					}
				}
			}
		}
	}
	sort.Slice(conflicts, func(i, j int) bool {
		a, b := conflicts[i], conflicts[j]
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		if a.Left.ServerID != b.Left.ServerID {
			return a.Left.ServerID < b.Left.ServerID
		}
		if a.Left.ItemID != b.Left.ItemID {
			return a.Left.ItemID < b.Left.ItemID
		}
		if a.Right.ServerID != b.Right.ServerID {
			return a.Right.ServerID < b.Right.ServerID
		}
		if a.Right.ItemID != b.Right.ItemID {
			return a.Right.ItemID < b.Right.ItemID
		}
		if a.LeftValue != b.LeftValue {
			return a.LeftValue < b.LeftValue
		}
		return a.RightValue < b.RightValue
	})
	if previousTrust == mergeTrustQuarantined && len(conflicts) == 0 && len(group.ConflictEdges) > 0 {
		conflicts = append(conflicts, group.ConflictEdges...) // preserve auditable contradiction pending explicit repair
	}
	group.ConflictEdges = conflicts
	if hasConflict || previousTrust == mergeTrustQuarantined {
		// Sticky until authorized repair + complete group revalidation in a
		// future explicitly approved operation. Missing provider NEVER heals it.
		group.TrustState = mergeTrustQuarantined
	} else if needsReview {
		group.TrustState = mergeTrustReview
	} else {
		group.TrustState = mergeTrustTrusted
	}
}

func (s *IDStore) MergeGroupTrust(id string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	group := s.storedMergeGroupLocked(id)
	if group == nil {
		return mergeTrustTrusted
	}
	if group.TrustState == "" {
		return mergeTrustTrusted
	}
	return group.TrustState
}

func (s *IDStore) MergeGroupRouteAllowed(id, server, item, source string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	g := s.storedMergeGroupLocked(id)
	if g == nil || g.TrustState != mergeTrustQuarantined {
		return true
	}
	for _, member := range g.Members {
		if member.Ref.ServerID == server && member.Ref.ItemID == item && (source == "" || member.Ref.MediaSourceID == source) {
			return true
		}
	}
	return false
}

// Revalidate persisted legacy snapshots at startup without querying upstream.
// Only groups with a real strong contradiction need a one-time atomic upgrade;
// unknown historic proofs are NOT treated as invented positive identity proof.
func (s *IDStore) revalidatePersistedMergeGroups() error {
	ids := make([]string, 0, len(s.mergeState.Groups))
	for id := range s.mergeState.Groups {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var changed []*mergeStoredGroup
	for _, id := range ids {
		before := s.mergeState.Groups[id]
		after := cloneMergeGroup(before)
		evaluateGroupEvidence(after)
		if after.TrustState == mergeTrustQuarantined && !reflect.DeepEqual(before, after) {
			changed = append(changed, after)
		}
	}
	if len(changed) == 0 {
		return nil
	}
	err := s.db.withWriteTx(func() error {
		for _, group := range changed {
			if err := s.writeMergeGroupSQL(group, nil); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, group := range changed {
		s.publishMergeGroupLocked(group, nil)
	}
	return nil
}
