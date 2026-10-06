package backend

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type task6Phase4Gate struct {
	entered     chan struct{}
	release     chan struct{}
	once        sync.Once
	enteredOnce sync.Once
}

func newTask6Phase4Gate() *task6Phase4Gate {
	return &task6Phase4Gate{entered: make(chan struct{}), release: make(chan struct{})}
}
func (g *task6Phase4Gate) unblock() { g.once.Do(func() { close(g.release) }) }
func task6Phase4Wait(t *testing.T, done <-chan *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	select {
	case rr := <-done:
		return rr
	case <-time.After(5 * time.Second):
		t.Fatal("bounded request did not complete")
		return nil
	}
}

func TestTask6Phase4BatchVersionSelection(t *testing.T) {
	a := task6HTTPMovie("movie-a", task6HTTPSource("main", 1000), task6HTTPSource("cut", 2000))
	b := task6HTTPMovie("movie-b", task6HTTPSource("b100", 1000))
	withTask6HTTPFixture(t, []map[string]any{a}, []map[string]any{b}, func(f *task6HTTPFixture) {
		f.list(t, "Movie")
		shared := task6HTTPGroup(t, f, "server-a", "movie-a", "main")
		cut := task6HTTPGroup(t, f, "server-a", "movie-a", "cut")
		for _, key := range []string{"Ids", "ids", "ItemIds"} {
			t.Run(key, func(t *testing.T) {
				rows := asItems(task6HTTPJSON(t, f.request(t, http.MethodGet, "/Items?"+key+"="+cut, nil, "")))
				if len(rows) != 1 || rows[0]["Id"] != cut {
					t.Fatalf("requested cut returned other identities: %+v", rows)
				}
				sources := asItems(rows[0]["MediaSources"])
				if len(sources) != 3 {
					t.Fatalf("cut sources %+v", sources)
				}
			})
		}
		t.Run("mixed_requested_versions", func(t *testing.T) {
			rows := asItems(task6HTTPJSON(t, f.request(t, http.MethodGet, "/Items?Ids="+shared+","+cut, nil, "")))
			if len(rows) != 1 {
				t.Fatalf("requested versions %+v", rows)
			}
		})
	})
}

func TestTask6Phase4ParentRawPageBoundary(t *testing.T) {
	a := task6HTTPMovie("first", task6HTTPSource("first-main", 1000), task6HTTPSource("first-cut", 2000))
	a["ParentId"] = "library-a"
	next := task6HTTPMovie("second", task6HTTPSource("second-main", 3000), task6HTTPSource("second-cut", 4000))
	next["ParentId"] = "library-a"
	next["ProviderIds"] = map[string]any{"Tmdb": "2"}
	b := task6HTTPMovie("movie-b", task6HTTPSource("b100", 1000))
	withTask6HTTPFixture(t, []map[string]any{a, next}, []map[string]any{b}, func(f *task6HTTPFixture) {
		all := f.list(t, "Movie")
		if len(all) != 2 {
			t.Fatalf("global versions %+v", all)
		}
		parent := f.app.IDStore.GetOrCreateVirtualID("library-a", "server-a")
		seen := map[string]bool{}
		for start := 0; start < 2; start++ {
			path := "/Users/" + f.user + "/Items?ParentId=" + parent + "&StartIndex=" + strconv.Itoa(start) + "&Limit=1"
			page := task6HTTPJSON(t, f.request(t, http.MethodGet, path, nil, ""))
			rows := asItems(page)
			// Contract retains raw upstream paging, so the total/offset count items,
			// while each raw item retains all its versions in a single tile.
			if page["TotalRecordCount"] != float64(2) || page["StartIndex"] != float64(start) || len(rows) != 1 {
				t.Fatalf("raw page %d %+v", start, page)
			}
			for _, row := range rows {
				id, _ := row["Id"].(string)
				if id == "" || seen[id] {
					t.Fatalf("duplicate/missing version %+v", row)
				}
				seen[id] = true
				if len(asItems(row["MediaSources"])) != 2 {
					t.Fatalf("parent mixed cuts %+v", row)
				}
			}
		}
		if len(seen) != 2 {
			t.Fatalf("encountered versions %d", len(seen))
		}
		f.upstream[0].mu.Lock()
		defer f.upstream[0].mu.Unlock()
		n := 0
		for _, call := range f.upstream[0].calls {
			if call.Query.Get("ParentId") == "library-a" && strings.HasSuffix(call.Path, "/Items") {
				n++
				if call.Query.Get("Limit") != "1" {
					t.Fatalf("raw paging widened %+v", call.Query)
				}
			}
		}
		if n != 2 {
			t.Fatalf("parent page requests %d", n)
		}
	})
}

