package backend

import (
	"encoding/json"
	"strings"
)

// These descriptors concern NEW work/episode associations only. They have no
// store, HTTP, watch-state or grouping side effects. Existing associations must
// be preserved by the Phase2 caller before invoking the new-association policy.
type mergeIdentity struct {
	Type        string
	Name        string
	Year        int64
	YearValid   bool
	ProviderIDs map[string]string
}

var mergeProviders = [...]string{"Tmdb", "Imdb", "Tvdb"}

func mergeIdentityFromItem(item map[string]any) mergeIdentity {
	kind, _ := item["Type"].(string)
	name, _ := item["Name"].(string)
	year := playbackTicksFromBody(item, "ProductionYear")
	ids := make(map[string]string)
	if raw, ok := item["ProviderIds"].(map[string]any); ok {
		for _, provider := range mergeProviders {
			if id, ok := raw[provider].(string); ok && strings.TrimSpace(id) != "" {
				ids[provider] = id
			}
		}
	}
	return mergeIdentity{
		Type: kind, Name: mergeLowerEnglish(name), Year: year.Ticks,
		YearValid:   year.valid() && year.Ticks > 0 && year.Ticks <= 9999,
		ProviderIDs: ids,
	}
}

// Fold only ASCII English letters. Preserve all spacing, punctuation and
// non-English characters; this is neither translation nor fuzzy matching.
func mergeLowerEnglish(name string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return r
	}, name)
}

type mergeReason string

const (
	mergeAllowed          mergeReason = "allowed"
	mergeTypeMismatch     mergeReason = "unsupported_or_different_type"
	mergeProviderConflict mergeReason = "provider_conflict"
	mergeIdentityMissing  mergeReason = "identity_incomplete"
	mergeIdentityMismatch mergeReason = "identity_mismatch"
	mergeSourceMissing    mergeReason = "source_incomplete"
	mergeSameServer       mergeReason = "same_server"
	mergeParentMissing    mergeReason = "parent_series_unproven"
	mergeNumberMissing    mergeReason = "number_incomplete"
	mergeNumberMismatch   mergeReason = "number_mismatch"
	mergeNoVersionMatch   mergeReason = "no_compatible_version"
	mergeVersionAmbiguous mergeReason = "version_ambiguous"
	mergeLiveVersion      mergeReason = "live_version"
)

// Return both presence and conflict: a matching Tmdb may not override a
// conflicting Imdb. Provider namespaces never compare against each other.
func compareMergeProviders(a, b mergeIdentity) (shared, conflict bool) {
	for _, provider := range mergeProviders {
		left, right := a.ProviderIDs[provider], b.ProviderIDs[provider]
		if left != "" && right != "" {
			shared = true
			if left != right {
				conflict = true
			}
		}
	}
	return
}

func compareMergeWork(a, b mergeIdentity) mergeReason {
	if a.Type != b.Type || (a.Type != "Movie" && a.Type != "Series") {
		return mergeTypeMismatch
	}
	shared, conflict := compareMergeProviders(a, b)
	if conflict {
		return mergeProviderConflict
	}
	if shared {
		return mergeAllowed
	}
	if strings.TrimSpace(a.Name) == "" || strings.TrimSpace(b.Name) == "" || !a.YearValid || !b.YearValid {
		return mergeIdentityMissing
	}
	if a.Name == b.Name && a.Year == b.Year {
		return mergeAllowed
	}
	return mergeIdentityMismatch
}

// A parent proof must come from a raw Series on this same server, not from an
// episode's SeriesName or its ProductionYear. The candidate binds it by SeriesId
// (or a Season's direct ParentId). Authorization remains the caller's duty.
type mergeSeriesEvidence struct {
	ServerID string
	SeriesID string
	Identity mergeIdentity
}

