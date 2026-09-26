package backend

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestUserViewsUsesCompleteLocalUserStateForRegularUsers(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "tok-a", "User": map[string]any{"Id": "user-a"}})
		case r.Method == http.MethodGet && r.URL.Path == "/Users/user-a/Views":
			_ = json.NewEncoder(w).Encode(map[string]any{"Items": []any{map[string]any{
				"Id": "movie-a", "Name": "Movie", "Type": "Movie",
				"UserData": map[string]any{
					"ItemId": "movie-a", "PlaybackPositionTicks": 800, "Played": true, "IsFavorite": false,
					"PlayedPercentage": 80, "LastPlayedDate": "2020-01-01T00:00:00Z",
					"PlayCount": 7, "UnplayedItemCount": 3, "Rating": 9,
				},
			}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	withTempAppConfig(t, singleUpstreamConfig(upstream.URL), func(app *App, handler http.Handler) {
		adminToken := loginTokenAs(t, handler, "admin", "secret")
		userToken := createRegularUser(t, handler)
		serverID := app.Upstream.Clients()[0].ID
		virtualID := app.IDStore.GetOrCreateVirtualID("movie-a", serverID)
		userID := watchUserID(t, app, "child")
		playedAt := time.Date(2026, 9, 25, 8, 32, 5, 0, time.UTC).UnixMilli()
		if err := app.WatchStore.RecordProgress(&WatchProgress{
			ProxyUserID: userID, VirtualItemID: virtualID, ServerID: serverID, OriginalItemID: "movie-a",
			ItemType: "Movie", PositionTicks: 25, RuntimeTicks: 100, IsFavorite: true, LastPlayed: playedAt,
		}); err != nil {
			t.Fatalf("RecordProgress: %v", err)
		}

		rr := doJSONRequest(t, handler, http.MethodGet, "/Users/"+app.Auth.ProxyUserID()+"/Views", nil, userToken)
		if rr.Code != http.StatusOK {
			t.Fatalf("regular views status=%d body=%s", rr.Code, rr.Body.String())
		}
		ud := firstResponseUserData(t, rr.Body.Bytes())
		if got, _ := numericInt64(ud["PlaybackPositionTicks"]); got != 25 {
			t.Fatalf("regular position=%v want 25", ud["PlaybackPositionTicks"])
		}
		if ud["Played"] != false || ud["IsFavorite"] != true {
			t.Fatalf("regular state not local: %#v", ud)
		}
		if pct, _ := ud["PlayedPercentage"].(float64); pct != 25 {
			t.Fatalf("regular percentage=%v want 25", ud["PlayedPercentage"])
		}
		if got, _ := ud["LastPlayedDate"].(string); got != "2026-09-25T08:32:05Z" {
			t.Fatalf("regular LastPlayedDate=%q", got)
		}
		for _, key := range []string{"PlayCount", "UnplayedItemCount", "Rating"} {
			if _, ok := ud[key]; ok {
				t.Fatalf("regular response leaked upstream %s: %#v", key, ud)
			}
		}

		rr = doJSONRequest(t, handler, http.MethodGet, "/Users/"+app.Auth.ProxyUserID()+"/Views", nil, adminToken)
		if rr.Code != http.StatusOK {
			t.Fatalf("admin views status=%d body=%s", rr.Code, rr.Body.String())
		}
		adminUD := firstResponseUserData(t, rr.Body.Bytes())
		if adminUD["Played"] != true || adminUD["IsFavorite"] != false {
			t.Fatalf("admin state should remain upstream: %#v", adminUD)
		}
		if got, _ := numericInt64(adminUD["PlayCount"]); got != 7 {
			t.Fatalf("admin PlayCount=%v want upstream 7", adminUD["PlayCount"])
		}
	})
}

func TestFallbackProxyClearsSharedUserDataForRegularUser(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "tok-a", "User": map[string]any{"Id": "user-a"}})
		case r.Method == http.MethodGet && r.URL.Path == "/Users/user-a/CustomEndpoint/item-a":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Id": "item-a", "Type": "Movie",
				"UserData": map[string]any{
					"ItemId": "item-a", "PlaybackPositionTicks": 999, "Played": true, "IsFavorite": true,
					"PlayedPercentage": 99, "LastPlayedDate": "2020-01-01T00:00:00Z", "PlayCount": 4,
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	withTempAppConfig(t, singleUpstreamConfig(upstream.URL), func(app *App, handler http.Handler) {
		userToken := createRegularUser(t, handler)
		virtualID := app.IDStore.GetOrCreateVirtualID("item-a", app.Upstream.Clients()[0].ID)
		rr := doJSONRequest(t, handler, http.MethodGet, "/Users/"+app.Auth.ProxyUserID()+"/CustomEndpoint/"+virtualID, nil, userToken)
		if rr.Code != http.StatusOK {
			t.Fatalf("fallback status=%d body=%s", rr.Code, rr.Body.String())
		}
		var item map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &item); err != nil {
			t.Fatal(err)
		}
		ud, _ := item["UserData"].(map[string]any)
		if ud["Played"] != false || ud["IsFavorite"] != false {
			t.Fatalf("fallback leaked shared flags: %#v", ud)
		}
		if pos, _ := numericInt64(ud["PlaybackPositionTicks"]); pos != 0 {
			t.Fatalf("fallback leaked position: %#v", ud)
		}
		if _, ok := ud["LastPlayedDate"]; ok {
			t.Fatalf("fallback leaked LastPlayedDate: %#v", ud)
		}
		if _, ok := ud["PlayCount"]; ok {
			t.Fatalf("fallback leaked PlayCount: %#v", ud)
		}
	})
}

