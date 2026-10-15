package backend

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// SearchHint responses commonly identify a media item with ItemId instead of Id.
// EIO must accept that wire shape and expose a virtual identity rather than
// silently dropping the hint during merge candidate construction.
func TestSearchHintsAcceptItemIDOnly(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"AccessToken": "upstream-token",
				"User":        map[string]any{"Id": "upstream-user"},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/Search/Hints":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"SearchHints": []map[string]any{{
					"ItemId":         "series-sntp-1",
					"Name":           "SNTP Hint",
					"Type":           "Series",
					"ProductionYear": 2026,
				}},
				"TotalRecordCount": 1,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	config := fmt.Sprintf("server:\n  port: 8096\n  name: \"Test Server\"\n  id: \"server-1\"\n\nadmin:\n  username: \"admin\"\n  password: \"secret\"\n\nplayback:\n  mode: \"proxy\"\n\ntimeouts:\n  api: 30000\n  global: 15000\n  login: 10000\n  healthCheck: 10000\n  healthInterval: 60000\n\nproxies: []\nupstream:\n  - name: \"SNTP\"\n    url: %q\n    username: \"u1\"\n    password: \"p1\"\n", upstream.URL)

	withTempAppConfig(t, config, func(_ *App, handler http.Handler) {
		token := loginToken(t, handler, "secret")
		rr := doJSONRequest(t, handler, http.MethodGet, "/Search/Hints?SearchTerm=sntp", nil, token)
		if rr.Code != http.StatusOK {
			t.Fatalf("Search/Hints status = %d, body=%s", rr.Code, rr.Body.String())
		}
		var payload map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		hints, _ := payload["SearchHints"].([]any)
		if len(hints) != 1 {
			t.Fatalf("SearchHints count = %d, want 1; payload=%#v", len(hints), payload)
		}
		hint := hints[0].(map[string]any)
		id, _ := hint["Id"].(string)
		itemID, _ := hint["ItemId"].(string)
		if id == "" || itemID == "" || id != itemID || id == "series-sntp-1" {
			t.Fatalf("SearchHint identity not safely virtualized: %#v", hint)
		}
	})
}

// Work identity is persistent state, so identical Movie/Series works discovered
// in separate HTTP requests must converge on the same merge group. A per-request
// lookup index is insufficient for scans, pagination and staggered upstreams.
func TestHistoricalWorkIdentityAssociatesAcrossRequests(t *testing.T) {
	cases := []struct {
		kind  string
		left  map[string]any
		right map[string]any
	}{
		{
			kind: "Movie",
			left: func() map[string]any {
				item := task6HTTPMovie("movie-a", task6HTTPSource("source-a", 1000))
				item["Name"] = "Historical Alpha"
				item["ProviderIds"] = map[string]any{"Tmdb": "777001"}
				return item
			}(),
			right: func() map[string]any {
				item := task6HTTPMovie("movie-b", task6HTTPSource("source-b", 1000))
				item["Name"] = "Historical Beta"
				item["ProviderIds"] = map[string]any{"Tmdb": "777001"}
				return item
			}(),
		},
		{
			kind: "Series",
			left: map[string]any{
				"Id": "series-a", "Type": "Series", "Name": "Historical Alpha",
				"ProductionYear": 2026, "ProviderIds": map[string]any{"Tmdb": "777002"},
			},
			right: map[string]any{
				"Id": "series-b", "Type": "Series", "Name": "Historical Beta",
				"ProductionYear": 2026, "ProviderIds": map[string]any{"Tmdb": "777002"},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			withTask6HTTPFixture(t, []map[string]any{tc.left}, []map[string]any{tc.right}, func(f *task6HTTPFixture) {
				request := func(term string) []map[string]any {
					path := "/Users/" + f.user + "/Items?Recursive=true&IncludeItemTypes=" + url.QueryEscape(tc.kind) + "&SearchTerm=" + url.QueryEscape(term)
					return asItems(task6HTTPJSON(t, f.request(t, http.MethodGet, path, nil, "")))
				}

				first := request("Historical Alpha")
				if len(first) != 1 {
					t.Fatalf("first request rows=%d, want 1: %#v", len(first), first)
				}
				firstID, _ := first[0]["Id"].(string)
				if firstID == "" {
					t.Fatal("first request missing virtual ID")
				}

				second := request("Historical Beta")
				if len(second) != 1 {
					t.Fatalf("second request rows=%d, want 1: %#v", len(second), second)
				}
				secondID, _ := second[0]["Id"].(string)
				if secondID != firstID {
					t.Fatalf("same persisted %s work split across requests: first=%q second=%q", tc.kind, firstID, secondID)
				}

				group := f.app.IDStore.ResolveMergeGroup(firstID)
				if group == nil {
					t.Fatalf("missing persisted merge group %q", firstID)
				}
				servers := map[string]bool{}
				for _, member := range group.Members {
					if member.Ref.MediaSourceID == "" {
						servers[member.Ref.ServerID] = true
					}
				}
				if !servers["server-a"] || !servers["server-b"] {
					t.Fatalf("historical group did not retain both upstream works: servers=%v group=%+v", servers, group)
				}
			})
		})
	}
}

