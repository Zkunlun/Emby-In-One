package backend

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// createTestUser creates a regular user through the admin API and returns its
// generated local user ID.
func createTestUser(t *testing.T, handler http.Handler, adminToken, username, password string) string {
	t.Helper()
	rr := doAuthJSON(t, handler, http.MethodPost, "/admin/api/users",
		map[string]any{
			"username":       username,
			"password":       password,
			"allowedServers": testAllUpstreamIDs(t, handler, adminToken),
		}, adminToken)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create user %q: status=%d body=%s", username, rr.Code, rr.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal created user: %v", err)
	}
	userID, _ := created["id"].(string)
	if userID == "" {
		t.Fatalf("created user %q has no id", username)
	}
	return userID
}

// responseIdentityUpstream is an upstream whose single item is served through
// /Users/{realUserID}/Items. It records the user ID it was asked for.
func responseIdentityUpstream(t *testing.T, realUserID string, seen *[]string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"AccessToken": "token-" + realUserID,
				"User":        map[string]any{"Id": realUserID},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/Users/"+realUserID+"/Items":
			*seen = append(*seen, realUserID)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Items": []map[string]any{{
					"Id":     "orig-item",
					"Name":   "Movie",
					"Type":   "Movie",
					"UserId": realUserID,
					"UserData": map[string]any{
						"UserId": realUserID,
						"Played": false,
					},
				}},
				"TotalRecordCount": 1,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// TestResponseIdentityPerUser is the second batch's contract: a regular user's
// current-user responses carry that user's own ID, the admin's carry the admin's,
// the same upstream item keeps one virtual ID across users, and a request that
// still holds the legacy global user ID keeps working.
func TestResponseIdentityPerUser(t *testing.T) {
	var seen []string
	upstream := responseIdentityUpstream(t, "user-a", &seen)

	withTempAppConfig(t, singleUpstreamConfig(upstream.URL), func(app *App, handler http.Handler) {
		adminToken := loginTokenAs(t, handler, "admin", "secret")
		aliceID := createTestUser(t, handler, adminToken, "alice", "alice123")
		bobID := createTestUser(t, handler, adminToken, "bob", "bob12345")
		aliceToken := loginTokenAs(t, handler, "alice", "alice123")
		bobToken := loginTokenAs(t, handler, "bob", "bob12345")

		legacyProxyUser := app.Auth.ProxyUserID()
		if aliceID == legacyProxyUser || bobID == legacyProxyUser {
			t.Fatalf("fixture collision: a local user ID equals the legacy proxy user ID")
		}

		virtualParent := app.IDStore.GetOrCreateVirtualID("orig-parent", app.Upstream.Clients()[0].ID)
		itemUserID := func(token string) (string, string) {
			rr := doJSONRequest(t, handler, http.MethodGet, "/Users/"+legacyProxyUser+"/Items?ParentId="+virtualParent, nil, token)
			if rr.Code != http.StatusOK {
				t.Fatalf("items status = %d, body=%s", rr.Code, rr.Body.String())
			}
			var payload map[string]any
			if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
				t.Fatalf("unmarshal items: %v", err)
			}
			items, _ := payload["Items"].([]any)
			if len(items) != 1 {
				t.Fatalf("items = %d, want 1", len(items))
			}
			item, _ := items[0].(map[string]any)
			id, _ := item["Id"].(string)
			userID, _ := item["UserId"].(string)
			return id, userID
		}

		aliceItemID, aliceUserID := itemUserID(aliceToken)
		if aliceUserID != aliceID {
			t.Fatalf("alice's response UserId = %q, want her own ID %q", aliceUserID, aliceID)
		}
		bobItemID, bobUserID := itemUserID(bobToken)
		if bobUserID != bobID {
			t.Fatalf("bob's response UserId = %q, want his own ID %q", bobUserID, bobID)
		}
		adminItemID, adminUserID := itemUserID(adminToken)
		if adminUserID != legacyProxyUser {
			t.Fatalf("admin's response UserId = %q, want %q", adminUserID, legacyProxyUser)
		}

		// One upstream item must not become a different virtual ID per user: the
		// mapping is per resource, not per viewer.
		if aliceItemID == "" || aliceItemID != bobItemID || aliceItemID != adminItemID {
			t.Fatalf("virtual IDs differ per user: alice=%q bob=%q admin=%q", aliceItemID, bobItemID, adminItemID)
		}
	})
}