func withTask6Phase4Alias(t *testing.T, fn func(*task6HTTPFixture, string, string)) {
	t.Helper()
	a := task6HTTPMovie("movie-a", task6HTTPSource("a100", 1000))
	b := task6HTTPMovie("movie-b", task6HTTPSource("b100", nil))
	b["RunTimeTicks"] = nil
	withTask6HTTPFixture(t, []map[string]any{a}, []map[string]any{b}, func(f *task6HTTPFixture) {
		task6SeedSeparateItems(t, f, a, b)
		alias := task6HTTPGroup(t, f, "server-b", "movie-b", "b100")
		if err := f.app.WatchStore.RecordProgress(&WatchProgress{ProxyUserID: f.user, VirtualItemID: alias, ServerID: "server-b", OriginalItemID: "movie-b", ItemType: "Movie", PositionTicks: 200, RuntimeTicks: 1000, IsFavorite: true, LastPlayed: 100, UpdatedAt: 200}); err != nil {
			t.Fatal(err)
		}
		b["MediaSources"] = []any{task6HTTPSource("b100", 1000)}
		f.upstream[1].update(b)
		f.list(t, "Movie")
		canonical := f.app.IDStore.CanonicalMergeID(alias)
		if canonical == alias {
			t.Fatal("expected alias")
		}
		fn(f, canonical, alias)
	})
}

func TestTask6Phase4AliasBatch(t *testing.T) {
	withTask6Phase4Alias(t, func(f *task6HTTPFixture, canonical, alias string) {
		rows := asItems(task6HTTPJSON(t, f.request(t, http.MethodGet, "/Items?Ids="+alias, nil, "")))
		if len(rows) != 1 || rows[0]["Id"] != canonical {
			t.Fatalf("alias batch %+v", rows)
		}
		ud, _ := rows[0]["UserData"].(map[string]any)
		if ud["PlaybackPositionTicks"] != float64(200) || ud["IsFavorite"] != true {
			t.Fatalf("alias batch state %+v", ud)
		}
	})
}

