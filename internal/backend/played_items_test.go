package backend

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
)

func requirePlayedItemsJSON(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("Content-Type") == "application/json" && r.ContentLength == 0 {
		return true
	}
	w.WriteHeader(http.StatusUnsupportedMediaType)
	return false
}

func TestPlayedItemsHillsEmptyJSONBodyWatchedAndUnwatched(t *testing.T) {
	var datePlayed atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "tok-a", "User": map[string]any{"Id": "user-a"}})
		case r.Method == http.MethodPost && r.URL.Path == "/Users/user-a/PlayedItems/item-a":
			if !requirePlayedItemsJSON(w, r) {
				return
			}
			if r.URL.Query().Get("X-Emby-Token") != "" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			datePlayed.Store(r.URL.Query().Get("DatePlayed"))
			_ = json.NewEncoder(w).Encode(map[string]any{"ItemId": "item-a", "UserId": "user-a", "Played": true})
		case r.Method == http.MethodDelete && r.URL.Path == "/Users/user-a/PlayedItems/item-a":
			if !requirePlayedItemsJSON(w, r) {
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ItemId": "item-a", "UserId": "user-a", "Played": false})
		case r.Method == http.MethodPost && r.URL.Path == "/Users/user-a/PlayedItems/item-a/Delete":
			if !requirePlayedItemsJSON(w, r) {
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"ItemId": "item-a", "UserId": "user-a", "Played": false})
		case r.URL.Path != "" && r.URL.Path != "/":
			w.WriteHeader(http.StatusUnsupportedMediaType)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	withTempAppConfig(t, singleUpstreamConfig(upstream.URL), func(app *App, handler http.Handler) {
		token := createRegularUser(t, handler)
		virtualItem := app.IDStore.GetOrCreateVirtualID("item-a", app.Upstream.Clients()[0].ID)
		watchUser := watchUserID(t, app, "child")
		basePath := "/Users/" + app.Auth.ProxyUserID() + "/PlayedItems/" + virtualItem

		// Hills Android was observed sending application/json with Content-Length 0.
		// Before the dedicated route this request fell through the generic fallback and
		// production returned 415. Preserve the wire shape while translating IDs/auth;
		// a non-credential query value must survive while the local token query is sanitized.
		const playedAt = "2026-09-25T08:32:05Z"
		firstPath := basePath + "?DatePlayed=" + url.QueryEscape(playedAt) + "&X-Emby-Token=" + url.QueryEscape(token)
		rr := doAuthJSON(t, handler, http.MethodPost, firstPath, nil, token)
		if rr.Code != http.StatusOK {
			t.Fatalf("POST PlayedItems status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
		}
		if got, _ := datePlayed.Load().(string); got != playedAt {
			t.Fatalf("DatePlayed forwarded as %q, want %q", got, playedAt)
		}
		var payload map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode POST PlayedItems response: %v", err)
		}
		if payload["ItemId"] == "item-a" || payload["UserId"] == "user-a" {
			t.Fatalf("POST PlayedItems response ids were not rewritten: %#v", payload)
		}
		if played, _ := payload["Played"].(bool); !played {
			t.Fatalf("POST PlayedItems response Played = %#v, want true", payload["Played"])
		}
		progress := app.WatchStore.GetProgress(watchUser, virtualItem)
		if progress == nil || !progress.Played {
			t.Fatalf("local watched state after POST = %#v, want Played=true", progress)
		}
		if want := parseLocalPlayedAt(playedAt); progress.LastPlayed != want {
			t.Fatalf("local LastPlayed = %d, want DatePlayed %d", progress.LastPlayed, want)
		}

		rr = doAuthJSON(t, handler, http.MethodDelete, basePath, nil, token)
		if rr.Code != http.StatusOK {
			t.Fatalf("DELETE PlayedItems status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
		}
		progress = app.WatchStore.GetProgress(watchUser, virtualItem)
		if progress == nil || progress.Played {
			t.Fatalf("local watched state after DELETE = %#v, want Played=false", progress)
		}

		rr = doAuthJSON(t, handler, http.MethodPost, basePath, nil, token)
		if rr.Code != http.StatusOK {
			t.Fatalf("second POST PlayedItems status = %d, want 200", rr.Code)
		}
		rr = doAuthJSON(t, handler, http.MethodPost, basePath+"/Delete", nil, token)
		if rr.Code != http.StatusOK {
			t.Fatalf("POST PlayedItems/Delete status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
		}
		progress = app.WatchStore.GetProgress(watchUser, virtualItem)
		if progress == nil || progress.Played {
			t.Fatalf("local watched state after POST Delete = %#v, want Played=false", progress)
		}
	})
}

func TestPlayedItemsFailedForwardDoesNotWriteLocalState(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName" {
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "tok-a", "User": map[string]any{"Id": "user-a"}})
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/Users/user-a/PlayedItems/item-a" {
			if !requirePlayedItemsJSON(w, r) {
				return
			}
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusUnsupportedMediaType)
	}))
	defer upstream.Close()

	withTempAppConfig(t, singleUpstreamConfig(upstream.URL), func(app *App, handler http.Handler) {
		token := createRegularUser(t, handler)
		virtualItem := app.IDStore.GetOrCreateVirtualID("item-a", app.Upstream.Clients()[0].ID)
		rr := doAuthJSON(t, handler, http.MethodPost, "/Users/"+app.Auth.ProxyUserID()+"/PlayedItems/"+virtualItem, nil, token)
		if rr.Code != http.StatusInternalServerError {
			t.Fatalf("failed PlayedItems forward status = %d, want 500 (body=%s)", rr.Code, rr.Body.String())
		}
		if progress := app.WatchStore.GetProgress(watchUserID(t, app, "child"), virtualItem); progress != nil {
			t.Fatalf("local watched state was written after failed upstream forward: %#v", progress)
		}
	})
}