func newMergeSeriesEvidence(serverID string, series map[string]any) *mergeSeriesEvidence {
	id, _ := series["Id"].(string)
	identity := mergeIdentityFromItem(series)
	if strings.TrimSpace(serverID) == "" || strings.TrimSpace(id) == "" || identity.Type != "Series" {
		return nil
	}
	return &mergeSeriesEvidence{ServerID: serverID, SeriesID: id, Identity: identity}
}

type mergeVersionRef struct {
	ServerID      string
	ItemID        string
	MediaSourceID string
	SourceIndex   int
}

type mergeVersionCandidate struct {
	Ref           mergeVersionRef
	Runtime       playbackTickValue
	RuntimeOrigin string // media_source or proven_single_item; never watch history
	Blocked       mergeReason
}

type mergeCandidate struct {
	ServerID        string
	ItemID          string
	Identity        mergeIdentity
	Parent          *mergeSeriesEvidence
	Season          playbackTickValue
	Episode         playbackTickValue
	Versions        []mergeVersionCandidate
	SourcesComplete bool
}

func mergePositiveTicks(body map[string]any, field string) playbackTickValue {
	value := playbackTicksFromBody(body, field)
	if value.State == playbackTicksValid && value.Ticks == 0 {
		value.State = playbackTicksInvalid
	}
	return value
}

// fullSources is explicit evidence from the acquisition path that MediaSources
// is the complete list. A one-element selected/partial list alone is NOT proof
// that item-level runtime describes that sole version.
func newMergeCandidate(serverID string, item map[string]any, parent *mergeSeriesEvidence, fullSources bool) mergeCandidate {
	id, _ := item["Id"].(string)
	candidate := mergeCandidate{
		ServerID: serverID, ItemID: id, Identity: mergeIdentityFromItem(item),
		Season:  playbackTicksFromBody(item, "ParentIndexNumber"),
		Episode: playbackTicksFromBody(item, "IndexNumber"),
	}
	if candidate.Identity.Type == "Season" {
		candidate.Season = candidate.Episode
	}
	if parent != nil && parent.ServerID == serverID && parent.Identity.Type == "Series" &&
		strings.TrimSpace(parent.SeriesID) != "" {
		seriesID, _ := item["SeriesId"].(string)
		if _, present := item["SeriesId"]; !present && candidate.Identity.Type == "Season" {
			seriesID, _ = item["ParentId"].(string)
		}
		if seriesID == parent.SeriesID {
			copyParent := *parent
			copyParent.Identity.ProviderIDs = make(map[string]string)
			for k, v := range parent.Identity.ProviderIDs {
				copyParent.Identity.ProviderIDs[k] = v
			}
			candidate.Parent = &copyParent
		}
	}
	if candidate.Identity.Type != "Movie" && candidate.Identity.Type != "Episode" {
		return candidate
	}
	sources, ok := item["MediaSources"].([]any)
	if !ok || len(sources) == 0 {
		return candidate
	}
	candidate.SourcesComplete = fullSources
	countConsistent := true
	if _, present := item["MediaSourceCount"]; present {
		count := playbackTicksFromBody(item, "MediaSourceCount")
		countConsistent = count.valid() && count.Ticks == int64(len(sources))
		if !countConsistent {
			candidate.SourcesComplete = false
		}
	}
	single := candidate.SourcesComplete && countConsistent && len(sources) == 1
	frequencies := make(map[string]int)
	for _, raw := range sources {
		if source, ok := raw.(map[string]any); ok {
			sourceID, _ := source["Id"].(string)
			frequencies[sourceID]++
		}
	}
	for index, raw := range sources {
		version := mergeVersionCandidate{
			Ref:     mergeVersionRef{ServerID: serverID, ItemID: id, SourceIndex: index},
			Runtime: playbackTickValue{State: playbackTicksMissing},
		}
		source, validMap := raw.(map[string]any)
		if !validMap {
			version.Blocked = mergeVersionAmbiguous
			candidate.SourcesComplete = false
			candidate.Versions = append(candidate.Versions, version)
			continue
		}
		sourceID, _ := source["Id"].(string)
		version.Ref.MediaSourceID = sourceID
		if strings.TrimSpace(sourceID) == "" || frequencies[sourceID] != 1 {
			version.Blocked = mergeVersionAmbiguous
			candidate.SourcesComplete = false
		}
		if mergeHasLiveEvidence(item) || mergeHasLiveEvidence(source) {
			version.Blocked = mergeLiveVersion
		}
		version.Runtime = mergePositiveTicks(source, "RunTimeTicks")
		if version.Runtime.State == playbackTicksValid {
			version.RuntimeOrigin = "media_source"
		} else if version.Runtime.absent() && single && version.Blocked == "" {
			version.Runtime = mergePositiveTicks(item, "RunTimeTicks")
			if version.Runtime.State == playbackTicksValid {
				version.RuntimeOrigin = "proven_single_item"
			}
		}
		candidate.Versions = append(candidate.Versions, version)
	}
	return candidate
}