// TestResponseIdentityLegacyUserIDCompat covers a client that still sends the
// global user ID older responses handed out. Switching the response identity must
// not require the client to clear its cache.
func TestResponseIdentityLegacyUserIDCompat(t *testing.T) {
	var seen []string
	upstream := responseIdentityUpstream(t, "user-a", &seen)

	withTempAppConfig(t, singleUpstreamConfig(upstream.URL), func(app *App, handler http.Handler) {
		adminToken := loginTokenAs(t, handler, "admin", "secret")
		aliceID := createTestUser(t, handler, adminToken, "alice", "alice123")
		aliceToken := loginTokenAs(t, handler, "alice", "alice123")
		legacyProxyUser := app.Auth.ProxyUserID()

		virtualParent := app.IDStore.GetOrCreateVirtualID("orig-parent", app.Upstream.Clients()[0].ID)
		// The client asks for the legacy global user's items with its own token.
		rr := doJSONRequest(t, handler, http.MethodGet, "/Users/"+legacyProxyUser+"/Items?ParentId="+virtualParent+"&UserId="+legacyProxyUser, nil, aliceToken)
		if rr.Code != http.StatusOK {
			t.Fatalf("legacy-cached request status = %d, body=%s", rr.Code, rr.Body.String())
		}
		var payload map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		items, _ := payload["Items"].([]any)
		if len(items) != 1 {
			t.Fatalf("items = %d, want 1", len(items))
		}
		item, _ := items[0].(map[string]any)
		if got, _ := item["UserId"].(string); got != aliceID {
			t.Fatalf("response UserId = %q, want alice's own ID %q", got, aliceID)
		}
		// The upstream was still addressed with its own real user ID.
		if len(seen) == 0 {
			t.Fatalf("the upstream never received a request")
		}
		for _, userID := range seen {
			if userID != "user-a" {
				t.Fatalf("the upstream was addressed as %q", userID)
			}
		}
	})
}

// A fallback (unclassified) response for a regular user must report that user's
// own identity, while an unknown write body keeps its own values.
func TestFallbackCurrentUserIdentity(t *testing.T) {
	var bodyUserID string
	var queryUserID string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "token-a", "User": map[string]any{"Id": "user-a"}})
		case r.Method == http.MethodGet && r.URL.Path == "/Users/user-a/CustomEndpoint/orig-item":
			queryUserID = r.URL.Query().Get("UserId")
			_ = json.NewEncoder(w).Encode(map[string]any{"ItemId": "orig-item", "UserId": "user-a"})
		case r.Method == http.MethodPost && r.URL.Path == "/Users/user-a/UnknownWrite":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			bodyUserID, _ = body["UserId"].(string)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	withTempAppConfig(t, singleUpstreamConfig(upstream.URL), func(app *App, handler http.Handler) {
		adminToken := loginTokenAs(t, handler, "admin", "secret")
		aliceID := createTestUser(t, handler, adminToken, "alice", "alice123")
		aliceToken := loginTokenAs(t, handler, "alice", "alice123")
		legacyProxyUser := app.Auth.ProxyUserID()

		virtualItem := app.IDStore.GetOrCreateVirtualID("orig-item", app.Upstream.Clients()[0].ID)

		// A regular user's unclassified read is normalized to the target upstream.
		rr := doJSONRequest(t, handler, http.MethodGet,
			"/Users/"+legacyProxyUser+"/CustomEndpoint/"+virtualItem+"?UserId="+legacyProxyUser, nil, aliceToken)
		if rr.Code != http.StatusOK {
			t.Fatalf("fallback read status = %d, body=%s", rr.Code, rr.Body.String())
		}
		if queryUserID != "user-a" {
			t.Fatalf("upstream query UserId = %q, want user-a", queryUserID)
		}
		var payload map[string]any
		_ = json.Unmarshal(rr.Body.Bytes(), &payload)
		if got, _ := payload["UserId"].(string); got != aliceID {
			t.Fatalf("fallback response UserId = %q, want %q", got, aliceID)
		}

		// An unknown write body keeps the values the client sent, including one that
		// matches a registered local user.
		rr = doJSONRequest(t, handler, http.MethodPost,
			"/Users/"+legacyProxyUser+"/UnknownWrite",
			map[string]any{"UserId": aliceID, "TargetUserId": "someone-else"}, aliceToken)
		if rr.Code >= http.StatusInternalServerError {
			t.Fatalf("unknown write status = %d, body=%s", rr.Code, rr.Body.String())
		}
		if bodyUserID != aliceID {
			t.Fatalf("unknown write body UserId = %q, want the client's own value %q", bodyUserID, aliceID)
		}
	})
}