func TestFallbackProxyMaterializesMissingUserDataFromLocalState(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "tok-a", "User": map[string]any{"Id": "user-a"}})
		case r.Method == http.MethodGet && r.URL.Path == "/Users/user-a/CustomEndpoint/item-a":
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": "item-a", "Type": "Movie", "Name": "Movie"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	withTempAppConfig(t, singleUpstreamConfig(upstream.URL), func(app *App, handler http.Handler) {
		userToken := createRegularUser(t, handler)
		serverID := app.Upstream.Clients()[0].ID
		virtualID := app.IDStore.GetOrCreateVirtualID("item-a", serverID)
		userID := watchUserID(t, app, "child")
		if err := app.WatchStore.RecordProgress(&WatchProgress{
			ProxyUserID: userID, VirtualItemID: virtualID, ServerID: serverID, OriginalItemID: "item-a",
			ItemType: "Movie", PositionTicks: 250, RuntimeTicks: 1000, IsFavorite: true,
		}); err != nil {
			t.Fatal(err)
		}

		rr := doJSONRequest(t, handler, http.MethodGet, "/Users/"+app.Auth.ProxyUserID()+"/CustomEndpoint/"+virtualID, nil, userToken)
		if rr.Code != http.StatusOK {
			t.Fatalf("fallback status=%d body=%s", rr.Code, rr.Body.String())
		}
		var item map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &item); err != nil {
			t.Fatal(err)
		}
		ud, _ := item["UserData"].(map[string]any)
		if ud == nil {
			t.Fatalf("local state was not materialized: %#v", item)
		}
		if pos, _ := numericInt64(ud["PlaybackPositionTicks"]); pos != 250 || ud["Played"] != false || ud["IsFavorite"] != true {
			t.Fatalf("materialized UserData=%#v", ud)
		}
	})
}

func TestWatchStoreFavoriteDoesNotManufactureLastPlayed(t *testing.T) {
	ws := newTestWatchStore(t)
	if err := ws.SetFavorite("user1", "item1", true); err != nil {
		t.Fatal(err)
	}
	got := ws.GetProgress("user1", "item1")
	if got == nil || !got.IsFavorite {
		t.Fatalf("favorite row=%#v", got)
	}
	if got.LastPlayed != 0 {
		t.Fatalf("favorite-only LastPlayed=%d want 0", got.LastPlayed)
	}
	if got.UpdatedAt == 0 {
		t.Fatal("favorite-only UpdatedAt was not recorded")
	}
}

