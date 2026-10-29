package backend

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// A GUID-shaped library ID and CollectionFolder DTO must not by themselves
// cause the item-detail 502 reported by VividPlayer.
func TestUserItemDetailCollectionFolderGUID(t *testing.T) {
	originalID := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "upstream-test-token", "User": map[string]any{"Id": "user-a"}})
		case r.Method == http.MethodGet && r.URL.Path == "/Users/user-a/Items/"+originalID:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"Id": originalID, "Type": "CollectionFolder", "Name": "Movies",
				"IsFolder": true, "CollectionType": "movies",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	config := fmt.Sprintf("server:\n  port: 8096\n  name: Test Server\n  id: server-1\nadmin:\n  username: admin\n  password: secret\nplayback:\n  mode: proxy\ntimeouts:\n  api: 30000\n  global: 15000\n  login: 10000\n  healthCheck: 10000\n  healthInterval: 60000\nproxies: []\nupstream:\n  - name: A\n    url: %q\n    username: u1\n    password: p1\n", upstream.URL)

	withTempAppConfig(t, config, func(app *App, handler http.Handler) {
		token := loginToken(t, handler, "secret")
		virtualID := app.IDStore.GetOrCreateVirtualID(originalID, app.Upstream.Clients()[0].ID)
		path := "/Users/" + app.Auth.ProxyUserID() + "/Items/" + virtualID
		rr := doJSONRequest(t, handler, http.MethodGet, path, nil, token)
		if rr.Code != http.StatusOK {
			t.Fatalf("CollectionFolder detail status = %d, body=%s", rr.Code, rr.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode item detail: %v", err)
		}
		if body["Id"] != virtualID || body["Type"] != "CollectionFolder" {
			t.Fatalf("unexpected folder detail: %#v", body)
		}
	})
}

func TestUserItemDetailMismatchedIDsRequireVerifiedView(t *testing.T) {
	for _, tc := range []struct {
		name, detailType, viewID, viewType string
		status                             int
		lookUp                             bool
	}{
		{"exact view", "CollectionFolder", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "CollectionFolder", 200, true},
		{"different view", "CollectionFolder", "another-item", "CollectionFolder", 502, true},
		{"wrong view type", "CollectionFolder", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "Movie", 502, true},
		{"movie is not a view", "Movie", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "CollectionFolder", 502, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
			var viewsCalls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == "POST" && r.URL.Path == "/Users/AuthenticateByName":
					_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "test-token", "User": map[string]any{"Id": "user-a"}})
				case r.Method == "GET" && r.URL.Path == "/Users/user-a/Items/"+id:
					_ = json.NewEncoder(w).Encode(map[string]any{"Id": "12", "Type": tc.detailType, "Name": "Incorrect object"})
				case r.Method == "GET" && r.URL.Path == "/Users/user-a/Views":
					viewsCalls.Add(1)
					if r.URL.Query().Get("UserId") != "user-a" {
						t.Errorf("unexpected user in Views request: %q", r.URL.Query().Get("UserId"))
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"Items": []map[string]any{
						{"Id": "12", "Type": "CollectionFolder", "Name": "Other folder"},
						{"Id": tc.viewID, "Type": tc.viewType, "Name": "Verified library", "CollectionType": "movies"},
					}})
				default:
					http.NotFound(w, r)
				}
			}))
			defer upstream.Close()
			config := fmt.Sprintf("server:\n  port: 8096\n  name: Test Server\n  id: server-1\nadmin:\n  username: admin\n  password: secret\nplayback:\n  mode: proxy\ntimeouts:\n  api: 30000\n  global: 15000\n  login: 10000\n  healthCheck: 10000\n  healthInterval: 60000\nproxies: []\nupstream:\n  - name: A\n    url: %q\n    username: u1\n    password: p1\n", upstream.URL)
			withTempAppConfig(t, config, func(app *App, handler http.Handler) {
				token := loginToken(t, handler, "secret")
				virtual := app.IDStore.GetOrCreateVirtualID(id, app.Upstream.Clients()[0].ID)
				rr := doJSONRequest(t, handler, http.MethodGet, "/Users/"+app.Auth.ProxyUserID()+"/Items/"+virtual, nil, token)
				if rr.Code != tc.status {
					t.Fatalf("status=%d, want=%d, response=%s", rr.Code, tc.status, rr.Body.String())
				}
				if (viewsCalls.Load() > 0) != tc.lookUp {
					t.Fatalf("Views lookup=%d, want=%v", viewsCalls.Load(), tc.lookUp)
				}
				if tc.status == 200 {
					var obj map[string]any
					if err := json.Unmarshal(rr.Body.Bytes(), &obj); err != nil {
						t.Fatal(err)
					}
					if obj["Id"] != virtual || obj["Name"] != "Verified library" || obj["CollectionType"] != "movies" {
						t.Fatalf("unverified library detail: %#v", obj)
					}
				}
			})
		})
	}
}