// Resume and NextUp must materialize local history and rewrite upstream identity.
func TestResponseIdentityResumeAndNextUp(t *testing.T) {
	var resumeHits, nextUpHits, parentHits atomic.Int32
	episode := func(id string, number int) map[string]any {
		return map[string]any{
			"Id": id, "UserId": "user-a", "Type": "Episode", "Name": id,
			"SeriesId": "series-a", "SeriesName": "Fixture Series",
			"ParentIndexNumber": 1, "IndexNumber": number, "RunTimeTicks": 1000,
			"MediaSources": []any{map[string]any{"Id": "source-" + id, "RunTimeTicks": 1000}},
			"UserData": map[string]any{
				"PlaybackPositionTicks": 777, "Played": true, "IsFavorite": true,
			},
		}
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "token-a", "User": map[string]any{"Id": "user-a"}})
		case r.Method == http.MethodGet && r.URL.Path == "/Items":
			if r.URL.Query().Get("Ids") == "series-a" {
				parentHits.Add(1)
				_ = json.NewEncoder(w).Encode(map[string]any{"Items": []any{map[string]any{"Id": "series-a", "Type": "Series", "Name": "Fixture Series", "ProductionYear": 2024}}})
				return
			}
			resumeHits.Add(1)
			if got := r.URL.Query().Get("Ids"); got != "orig-item" {
				t.Errorf("resume metadata Ids = %q, want orig-item", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"Items": []any{episode("orig-item", 1)}})
		case r.Method == http.MethodGet && r.URL.Path == "/Shows/series-a/Episodes":
			nextUpHits.Add(1)
			if got := r.URL.Query().Get("UserId"); got != "user-a" {
				t.Errorf("NextUp upstream UserId = %q, want user-a", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"Items": []any{episode("orig-item", 1), episode("next-item", 2)}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"Items": []any{}})
		}
	}))
	defer upstream.Close()

	withTempAppConfig(t, singleUpstreamConfig(upstream.URL), func(app *App, handler http.Handler) {
		adminToken := loginTokenAs(t, handler, "admin", "secret")
		aliceID := createTestUser(t, handler, adminToken, "alice", "alice123")
		aliceToken := loginTokenAs(t, handler, "alice", "alice123")
		bobID := createTestUser(t, handler, adminToken, "bob", "bob12345")
		bobToken := loginTokenAs(t, handler, "bob", "bob12345")
		if app.WatchStore == nil {
			t.Fatal("local WatchStore is required")
		}
		clients := app.Upstream.Clients()
		if len(clients) != 1 {
			t.Fatalf("upstream clients = %d, want 1", len(clients))
		}
		serverID := clients[0].ID
		virtualItem := app.IDStore.GetOrCreateVirtualID("orig-item", serverID)
		virtualSeries := app.IDStore.GetOrCreateVirtualID("series-a", serverID)
		progress := &WatchProgress{
			ProxyUserID: aliceID, VirtualItemID: virtualItem, ServerID: serverID,
			OriginalItemID: "orig-item", ItemType: "Episode",
			SeriesVirtualID: virtualSeries, SeriesOriginalID: "series-a", SeriesName: "Fixture Series",
			ParentIndexNumber: 1, IndexNumber: 1, PositionTicks: 100, RuntimeTicks: 1000,
		}
		if err := app.WatchStore.RecordProgress(progress); err != nil {
			t.Fatalf("record progress: %v", err)
		}
		readItems := func(t *testing.T, path, token string) []any {
			t.Helper()
			rr := doJSONRequest(t, handler, http.MethodGet, path, nil, token)
			if rr.Code != http.StatusOK {
				t.Fatalf("GET %s status = %d, body=%s", path, rr.Code, rr.Body.String())
			}
			var payload map[string]any
			if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
				t.Fatalf("unmarshal %s: %v", path, err)
			}
			items, ok := payload["Items"].([]any)
			if !ok {
				t.Fatalf("GET %s Items is not an array: %#v", path, payload)
			}
			if got, ok := payload["TotalRecordCount"].(float64); !ok || int(got) != len(items) {
				t.Fatalf("GET %s TotalRecordCount = %#v, want %d", path, payload["TotalRecordCount"], len(items))
			}
			return items
		}
		assertItem := func(t *testing.T, items []any, id string, position float64) {
			t.Helper()
			if len(items) != 1 {
				t.Fatalf("items = %d, want exactly 1", len(items))
			}
			item, ok := items[0].(map[string]any)
			if !ok || item["Id"] != id || item["UserId"] != aliceID {
				t.Fatalf("item = %#v, want Id=%q UserId=%q", items[0], id, aliceID)
			}
			ud, ok := item["UserData"].(map[string]any)
			if !ok || ud["PlaybackPositionTicks"] != position || ud["Played"] != false || ud["IsFavorite"] != false {
				t.Fatalf("local UserData = %#v, want position=%v played=false favorite=false", item["UserData"], position)
			}
		}
		t.Run("Resume", func(t *testing.T) {
			assertItem(t, readItems(t, "/Users/"+aliceID+"/Items/Resume", aliceToken), virtualItem, 100)
			if items := readItems(t, "/Users/"+bobID+"/Items/Resume", bobToken); len(items) != 0 {
				t.Fatalf("Alice's Resume leaked to Bob: %#v", items)
			}
		})
		progress.Played = true
		progress.PositionTicks = 1000
		if err := app.WatchStore.RecordProgress(progress); err != nil {
			t.Fatalf("record played episode: %v", err)
		}
		t.Run("NextUp", func(t *testing.T) {
			nextID := app.IDStore.GetOrCreateVirtualID("next-item", serverID)
			assertItem(t, readItems(t, "/Shows/NextUp?UserId="+aliceID+"&SeriesId="+virtualSeries, aliceToken), nextID, 0)
			if items := readItems(t, "/Shows/NextUp?UserId="+bobID+"&SeriesId="+virtualSeries, bobToken); len(items) != 0 {
				t.Fatalf("Alice's NextUp leaked to Bob: %#v", items)
			}
		})
		if parentHits.Load() != 1 {
			t.Fatalf("encountered parent metadata hits = %d, want 1", parentHits.Load())
		}
		if resumeHits.Load() != 1 || nextUpHits.Load() != 1 {
			t.Fatalf("metadata hits: Resume=%d NextUp=%d, want 1 each", resumeHits.Load(), nextUpHits.Load())
		}
	})
}