func TestManualPlayedEpisodeSeedsMetadataForNextUp(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "tok-a", "User": map[string]any{"Id": "user-a"}})
		case r.Method == http.MethodPost && r.URL.Path == "/Users/user-a/PlayedItems/ep-1":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/Users/user-a/Items/ep-1":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Id": "ep-1", "Type": "Episode", "Name": "Episode 1", "SeriesId": "series-a", "SeriesName": "Show",
				"ParentIndexNumber": 1, "IndexNumber": 1, "RunTimeTicks": 1000,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/Shows/series-a/Episodes":
			_ = json.NewEncoder(w).Encode(map[string]any{"Items": []any{
				map[string]any{"Id": "ep-1", "Type": "Episode", "Name": "Episode 1", "SeriesId": "series-a", "SeriesName": "Show", "ParentIndexNumber": 1, "IndexNumber": 1, "UserData": map[string]any{"ItemId": "ep-1"}},
				map[string]any{"Id": "ep-2", "Type": "Episode", "Name": "Episode 2", "SeriesId": "series-a", "SeriesName": "Show", "ParentIndexNumber": 1, "IndexNumber": 2, "UserData": map[string]any{"ItemId": "ep-2", "Played": true}},
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	withTempAppConfig(t, singleUpstreamConfig(upstream.URL), func(app *App, handler http.Handler) {
		userToken := createRegularUser(t, handler)
		serverID := app.Upstream.Clients()[0].ID
		ep1Virtual := app.IDStore.GetOrCreateVirtualID("ep-1", serverID)
		rr := doAuthJSON(t, handler, http.MethodPost, "/Users/"+app.Auth.ProxyUserID()+"/PlayedItems/"+ep1Virtual, nil, userToken)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("mark episode played status=%d body=%s", rr.Code, rr.Body.String())
		}
		progress := app.WatchStore.GetProgress(watchUserID(t, app, "child"), ep1Virtual)
		if progress == nil || progress.ItemType != "Episode" || progress.SeriesVirtualID == "" || progress.IndexNumber != 1 {
			t.Fatalf("manual played episode metadata not seeded: %#v", progress)
		}
		rr = doJSONRequest(t, handler, http.MethodGet, "/Shows/NextUp?SeriesId="+progress.SeriesVirtualID, nil, userToken)
		if rr.Code != http.StatusOK {
			t.Fatalf("NextUp status=%d body=%s", rr.Code, rr.Body.String())
		}
		var payload map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		items, _ := payload["Items"].([]any)
		if len(items) != 1 {
			t.Fatalf("NextUp items=%#v", items)
		}
		next, _ := items[0].(map[string]any)
		if idx, _ := numericInt(next["IndexNumber"]); idx != 2 {
			t.Fatalf("NextUp index=%v want 2", next["IndexNumber"])
		}
		ud, _ := next["UserData"].(map[string]any)
		if ud["Played"] != false {
			t.Fatalf("NextUp leaked shared episode Played state: %#v", ud)
		}
	})
}

func TestUpstreamDeletePreservesWatchStateForPromotedInstance(t *testing.T) {
	newUpstream := func(userID string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName" {
				_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "tok-" + userID, "User": map[string]any{"Id": userID}})
				return
			}
			http.NotFound(w, r)
		}))
	}
	a := newUpstream("user-a")
	defer a.Close()
	b := newUpstream("user-b")
	defer b.Close()

	withTempAppConfig(t, dualUpstreamConfig(a.URL, b.URL), func(app *App, handler http.Handler) {
		adminToken := loginTokenAs(t, handler, "admin", "secret")
		userToken := createRegularUser(t, handler)
		_ = userToken
		clients := app.Upstream.Clients()
		if len(clients) != 2 {
			t.Fatalf("clients=%d want 2", len(clients))
		}
		primaryID, secondaryID := clients[0].ID, clients[1].ID
		virtualKeep := app.IDStore.GetOrCreateVirtualID("movie-a", primaryID)
		app.IDStore.AssociateAdditionalInstance(virtualKeep, "movie-b", secondaryID)
		virtualDrop := app.IDStore.GetOrCreateVirtualID("orphan-a", primaryID)
		userID := watchUserID(t, app, "child")
		for _, item := range []string{virtualKeep, virtualDrop} {
			if err := app.WatchStore.RecordProgress(&WatchProgress{ProxyUserID: userID, VirtualItemID: item, ServerID: primaryID, OriginalItemID: "orig", PositionTicks: 100, RuntimeTicks: 1000}); err != nil {
				t.Fatal(err)
			}
		}

		rr := doJSONRequest(t, handler, http.MethodDelete, "/admin/api/upstream/"+primaryID, nil, adminToken)
		if rr.Code != http.StatusOK {
			t.Fatalf("delete upstream status=%d body=%s", rr.Code, rr.Body.String())
		}
		resolved := app.IDStore.ResolveVirtualID(virtualKeep)
		if resolved == nil || resolved.ServerID != secondaryID || resolved.OriginalID != "movie-b" {
			t.Fatalf("surviving virtual was not promoted: %#v", resolved)
		}
		if got := app.WatchStore.GetProgress(userID, virtualKeep); got == nil || got.PositionTicks != 100 {
			t.Fatalf("promoted virtual lost watch state: %#v", got)
		}
		if app.IDStore.ResolveVirtualID(virtualDrop) != nil {
			t.Fatal("orphan virtual mapping survived deleted last instance")
		}
		if got := app.WatchStore.GetProgress(userID, virtualDrop); got != nil {
			t.Fatalf("orphan virtual watch state survived: %#v", got)
		}
	})
}