func TestPlayedItemsMutationUsesPrimaryInstanceOnly(t *testing.T) {
	var primaryHits atomic.Int32
	var additionalHits atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "tok-a", "User": map[string]any{"Id": "user-a"}})
		case r.Method == http.MethodPost && r.URL.Path == "/Users/user-a/PlayedItems/item-a":
			if !requirePlayedItemsJSON(w, r) {
				return
			}
			primaryHits.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"ItemId": "item-a", "Played": true})
		default:
			w.WriteHeader(http.StatusUnsupportedMediaType)
		}
	}))
	defer primary.Close()
	additional := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "tok-b", "User": map[string]any{"Id": "user-b"}})
		case r.URL.Path == "/Users/user-b/PlayedItems/item-b":
			additionalHits.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"ItemId": "item-b", "Played": true})
		default:
			http.NotFound(w, r)
		}
	}))
	defer additional.Close()

	withTempAppConfig(t, dualUpstreamConfig(primary.URL, additional.URL), func(app *App, handler http.Handler) {
		token := createRegularUser(t, handler)
		virtualItem := app.IDStore.GetOrCreateVirtualID("item-a", app.Upstream.Clients()[0].ID)
		app.IDStore.AssociateAdditionalInstance(virtualItem, "item-b", app.Upstream.Clients()[1].ID)
		rr := doAuthJSON(t, handler, http.MethodPost, "/Users/"+app.Auth.ProxyUserID()+"/PlayedItems/"+virtualItem, nil, token)
		if rr.Code != http.StatusOK {
			t.Fatalf("POST PlayedItems status = %d, want 200 (body=%s)", rr.Code, rr.Body.String())
		}
		if got := primaryHits.Load(); got != 1 {
			t.Fatalf("primary PlayedItems hits = %d, want 1", got)
		}
		if got := additionalHits.Load(); got != 0 {
			t.Fatalf("additional instance PlayedItems hits = %d, want 0", got)
		}
	})
}