func TestHistoricalWorkIdentityProviderConflictStillRejects(t *testing.T) {
	left := map[string]any{
		"Id": "series-a", "Type": "Series", "Name": "Conflict Work", "ProductionYear": 2026,
		"ProviderIds": map[string]any{"Tmdb": "888", "Imdb": "tt-left"},
	}
	right := map[string]any{
		"Id": "series-b", "Type": "Series", "Name": "Conflict Work", "ProductionYear": 2026,
		"ProviderIds": map[string]any{"Tmdb": "888", "Imdb": "tt-right"},
	}
	withTask6HTTPFixture(t, []map[string]any{left}, []map[string]any{right}, func(f *task6HTTPFixture) {
		firstPath := "/Users/" + f.user + "/Items?Recursive=true&IncludeItemTypes=Series&SearchTerm=" + url.QueryEscape("Conflict Work")
		rows := asItems(task6HTTPJSON(t, f.request(t, http.MethodGet, firstPath, nil, "")))
		// Both are present in one request and the existing policy must keep them separate.
		if len(rows) != 2 {
			t.Fatalf("provider conflict unexpectedly merged: %#v", rows)
		}
		ids := []string{rows[0]["Id"].(string), rows[1]["Id"].(string)}
		if ids[0] == ids[1] || strings.TrimSpace(ids[0]) == "" || strings.TrimSpace(ids[1]) == "" {
			t.Fatalf("provider conflict identities invalid: %v", ids)
		}
	})
}

func TestMergeCandidateAcceptsItemIDOnly(t *testing.T) {
	item := map[string]any{
		"ItemId": "sntp-item-only",
		"Type":   "Series",
		"Name":   "SNTP ItemId Shape",
	}
	candidate := newMergeCandidate("server-sntp", item, nil, false)
	if candidate.ItemID != "sntp-item-only" {
		t.Fatalf("candidate ItemID = %q, want ItemId fallback", candidate.ItemID)
	}
}

func TestPersistedWorkIndexFindsAndRemovesHistoricalSeries(t *testing.T) {
	identity := mergeIdentity{
		Type: "Series", Name: "historical series", Year: 2026, YearValid: true,
		ProviderIDs: map[string]string{"Tmdb": "282326", "Imdb": "tt32500958"},
	}
	group := &mergeStoredGroup{
		VirtualID: "historical-group", MediaType: "Series", Policy: mergePolicyWork,
		Members: []mergeStoredMember{{
			Ref:      mergeVersionRef{ServerID: "server-a", ItemID: "series-a", SourceIndex: -1},
			Identity: identity,
		}},
	}
	state := emptyMergeStoreState()
	state.Groups[group.VirtualID] = group
	addMergeGroupWorkIndex(&state, group)
	store := &IDStore{mergeState: state}
	candidate := mergeCandidate{
		ServerID: "server-b", ItemID: "series-b",
		Identity: mergeIdentity{Type: "Series", Name: "different title", ProviderIDs: map[string]string{"Tmdb": "282326"}},
	}
	found := store.HistoricalWorkCandidates(candidate)
	if len(found) != 1 || found[0].VirtualID != group.VirtualID ||
		found[0].Candidate.ServerID != "server-a" || found[0].Candidate.ItemID != "series-a" {
		t.Fatalf("historical lookup = %#v, want one persisted anchor", found)
	}

	removeMergeGroupWorkIndex(&state, group)
	store.mergeState = state
	if found := store.HistoricalWorkCandidates(candidate); len(found) != 0 {
		t.Fatalf("removed group remained in work index: %#v", found)
	}
}

func TestPersistedWorkIndexIsHintNotProviderConflictProof(t *testing.T) {
	left := mergeCandidate{
		ServerID: "server-a", ItemID: "series-a",
		Identity: mergeIdentity{Type: "Series", ProviderIDs: map[string]string{"Tmdb": "888", "Imdb": "tt-left"}},
	}
	right := mergeCandidate{
		ServerID: "server-b", ItemID: "series-b",
		Identity: mergeIdentity{Type: "Series", ProviderIDs: map[string]string{"Tmdb": "888", "Imdb": "tt-right"}},
	}
	sharedKey := false
	leftKeys := map[string]bool{}
	for _, key := range left.lookupKeys() {
		leftKeys[key] = true
	}
	for _, key := range right.lookupKeys() {
		if leftKeys[key] {
			sharedKey = true
		}
	}
	if !sharedKey {
		t.Fatal("fixture must share a lookup hint")
	}
	decision := compareMergeCandidates(left, right)
	if decision.Allowed || decision.Reason != mergeProviderConflict {
		t.Fatalf("lookup hint bypassed provider conflict: %+v", decision)
	}
}