func mergeHasLiveEvidence(body map[string]any) bool {
	if raw, present := body["IsInfiniteStream"]; present && raw != nil {
		flag, ok := raw.(bool)
		if !ok || flag {
			return true
		}
	}
	if raw, present := body["LiveStreamId"]; present && raw != nil {
		id, ok := raw.(string)
		if !ok || strings.TrimSpace(id) != "" {
			return true
		}
	}
	return false
}

type mergeVersionPair struct {
	Left  mergeVersionRef
	Right mergeVersionRef
}

type mergeDecision struct {
	Allowed      bool
	Reason       mergeReason
	VersionPairs []mergeVersionPair
}

// Pairwise only: no transitive conflict audit. Same-server items use the same
// identity rules as cross-server items. Allowed proves the work/episode identity;
// VersionPairs lists qualified routes and never imposes a duration requirement.
func compareMergeCandidates(a, b mergeCandidate) mergeDecision {
	return compareMergeCandidatesWithSavedParent(a, b, false)
}

// savedParent is supplied only after the store proves existing typed membership.
// It preserves that relation even when its original identity metadata is absent.
func compareMergeCandidatesWithSavedParent(a, b mergeCandidate, savedParent bool) mergeDecision {
	reject := func(reason mergeReason) mergeDecision { return mergeDecision{Reason: reason} }
	if strings.TrimSpace(a.ServerID) == "" || strings.TrimSpace(b.ServerID) == "" ||
		strings.TrimSpace(a.ItemID) == "" || strings.TrimSpace(b.ItemID) == "" {
		return reject(mergeSourceMissing)
	}
	kind := a.Identity.Type
	if kind != b.Identity.Type {
		return reject(mergeTypeMismatch)
	}
	switch kind {
	case "Movie", "Series":
		if reason := compareMergeWork(a.Identity, b.Identity); reason != mergeAllowed {
			return reject(reason)
		}
	case "Season", "Episode":
		if a.Parent == nil || b.Parent == nil {
			return reject(mergeParentMissing)
		}
		if !savedParent {
			if reason := compareMergeWork(a.Parent.Identity, b.Parent.Identity); reason != mergeAllowed {
				return reject(reason)
			}
		}
		if _, conflict := compareMergeProviders(a.Identity, b.Identity); conflict {
			return reject(mergeProviderConflict)
		}
		if !a.Season.valid() || !b.Season.valid() {
			return reject(mergeNumberMissing)
		}
		if a.Season.Ticks != b.Season.Ticks {
			return reject(mergeNumberMismatch)
		}
		if kind == "Episode" {
			if !a.Episode.valid() || !b.Episode.valid() || a.Episode.Ticks == 0 || b.Episode.Ticks == 0 {
				return reject(mergeNumberMissing)
			}
			if a.Episode.Ticks != b.Episode.Ticks {
				return reject(mergeNumberMismatch)
			}
		}
	default:
		return reject(mergeTypeMismatch)
	}
	if kind == "Series" || kind == "Season" {
		return mergeDecision{Allowed: true, Reason: mergeAllowed}
	}
	var pairs []mergeVersionPair
	for _, left := range a.Versions {
		if !mergeVersionUsable(a, left) {
			continue
		}
		for _, right := range b.Versions {
			if mergeVersionUsable(b, right) {
				pairs = append(pairs, mergeVersionPair{Left: left.Ref, Right: right.Ref})
			}
		}
	}
	return mergeDecision{Allowed: true, Reason: mergeAllowed, VersionPairs: pairs}
}