func TestTask6Phase4AliasLifecycle(t *testing.T) {
	for _, operation := range []string{"unbind", "delete_user", "delete_server_recovery"} {
		t.Run(operation, func(t *testing.T) {
			withTask6Phase4Alias(t, func(f *task6HTTPFixture, canonical, alias string) {
				media, err := f.app.IDStore.GetMergeMediaSourceID("server-b", "movie-b", "b100")
				if err != nil {
					t.Fatal(err)
				}
				playback := task6HTTPJSON(t, f.request(t, http.MethodGet, "/Items/"+alias+"/PlaybackInfo?MediaSourceId="+media, nil, ""))
				session, _ := playback["PlaySessionId"].(string)
				body := map[string]any{"ItemId": alias, "MediaSourceId": media, "PlaySessionId": session, "PositionTicks": 100, "RunTimeTicks": 1000}
				if rr := f.request(t, http.MethodPost, "/Sessions/Playing", body, ""); rr.Code != 204 {
					t.Fatalf("start %d %s", rr.Code, rr.Body.String())
				}
				body["PositionTicks"] = 400
				if rr := f.request(t, http.MethodPost, "/Sessions/Playing/Progress", body, ""); rr.Code != 204 {
					t.Fatalf("progress %d %s", rr.Code, rr.Body.String())
				}
				before := *f.app.WatchStore.GetProgress(f.user, canonical)
				gate := newTask6Phase4Gate()
				defer gate.unblock()
				f.upstream[1].mu.Lock()
				f.upstream[1].progressGate = gate
				f.upstream[1].mu.Unlock()
				body["PositionTicks"] = 800
				done := make(chan *httptest.ResponseRecorder, 1)
				go func() { done <- f.request(t, http.MethodPost, "/Sessions/Playing/Progress", body, "") }()
				select {
				case <-gate.entered:
				case <-time.After(5 * time.Second):
					t.Fatal("progress did not reach upstream")
				}
				admin := loginTokenAs(t, f.handler, "admin", "secret")
				var bobID string
				switch operation {
				case "unbind":
					rr := doAuthJSON(t, f.handler, http.MethodPut, "/admin/api/users/"+f.user, map[string]any{"allowedServers": []string{"server-a"}}, admin)
					if rr.Code != 200 {
						t.Fatalf("unbind %d %s", rr.Code, rr.Body.String())
					}
				case "delete_user":
					rr := doAuthJSON(t, f.handler, http.MethodPost, "/admin/api/users", map[string]any{"username": "bob", "password": "password123", "allowedServers": []string{"server-a", "server-b"}}, admin)
					if rr.Code != 201 {
						t.Fatalf("bob %d %s", rr.Code, rr.Body.String())
					}
					bob := loginTokenAs(t, f.handler, "bob", "password123")
					bobID = f.app.Auth.ValidateToken(bob).UserID
					if err := f.app.WatchStore.RecordProgress(&WatchProgress{ProxyUserID: bobID, VirtualItemID: canonical, ServerID: "server-a", OriginalItemID: "movie-a", PositionTicks: 555, IsFavorite: true}); err != nil {
						t.Fatal(err)
					}
					rr = doAuthJSON(t, f.handler, http.MethodDelete, "/admin/api/users/"+f.user, nil, admin)
					if rr.Code != 200 {
						t.Fatalf("delete %d %s", rr.Code, rr.Body.String())
					}
				case "delete_server_recovery":
					if err := f.app.IDStore.db.exec("CREATE TRIGGER t6_p4_fail BEFORE DELETE ON media_merge_members BEGIN SELECT RAISE(ABORT,'test cleanup failure'); END"); err != nil {
						t.Fatal(err)
					}
					rr := doAuthJSON(t, f.handler, http.MethodDelete, "/admin/api/upstream/server-b", nil, admin)
					if rr.Code != 500 || !f.app.lifecyclePending {
						t.Fatalf("pending %d %s", rr.Code, rr.Body.String())
					}
					if err := f.app.IDStore.db.exec("DROP TRIGGER t6_p4_fail"); err != nil {
						t.Fatal(err)
					}
					rr = doAuthJSON(t, f.handler, http.MethodPut, "/admin/api/users/"+f.user, map[string]any{}, admin)
					if rr.Code != 200 || f.app.lifecyclePending {
						t.Fatalf("recovery %d %s", rr.Code, rr.Body.String())
					}
				}
				gate.unblock()
				rr := task6Phase4Wait(t, done)
				if rr.Code != 204 {
					t.Fatalf("admitted response %d %s", rr.Code, rr.Body.String())
				}
				switch operation {
				case "delete_user":
					if f.app.WatchStore.GetProgress(f.user, canonical) != nil || f.app.WatchStore.GetProgress(f.user, alias) != nil {
						t.Fatal("deleted alias/canonical history recreated")
					}
					bob := f.app.WatchStore.GetProgress(bobID, canonical)
					if bob == nil || bob.PositionTicks != 555 || !bob.IsFavorite {
						t.Fatalf("bob changed %+v", bob)
					}
				case "unbind":
					current := f.app.WatchStore.GetProgress(f.user, canonical)
					if current == nil || current.PositionTicks != before.PositionTicks {
						t.Fatalf("revoked event wrote %+v before %+v", current, before)
					}
					if rr = f.request(t, http.MethodGet, "/Items/"+alias, nil, ""); rr.Code != 401 {
						t.Fatalf("stale token %d", rr.Code)
					}
					f.token = loginTokenAs(t, f.handler, "alice", "password123")
					state := task6HTTPState(t, f, alias, "")
					if state["PlaybackPositionTicks"] != float64(400) {
						t.Fatalf("alias visible state %+v", state)
					}
					if rr = f.request(t, http.MethodGet, "/Items/"+alias+"/PlaybackInfo?MediaSourceId="+media, nil, ""); rr.Code < 400 {
						t.Fatal("unbound version allowed")
					}
				case "delete_server_recovery":
					current := f.app.WatchStore.GetProgress(f.user, canonical)
					if current == nil || current.PositionTicks != 800 || current.RuntimeTicks != 0 || current.ServerID != "server-a" {
						t.Fatalf("inherited event %+v", current)
					}
					if f.app.IDStore.CanonicalMergeID(alias) != canonical || f.app.IDStore.ResolveMergeMember("server-b", "movie-b", "b100") != "" {
						t.Fatal("alias lost or deleted source survived")
					}
					if _, err := f.app.IDStore.GetMergeMediaSourceID("server-b", "movie-b", "b100"); err == nil {
						t.Fatal("deleted source recreated")
					}
					f.token = loginTokenAs(t, f.handler, "alice", "password123")
					state := task6HTTPState(t, f, alias, "")
					if state["PlaybackPositionTicks"] != float64(800) {
						t.Fatalf("inherited alias state %+v", state)
					}
				}
			})
		})
	}
}