func TestMarkUnplayedPreservesHistoryButClearsResumePosition(t *testing.T) {
	ws := newTestWatchStore(t)
	playedAt := time.Date(2026, 9, 20, 1, 2, 3, 0, time.UTC).UnixMilli()
	if err := ws.RecordProgress(&WatchProgress{ProxyUserID: "user1", VirtualItemID: "item1", PositionTicks: 400, RuntimeTicks: 1000, LastPlayed: playedAt}); err != nil {
		t.Fatal(err)
	}
	if err := ws.MarkPlayedAt("user1", "item1", false, 0); err != nil {
		t.Fatal(err)
	}
	got := ws.GetProgress("user1", "item1")
	if got == nil || got.PositionTicks != 0 || got.Played || got.LastPlayed != playedAt {
		t.Fatalf("unplayed transition=%#v, want pos=0 played=false LastPlayed preserved", got)
	}
}

func TestUserDataUnplayedWithPositionKeepsResumeProgress(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "tok-a", "User": map[string]any{"Id": "user-a"}})
		case r.Method == http.MethodPost && r.URL.Path == "/Users/user-a/Items/movie-a/UserData":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/Users/user-a/Items/movie-a":
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": "movie-a", "Type": "Movie", "Name": "Movie", "RunTimeTicks": 1000})
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	withTempAppConfig(t, singleUpstreamConfig(upstream.URL), func(app *App, handler http.Handler) {
		userToken := createRegularUser(t, handler)
		serverID := app.Upstream.Clients()[0].ID
		virtualID := app.IDStore.GetOrCreateVirtualID("movie-a", serverID)
		rr := doAuthJSON(t, handler, http.MethodPost, "/Users/"+app.Auth.ProxyUserID()+"/Items/"+virtualID+"/UserData", map[string]any{
			"Played": false, "PlaybackPositionTicks": 350, "RunTimeTicks": 1000,
		}, userToken)
		if rr.Code != http.StatusNoContent {
			t.Fatalf("UserData status=%d body=%s", rr.Code, rr.Body.String())
		}
		progress := app.WatchStore.GetProgress(watchUserID(t, app, "child"), virtualID)
		if progress == nil || progress.Played || progress.PositionTicks != 350 || progress.RuntimeTicks != 1000 {
			t.Fatalf("combined unplayed/progress state=%#v, want Played=false PositionTicks=350 RuntimeTicks=1000", progress)
		}
	})
}

func TestParseLocalPlayedAtFormats(t *testing.T) {
	want := time.Date(2026, 9, 25, 8, 32, 5, 0, time.UTC).UnixMilli()
	for _, input := range []string{"2026-09-25T08:32:05Z", "20260925083205"} {
		if got := parseLocalPlayedAt(input); got != want {
			t.Fatalf("parseLocalPlayedAt(%q)=%d want %d", input, got, want)
		}
	}
}

func firstResponseUserData(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("decode response: %v body=%s", err, raw)
	}
	items, _ := payload["Items"].([]any)
	if len(items) == 0 {
		t.Fatalf("response has no items: %s", raw)
	}
	item, _ := items[0].(map[string]any)
	ud, _ := item["UserData"].(map[string]any)
	if ud == nil {
		t.Fatalf("first item has no UserData: %#v", item)
	}
	return ud
}