// Route qualification remains source-specific even when runtime is unavailable.
// A missing/ambiguous/live route cannot manufacture a playable version, but it
// does not invalidate an otherwise proven work or episode identity.
func mergeVersionUsable(item mergeCandidate, version mergeVersionCandidate) bool {
	return version.Blocked == "" &&
		version.Ref.SourceIndex >= 0 && version.Ref.SourceIndex < len(item.Versions) &&
		version.Ref.ServerID == item.ServerID &&
		version.Ref.ItemID == item.ItemID && strings.TrimSpace(version.Ref.MediaSourceID) != ""
}

// Lookup hints are NOT proof of equality. Use every provider plus name/year,
// then run compareMergeCandidates on candidate pairs. A single priority key
// would miss fallback matches and conceal conflicts in a second provider.
func (identity mergeIdentity) lookupKeys() []string {
	if identity.Type != "Movie" && identity.Type != "Series" {
		return nil
	}
	var keys []string
	encode := func(parts ...any) string {
		data, _ := json.Marshal(parts)
		return string(data)
	}
	for _, provider := range mergeProviders {
		if id := identity.ProviderIDs[provider]; id != "" {
			keys = append(keys, encode(identity.Type, provider, id))
		}
	}
	if strings.TrimSpace(identity.Name) != "" && identity.YearValid {
		keys = append(keys, encode(identity.Type, "name_year", identity.Name, identity.Year))
	}
	return keys
}

// Candidate hints retain type, parent and season/episode boundaries. Runtime,
// source availability and the number of versions do not affect identity lookup.
// These hints still require pairwise proof before association.
func (candidate mergeCandidate) lookupKeys() []string {
	kind := candidate.Identity.Type
	identityKeys := candidate.Identity.lookupKeys()
	if kind == "Season" || kind == "Episode" {
		if candidate.Parent == nil || !candidate.Season.valid() {
			return nil
		}
		if kind == "Episode" && (!candidate.Episode.valid() || candidate.Episode.Ticks == 0) {
			return nil
		}
		identityKeys = candidate.Parent.Identity.lookupKeys()
	}
	var keys []string
	seen := map[string]bool{}
	add := func(parts ...any) {
		data, _ := json.Marshal(parts)
		key := string(data)
		if !seen[key] {
			seen[key] = true
			keys = append(keys, key)
		}
	}
	for _, identityKey := range identityKeys {
		switch kind {
		case "Movie", "Series":
			add(kind, identityKey)
		case "Season":
			add(kind, identityKey, candidate.Season.Ticks)
		case "Episode":
			add(kind, identityKey, candidate.Season.Ticks, candidate.Episode.Ticks)
		}
	}
	return keys
}

// The caller supplies a persisted parent owner. This is an additional lookup
// hint, not proof; store-level association still verifies typed parent ownership.
func (candidate mergeCandidate) savedParentLookupKey(parent string) string {
	kind := candidate.Identity.Type
	if parent == "" || candidate.Parent == nil || !candidate.Season.valid() ||
		(kind != "Season" && kind != "Episode") {
		return ""
	}
	if kind == "Episode" && (!candidate.Episode.valid() || candidate.Episode.Ticks == 0) {
		return ""
	}
	data, _ := json.Marshal([]any{"saved_parent", parent, kind, candidate.Season.Ticks, candidate.Episode.Ticks})
	return string(data)
}