func TestTask6Phase4AliasStateRestart(t *testing.T) {
	dir := t.TempDir()
	store, err := NewIDStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	a, b := task6MergeMovie("server-a", "movie-a", 1000), task6MergeMovie("server-b", "movie-b", 1000)
	left, err := store.RegisterMergeCandidate(a, task6Source("movie-a"))
	if err != nil {
		t.Fatal(err)
	}
	right, err := store.RegisterMergeCandidate(b, task6Source("movie-b"))
	if err != nil {
		t.Fatal(err)
	}
	ws, err := NewWatchStore(store.DB(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []WatchProgress{
		{ProxyUserID: "user", VirtualItemID: left, ServerID: "server-a", PositionTicks: 100, UpdatedAt: 1000, LastPlayed: 100},
		{ProxyUserID: "user", VirtualItemID: right, ServerID: "server-b", PositionTicks: 200, IsFavorite: true, UpdatedAt: 1000, LastPlayed: 200},
	} {
		p := p
		if err := ws.RecordProgress(&p); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.AssociateMergePair(a, b, task6Source("movie-a"), task6Source("movie-b"), left); err != nil {
		t.Fatal(err)
	}
	scope := mediaAccessScope{userID: "user", serverIDs: []string{"server-a", "server-b"}, onlineServerIDs: []string{"server-a", "server-b"}, allowed: map[string]struct{}{"server-a": {}, "server-b": {}}}
	read := func(w *WatchStore) {
		t.Helper()
		p, err := w.GetVisibleProgress(scope, left)
		if err != nil || p == nil || p.PositionTicks != 100 || p.IsFavorite {
			t.Fatalf("canonical tie %+v %v", p, err)
		}
	}
	read(ws)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = NewIDStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ws, err = NewWatchStore(store.DB(), nil)
	if err != nil {
		t.Fatal(err)
	}
	read(ws)
	if err := ws.SeedMergeState("user", left); err != nil {
		t.Fatal(err)
	}
	if err := ws.SetFavorite("user", left, true); err != nil {
		t.Fatal(err)
	}
	if alias := ws.GetProgress("user", right); alias == nil || alias.PositionTicks != 200 || !alias.IsFavorite || alias.UpdatedAt != 1000 {
		t.Fatalf("alias rewritten %+v", alias)
	}
	if store.CanonicalMergeID(right) != left {
		t.Fatal("restart alias mapping")
	}
}