func TestHistoricalSplitWorkGroupsHealWhenEncounteredAgain(t *testing.T) {
	cases := []struct {
		kind  string
		left  map[string]any
		right map[string]any
	}{
		{
			kind: "Movie",
			left: func() map[string]any {
				item := task6HTTPMovie("heal-movie-a", task6HTTPSource("heal-source-a", 1000))
				item["Name"] = "Heal Movie Alpha"
				item["ProviderIds"] = map[string]any{"Tmdb": "990001"}
				return item
			}(),
			right: func() map[string]any {
				item := task6HTTPMovie("heal-movie-b", task6HTTPSource("heal-source-b", 1000))
				item["Name"] = "Heal Movie Beta"
				item["ProviderIds"] = map[string]any{"Tmdb": "990001"}
				return item
			}(),
		},
		{
			kind:  "Series",
			left:  map[string]any{"Id": "heal-series-a", "Type": "Series", "Name": "Heal Series Alpha", "ProductionYear": 2026, "ProviderIds": map[string]any{"Tmdb": "990002"}},
			right: map[string]any{"Id": "heal-series-b", "Type": "Series", "Name": "Heal Series Beta", "ProductionYear": 2026, "ProviderIds": map[string]any{"Tmdb": "990002"}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			withTask6HTTPFixture(t, []map[string]any{tc.left}, []map[string]any{tc.right}, func(f *task6HTTPFixture) {
				leftCandidate := newMergeCandidate("server-a", tc.left, nil, true)
				rightCandidate := newMergeCandidate("server-b", tc.right, nil, true)
				leftID, err := f.app.IDStore.RegisterMergeCandidate(leftCandidate, "")
				if err != nil {
					t.Fatal(err)
				}
				rightID, err := f.app.IDStore.RegisterMergeCandidate(rightCandidate, "")
				if err != nil {
					t.Fatal(err)
				}
				if leftID == rightID {
					t.Fatalf("fixture did not create split groups: %q", leftID)
				}

				term, _ := tc.left["Name"].(string)
				path := "/Users/" + f.user + "/Items?Recursive=true&IncludeItemTypes=" + url.QueryEscape(tc.kind) + "&SearchTerm=" + url.QueryEscape(term)
				rows := asItems(task6HTTPJSON(t, f.request(t, http.MethodGet, path, nil, "")))
				if len(rows) != 1 {
					t.Fatalf("repair request rows=%d, want 1: %#v", len(rows), rows)
				}

				leftCanonical := f.app.IDStore.CanonicalMergeID(leftID)
				rightCanonical := f.app.IDStore.CanonicalMergeID(rightID)
				if leftCanonical == "" || leftCanonical != rightCanonical {
					t.Fatalf("existing split %s groups were not healed: left=%q right=%q", tc.kind, leftCanonical, rightCanonical)
				}
				group := f.app.IDStore.ResolveMergeGroup(leftCanonical)
				if group == nil {
					t.Fatalf("missing healed group %q", leftCanonical)
				}
				servers := map[string]bool{}
				for _, member := range group.Members {
					if member.Ref.MediaSourceID == "" {
						servers[member.Ref.ServerID] = true
					}
				}
				if !servers["server-a"] || !servers["server-b"] {
					t.Fatalf("healed group missing work anchors: %v", servers)
				}
			})
		})
	}
}

func TestPersistedWorkIndexRebuildsAfterRestart(t *testing.T) {
	dir := t.TempDir()
	store, err := NewIDStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	left := mergeCandidate{
		ServerID: "server-a", ItemID: "restart-series-a",
		Identity: mergeIdentity{Type: "Series", Name: "restart series", Year: 2026, YearValid: true, ProviderIDs: map[string]string{"Tmdb": "991001"}},
	}
	groupID, err := store.RegisterMergeCandidate(left, "")
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewIDStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	right := mergeCandidate{
		ServerID: "server-b", ItemID: "restart-series-b",
		Identity: mergeIdentity{Type: "Series", Name: "other title", ProviderIDs: map[string]string{"Tmdb": "991001"}},
	}
	found := reopened.HistoricalWorkCandidates(right)
	if len(found) != 1 || found[0].VirtualID != groupID || found[0].Candidate.ItemID != left.ItemID {
		t.Fatalf("restart work index = %#v, want persisted group %q", found, groupID)
	}
}
