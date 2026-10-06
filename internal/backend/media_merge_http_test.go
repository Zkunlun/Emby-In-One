package backend

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
)

type task6HTTPCall struct {
	Path  string
	Query url.Values
	Body  map[string]any
}
type task6HTTPUpstream struct {
	server       *httptest.Server
	label        string
	mu           sync.Mutex
	items        map[string]map[string]any
	calls        []task6HTTPCall
	progressGate *task6Phase4Gate
}

func newTask6HTTPUpstream(t *testing.T, label string, items []map[string]any) *task6HTTPUpstream {
	t.Helper()
	u := &task6HTTPUpstream{label: label, items: map[string]map[string]any{}}
	for _, item := range items {
		id, _ := item["Id"].(string)
		u.items[id] = deepCloneMap(item)
	}
	u.server = httptest.NewServer(http.HandlerFunc(u.serve))
	t.Cleanup(u.server.Close)
	return u
}
func (u *task6HTTPUpstream) update(item map[string]any) {
	u.mu.Lock()
	defer u.mu.Unlock()
	id, _ := item["Id"].(string)
	u.items[id] = deepCloneMap(item)
}
func (u *task6HTTPUpstream) count(path string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	n := 0
	for _, call := range u.calls {
		if call.Path != "/Items/Counts" && strings.Contains(call.Path, path) {
			n++
		}
	}
	return n
}
func (u *task6HTTPUpstream) serve(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	u.mu.Lock()
	u.calls = append(u.calls, task6HTTPCall{Path: r.URL.Path, Query: cloneValues(r.URL.Query()), Body: body})
	items := map[string]map[string]any{}
	for id, item := range u.items {
		items[id] = deepCloneMap(item)
	}
	gate := u.progressGate
	u.mu.Unlock()
	write := func(value any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(value)
	}
	path := r.URL.Path
	if path == "/Users/AuthenticateByName" {
		write(map[string]any{"AccessToken": "up-" + u.label, "User": map[string]any{"Id": "user-" + u.label}})
		return
	}
	if path == "/System/Info/Public" {
		write(map[string]any{"Id": "source-" + u.label})
		return
	}
	if path == "/Items/Counts" {
		write(map[string]any{"MovieCount": len(items), "SeriesCount": 0, "EpisodeCount": 0})
		return
	}
	if strings.HasPrefix(path, "/Sessions/Playing") {
		if path == "/Sessions/Playing/Progress" && gate != nil {
			gate.enteredOnce.Do(func() { close(gate.entered) })
			select {
			case <-gate.release:
			case <-r.Context().Done():
				return
			}
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if strings.HasPrefix(path, "/Videos/") {
		parts := strings.Split(path, "/")
		if strings.HasSuffix(path, "/master.m3u8") {
			w.Header().Set("Content-Type", "application/x-mpegURL")
			_, _ = fmt.Fprint(w, "#EXTM3U\nsegment.ts\n")
			return
		}
		_, _ = fmt.Fprintf(w, "%s:%s:%s", u.label, parts[2], r.URL.Query().Get("MediaSourceId"))
		return
	}
	if strings.HasSuffix(path, "/PlaybackInfo") {
		parts := strings.Split(path, "/")
		item := items[parts[2]]
		if item == nil {
			http.NotFound(w, r)
			return
		}
		selected := r.URL.Query().Get("MediaSourceId")
		if selected == "" {
			selected, _ = body["MediaSourceId"].(string)
		}
		var sources []any
		for _, source := range asItems(map[string]any{"Items": item["MediaSources"]}) {
			if selected != "" && source["Id"] != selected {
				continue
			}
			source["DirectStreamUrl"] = "/Videos/" + parts[2] + "/stream.mp4"
			sources = append(sources, source)
		}
		write(map[string]any{"PlaySessionId": "play-" + u.label + "-" + parts[2], "MediaSources": sources})
		return
	}
	detailID := ""
	if strings.HasPrefix(path, "/Users/user-"+u.label+"/Items/") && !strings.HasSuffix(path, "/Latest") {
		detailID = strings.TrimPrefix(path, "/Users/user-"+u.label+"/Items/")
	}
	if strings.HasPrefix(path, "/Items/") {
		detailID = strings.TrimPrefix(path, "/Items/")
	}
	if item := items[detailID]; item != nil {
		write(item)
		return
	}
	if strings.Contains(path, "/FavoriteItems/") || strings.Contains(path, "/PlayedItems/") {
		write(map[string]any{})
		return
	}
	list := path == "/Items" || path == "/Users/user-"+u.label+"/Items" || strings.HasSuffix(path, "/Items/Latest") || path == "/Search/Hints" || strings.HasPrefix(path, "/Shows/")
	if !list {
		http.NotFound(w, r)
		return
	}
	ids := strings.Split(r.URL.Query().Get("Ids"), ",")
	requested := map[string]bool{}
	for _, id := range ids {
		if id != "" {
			requested[id] = true
		}
	}
	keys := make([]string, 0, len(items))
	for id := range items {
		keys = append(keys, id)
	}
	sort.Strings(keys)
	var rows []any
	for _, id := range keys {
		item := items[id]
		if len(requested) > 0 && !requested[id] {
			continue
		}
		kind, _ := item["Type"].(string)
		if types := r.URL.Query().Get("IncludeItemTypes"); types != "" && !containsString(strings.Split(types, ","), kind) {
			continue
		}
		if strings.HasPrefix(path, "/Shows/") {
			parts := strings.Split(path, "/")
			if len(parts) > 3 && parts[3] == "Episodes" && (kind != "Episode" || item["SeriesId"] != parts[2]) {
				continue
			}
			if len(parts) > 3 && parts[3] == "Seasons" && (kind != "Season" || item["SeriesId"] != parts[2]) {
				continue
			}
		}
		if parent, _ := item["ParentId"].(string); parent != "" && r.URL.Query().Get("ParentId") != "" && parent != r.URL.Query().Get("ParentId") {
			continue
		}
		if term := r.URL.Query().Get("SearchTerm"); term != "" {
			name, _ := item["Name"].(string)
			if !strings.Contains(strings.ToLower(name), strings.ToLower(term)) {
				continue
			}
		}
		if path == "/Search/Hints" {
			delete(item, "MediaSources")
			delete(item, "ProviderIds")
		}
		rows = append(rows, item)
	}
	total, start := len(rows), 0
	if raw := r.URL.Query().Get("StartIndex"); raw != "" {
		if value, ok := queryInt(r.URL.Query(), "StartIndex"); ok && value > 0 {
			start = value
		}
	}
	if start > len(rows) {
		start = len(rows)
	}
	rows = rows[start:]
	if limit, ok := queryInt(r.URL.Query(), "Limit"); ok && limit >= 0 && limit < len(rows) {
		rows = rows[:limit]
	}
	if path == "/Search/Hints" {
		write(map[string]any{"SearchHints": rows, "TotalRecordCount": len(rows)})
		return
	}
	if strings.HasSuffix(path, "/Latest") {
		write(rows)
		return
	}
	write(map[string]any{"Items": rows, "TotalRecordCount": total, "StartIndex": start})
}

type task6HTTPFixture struct {
	app      *App
	handler  http.Handler
	token    string
	user     string
	upstream [2]*task6HTTPUpstream
}

func withTask6HTTPFixture(t *testing.T, itemsA, itemsB []map[string]any, fn func(*task6HTTPFixture)) {
	t.Helper()
	a, b := newTask6HTTPUpstream(t, "a", itemsA), newTask6HTTPUpstream(t, "b", itemsB)
	config := strings.ReplaceAll(phase1EDualPlaybackConfig(a.server.URL, b.server.URL), "maxConcurrent: 1", "maxConcurrent: 0")
	withTempAppConfig(t, config, func(app *App, handler http.Handler) {
		token := phase1ECreateAuthorizedUser(t, handler, []string{"server-a", "server-b"})
		info := app.Auth.ValidateToken(token)
		if info == nil {
			t.Fatal("token missing")
		}
		fn(&task6HTTPFixture{app: app, handler: handler, token: token, user: info.UserID, upstream: [2]*task6HTTPUpstream{a, b}})
	})
}
func (f *task6HTTPFixture) request(t *testing.T, method, path string, body any, token string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, reader)
	if token == "" {
		token = f.token
	}
	req.Header.Set("X-Emby-Token", token)
	req.Header.Set("X-Emby-Device-Id", "real-device")
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	f.handler.ServeHTTP(rr, req)
	return rr
}
func task6HTTPJSON(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if rr.Code != 200 {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}
func (f *task6HTTPFixture) list(t *testing.T, kind string) []map[string]any {
	return asItems(task6HTTPJSON(t, f.request(t, http.MethodGet, "/Users/"+f.user+"/Items?Recursive=true&IncludeItemTypes="+kind, nil, "")))
}
func task6HTTPMovie(id string, sources ...map[string]any) map[string]any {
	raw := []any{}
	for _, source := range sources {
		raw = append(raw, source)
	}
	return map[string]any{"Id": id, "Type": "Movie", "Name": "Example", "ProductionYear": 2024, "ProviderIds": map[string]any{"Tmdb": "1"},
		"RunTimeTicks": 9999, "MediaSources": raw, "UserData": map[string]any{"ItemId": id, "Played": true, "IsFavorite": true, "PlaybackPositionTicks": 999}}
}
func task6HTTPSource(id string, ticks any) map[string]any {
	return map[string]any{"Id": id, "RunTimeTicks": ticks, "Container": "mp4", "Protocol": "File"}
}
func task6HTTPGroup(t *testing.T, f *task6HTTPFixture, server, item, source string) string {
	t.Helper()
	id := f.app.IDStore.ResolveMergeMember(server, item, source)
	if id == "" {
		t.Fatalf("member not registered %s/%s/%s", server, item, source)
	}
	return id
}
func task6HTTPState(t *testing.T, f *task6HTTPFixture, id, token string) map[string]any {
	item := task6HTTPJSON(t, f.request(t, http.MethodGet, "/Users/"+f.user+"/Items/"+id, nil, token))
	ud, ok := item["UserData"].(map[string]any)
	if !ok {
		t.Fatalf("missing state %+v", item)
	}
	return ud
}

func TestTask6HTTPVersionsListsAndDetails(t *testing.T) {
	a := task6HTTPMovie("movie-a", task6HTTPSource("a100", 1000), task6HTTPSource("a200", 2000), task6HTTPSource("au", nil))
	b := task6HTTPMovie("movie-b", task6HTTPSource("b100", 1000), task6HTTPSource("b201", 2001), task6HTTPSource("bu", nil))
	withTask6HTTPFixture(t, []map[string]any{a}, []map[string]any{b}, func(f *task6HTTPFixture) {
		if f.upstream[0].count("/Items") != 0 || f.upstream[1].count("/Items") != 0 {
			t.Fatal("startup media scan")
		}
		rows := f.list(t, "Movie")
		if len(rows) != 1 {
			t.Fatalf("version tiles=%d: %+v", len(rows), rows)
		}
		shared := task6HTTPGroup(t, f, "server-a", "movie-a", "a100")
		if shared != task6HTTPGroup(t, f, "server-b", "movie-b", "b100") {
			t.Fatal("equal versions not shared")
		}
		seen := map[string]bool{}
		for _, row := range rows {
			id, _ := row["Id"].(string)
			if id == "" || seen[id] {
				t.Fatalf("invalid tile %+v", row)
			}
			seen[id] = true
		}
		for _, path := range []string{"/Items/" + shared, "/Users/" + f.user + "/Items/" + shared} {
			detail := task6HTTPJSON(t, f.request(t, http.MethodGet, path, nil, ""))
			sources := asItems(map[string]any{"Items": detail["MediaSources"]})
			if len(sources) != 6 {
				t.Fatalf("mixed detail versions %+v", sources)
			}
			for _, source := range sources {
				virtual, _ := source["Id"].(string)
				mapped := f.app.IDStore.ResolveVirtualID(virtual)
				if mapped == nil || !containsString([]string{"a100", "a200", "au", "b100", "b201", "bu"}, mapped.OriginalID) || source["ItemId"] != shared {
					t.Fatalf("wrong detail source %+v -> %+v", source, mapped)
				}
			}
		}
		hints := task6HTTPJSON(t, f.request(t, http.MethodGet, "/Search/Hints?SearchTerm=example", nil, ""))
		if len(asItems(map[string]any{"Items": hints["SearchHints"]})) != 1 {
			t.Fatalf("hints %+v", hints)
		}
		latest := f.request(t, http.MethodGet, "/Users/"+f.user+"/Items/Latest", nil, "")
		var latestRows []map[string]any
		if latest.Code != 200 || json.Unmarshal(latest.Body.Bytes(), &latestRows) != nil || len(latestRows) != 1 {
			t.Fatalf("latest=%s", latest.Body.String())
		}
		for _, u := range f.upstream {
			u.mu.Lock()
			for _, call := range u.calls {
				if strings.HasSuffix(call.Path, "/Items") && call.Query.Get("Ids") == "" {
					if !strings.Contains(call.Query.Get("Fields"), "MediaSources") {
						t.Fatal("source fields not requested")
					}
				}
			}
			u.mu.Unlock()
		}
	})
}

func TestTask6HTTPExactPlaybackAndNamespace(t *testing.T) {
	a := task6HTTPMovie("movie-a", task6HTTPSource("common", 1000), task6HTTPSource("cut", 2000))
	other := task6HTTPMovie("other-a", task6HTTPSource("common", 3000))
	other["ProviderIds"] = map[string]any{"Tmdb": "2"}
	b := task6HTTPMovie("movie-b", task6HTTPSource("b100", 1000))
	withTask6HTTPFixture(t, []map[string]any{a, other}, []map[string]any{b}, func(f *task6HTTPFixture) {
		f.list(t, "Movie")
		shared := task6HTTPGroup(t, f, "server-a", "movie-a", "common")
		cut := task6HTTPGroup(t, f, "server-a", "movie-a", "cut")
		wrong := task6HTTPGroup(t, f, "server-a", "other-a", "common")
		source1, err := f.app.IDStore.GetMergeMediaSourceID("server-a", "movie-a", "common")
		if err != nil {
			t.Fatal(err)
		}
		source2, err := f.app.IDStore.GetMergeMediaSourceID("server-a", "other-a", "common")
		if err != nil {
			t.Fatal(err)
		}
		if source1 == source2 {
			t.Fatal("same-text source collision")
		}
		rr := f.request(t, http.MethodGet, "/Items/"+shared+"/PlaybackInfo?MediaSourceId="+source2, nil, "")
		if rr.Code != 404 {
			t.Fatalf("wrong item source accepted status=%d", rr.Code)
		}
		wrongCut, _ := f.app.IDStore.GetMergeMediaSourceID("server-a", "movie-a", "cut")
		rr = f.request(t, http.MethodPost, "/Items/"+shared+"/PlaybackInfo", map[string]any{"MediaSourceId": wrongCut}, "")
		if rr.Code != 200 || len(asItems(task6HTTPJSON(t, rr)["MediaSources"])) != 1 {
			t.Fatalf("explicit version rejected %d", rr.Code)
		}
		playback := task6HTTPJSON(t, f.request(t, http.MethodPost, "/Items/"+shared+"/PlaybackInfo", map[string]any{}, ""))
		if len(asItems(map[string]any{"Items": playback["MediaSources"]})) != 3 {
			t.Fatalf("playback %+v", playback)
		}
		stream := f.request(t, http.MethodGet, "/Videos/"+cut+"/stream.mp4?MediaSourceId="+wrongCut, nil, "")
		if stream.Code != 200 || stream.Body.String() != "a:movie-a:cut" {
			t.Fatalf("default wrong version %d %s", stream.Code, stream.Body.String())
		}
		forged := f.request(t, http.MethodPost, "/Sessions/Playing", map[string]any{"ItemId": shared, "MediaSourceId": source2, "PlaySessionId": f.app.IDStore.GetOrCreateVirtualID("foreign-session", "server-a")}, "")
		if forged.Code != 400 {
			t.Fatalf("foreign session source accepted %d %s", forged.Code, forged.Body.String())
		}
		if task6HTTPGroup(t, f, "server-a", "other-a", "common") != wrong {
			t.Fatal("foreign group changed")
		}
	})
}

func TestTask6HTTPSharedStateAndIsolation(t *testing.T) {
	a := task6HTTPMovie("movie-a", task6HTTPSource("a100", 1000), task6HTTPSource("cut", 2000))
	b := task6HTTPMovie("movie-b", task6HTTPSource("b100", 1000))
	withTask6HTTPFixture(t, []map[string]any{a}, []map[string]any{b}, func(f *task6HTTPFixture) {
		f.list(t, "Movie")
		shared := task6HTTPGroup(t, f, "server-a", "movie-a", "a100")
		cut := task6HTTPGroup(t, f, "server-a", "movie-a", "cut")
		playback := task6HTTPJSON(t, f.request(t, http.MethodPost, "/Items/"+shared+"/PlaybackInfo", map[string]any{}, ""))
		session, _ := playback["PlaySessionId"].(string)
		server := f.app.IDStore.ResolveVirtualID(session).ServerID
		item := "movie-a"
		source := "a100"
		if server == "server-b" {
			item = "movie-b"
			source = "b100"
		}
		media, _ := f.app.IDStore.GetMergeMediaSourceID(server, item, source)
		body := map[string]any{"ItemId": shared, "MediaSourceId": media, "PlaySessionId": session, "PositionTicks": 200, "RunTimeTicks": 1000}
		for _, step := range []struct {
			path     string
			position int
		}{{"/Sessions/Playing", 200}, {"/Sessions/Playing/Progress", 400}, {"/Sessions/Playing/Stopped", 500}} {
			body["PositionTicks"] = step.position
			rr := f.request(t, http.MethodPost, step.path, body, "")
			if rr.Code != 204 {
				t.Fatalf("event %s %d %s", step.path, rr.Code, rr.Body.String())
			}
		}
		rr := f.request(t, http.MethodPost, "/Users/"+f.user+"/FavoriteItems/"+shared, nil, "")
		if rr.Code != 200 {
			t.Fatalf("favorite %d %s", rr.Code, rr.Body.String())
		}
		state := task6HTTPState(t, f, shared, "")
		if state["PlaybackPositionTicks"] != float64(500) || state["IsFavorite"] != true || state["Played"] != false {
			t.Fatalf("shared state %+v", state)
		}
		rr = f.request(t, http.MethodPost, "/Users/"+f.user+"/PlayedItems/"+shared, nil, "")
		if rr.Code != 200 {
			t.Fatalf("shared played %d %s", rr.Code, rr.Body.String())
		}
		if played := task6HTTPState(t, f, shared, "")["Played"]; played != true {
			t.Fatalf("played not shared: %v", played)
		}
		independent := task6HTTPState(t, f, cut, "")
		if cut != shared || independent["Played"] != true || independent["IsFavorite"] != true || independent["PlaybackPositionTicks"] != task6HTTPState(t, f, shared, "")["PlaybackPositionTicks"] {
			t.Fatalf("versions do not share state %+v", independent)
		}
		adminToken := loginTokenAs(t, f.handler, "admin", "secret")
		created := doAuthJSON(t, f.handler, http.MethodPost, "/admin/api/users", map[string]any{"username": "bob", "password": "password123", "allowedServers": []string{"server-a", "server-b"}}, adminToken)
		if created.Code != http.StatusCreated {
			t.Fatalf("create second user: %d %s", created.Code, created.Body.String())
		}
		token2 := loginTokenAs(t, f.handler, "bob", "password123")
		second := *f
		second.user = f.app.Auth.ValidateToken(token2).UserID
		state2 := task6HTTPState(t, &second, shared, token2)
		if state2["PlaybackPositionTicks"] != float64(0) || state2["IsFavorite"] != false {
			t.Fatalf("user leakage %+v", state2)
		}
	})
}

func TestTask6HTTPAliasesPreserveRowsAndReset(t *testing.T) {
	a := task6HTTPMovie("movie-a", task6HTTPSource("a100", 1000))
	b := task6HTTPMovie("movie-b", task6HTTPSource("b100", nil))
	b["RunTimeTicks"] = nil
	withTask6HTTPFixture(t, []map[string]any{a}, []map[string]any{b}, func(f *task6HTTPFixture) {
		task6SeedSeparateItems(t, f, a, b)
		left := task6HTTPGroup(t, f, "server-a", "movie-a", "a100")
		alias := task6HTTPGroup(t, f, "server-b", "movie-b", "b100")
		for _, row := range []WatchProgress{
			{ProxyUserID: f.user, VirtualItemID: left, ServerID: "server-a", OriginalItemID: "movie-a", ItemType: "Movie", PositionTicks: 100, RuntimeTicks: 1000, UpdatedAt: 500, LastPlayed: 100},
			{ProxyUserID: f.user, VirtualItemID: alias, ServerID: "server-b", OriginalItemID: "movie-b", ItemType: "Movie", PositionTicks: 200, RuntimeTicks: 1000, IsFavorite: true, UpdatedAt: 1000, LastPlayed: 200},
		} {
			copy := row
			if err := f.app.WatchStore.RecordProgress(&copy); err != nil {
				t.Fatal(err)
			}
		}
		old := *f.app.WatchStore.GetProgress(f.user, alias)
		b["MediaSources"] = []any{task6HTTPSource("b100", 1000)}
		f.upstream[1].update(b)
		if len(f.list(t, "Movie")) != 1 {
			t.Fatal("newly known matching version not merged")
		}
		canonical := f.app.IDStore.CanonicalMergeID(alias)
		if canonical == alias {
			t.Fatal("no preserved alias")
		}
		if current := f.app.WatchStore.GetProgress(f.user, alias); current == nil || *current != old {
			t.Fatalf("discovery rewrote old row %+v", current)
		}
		for _, id := range []string{canonical, alias} {
			state := task6HTTPState(t, f, id, "")
			if state["PlaybackPositionTicks"] != float64(200) || state["IsFavorite"] != true {
				t.Fatalf("alias state %+v", state)
			}
		}
		rr := f.request(t, http.MethodDelete, "/Users/"+f.user+"/FavoriteItems/"+alias, nil, "")
		if rr.Code != 200 {
			t.Fatalf("unfavorite %d %s", rr.Code, rr.Body.String())
		}
		if state := task6HTTPState(t, f, canonical, ""); state["PlaybackPositionTicks"] != float64(200) || state["IsFavorite"] != false {
			t.Fatalf("canonical mutation %+v", state)
		}
		if current := f.app.WatchStore.GetProgress(f.user, alias); current == nil || *current != old {
			t.Fatal("alias row mutated")
		}
		media, _ := f.app.IDStore.GetMergeMediaSourceID("server-b", "movie-b", "b100")
		session := f.app.IDStore.GetOrCreateVirtualID("alias-session", "server-b")
		body := map[string]any{"ItemId": alias, "MediaSourceId": media, "PlaySessionId": session, "PositionTicks": 400, "RunTimeTicks": 1000}
		if rr := f.request(t, http.MethodPost, "/Sessions/Playing", body, ""); rr.Code != 204 {
			t.Fatalf("alias start %d %s", rr.Code, rr.Body.String())
		}
		body["PositionTicks"] = 500
		if rr := f.request(t, http.MethodPost, "/Sessions/Playing/Progress", body, ""); rr.Code != 204 {
			t.Fatal("alias progress")
		}
		rr = f.request(t, http.MethodDelete, "/Users/"+f.user+"/PlayedItems/"+alias, nil, "")
		if rr.Code != 200 {
			t.Fatalf("reset %d %s", rr.Code, rr.Body.String())
		}
		body["PositionTicks"] = 700
		f.request(t, http.MethodPost, "/Sessions/Playing/Progress", body, "")
		state := task6HTTPState(t, f, canonical, "")
		if state["PlaybackPositionTicks"] != float64(0) || state["Played"] != false {
			t.Fatalf("reset resurrected %+v", state)
		}
		resume := task6HTTPJSON(t, f.request(t, http.MethodGet, "/Users/"+f.user+"/Items/Resume", nil, ""))
		if len(asItems(resume)) != 0 {
			t.Fatalf("old alias resume resurrected %+v", resume)
		}
	})
}

func TestTask6HTTPParentAndEpisodeConsistency(t *testing.T) {
	makeItems := func(label string, second int) []map[string]any {
		series := map[string]any{"Id": "series-" + label, "Type": "Series", "Name": "Example Series", "ProductionYear": 2024, "ProviderIds": map[string]any{"Tmdb": "99"}}
		season := map[string]any{"Id": "season-" + label, "Type": "Season", "Name": "Season 1", "SeriesId": "series-" + label, "IndexNumber": 1}
		ep1 := task6HTTPMovie("ep1-"+label, task6HTTPSource("ep1-ms-"+label, 1000))
		ep2 := task6HTTPMovie("ep2-"+label, task6HTTPSource("ep2-ms-"+label, second))
		for i, ep := range []map[string]any{ep1, ep2} {
			ep["Type"] = "Episode"
			ep["SeriesId"] = "series-" + label
			ep["ParentIndexNumber"] = 1
			ep["IndexNumber"] = i + 1
			delete(ep, "ProviderIds")
		}
		return []map[string]any{series, season, ep1, ep2}
	}
	withTask6HTTPFixture(t, makeItems("a", 2000), makeItems("b", 2001), func(f *task6HTTPFixture) {
		global := f.list(t, "Episode")
		if len(global) != 2 {
			t.Fatalf("episode groups=%d %+v", len(global), global)
		}
		parent := task6HTTPGroup(t, f, "server-a", "series-a", "")
		if parent != task6HTTPGroup(t, f, "server-b", "series-b", "") {
			t.Fatal("parents not confirmed")
		}
		season := task6HTTPJSON(t, f.request(t, http.MethodGet, "/Shows/"+parent+"/Seasons", nil, ""))
		if len(asItems(season)) != 1 {
			t.Fatalf("season %+v", season)
		}
		episodes := task6HTTPJSON(t, f.request(t, http.MethodGet, "/Shows/"+parent+"/Episodes", nil, ""))
		if len(asItems(episodes)) != 2 {
			t.Fatalf("episodes %+v", episodes)
		}
		seen := map[string]bool{}
		for _, ep := range global {
			seen[ep["Id"].(string)] = true
		}
		for _, ep := range asItems(episodes) {
			if !seen[ep["Id"].(string)] {
				t.Fatalf("global/special identity mismatch %+v", ep)
			}
		}
	})
}

func TestTask6HTTPPlaceholderAndQualifiedRouteRestart(t *testing.T) {
	dir := t.TempDir()
	store, err := NewIDStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	item := task6HTTPMovie("movie")
	delete(item, "MediaSources")
	unresolved := newMergeCandidate("server", item, nil, false)
	id, err := store.RegisterMergePlaceholder(unresolved)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = NewIDStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if group := store.ResolveMergeGroup(id); group == nil || group.Policy != mergePolicyWork {
		t.Fatalf("unresolved restart %+v", group)
	}
	item["MediaSources"] = []any{task6HTTPSource("source", 1000), task6HTTPSource("cut", 2000)}
	full := newMergeCandidate("server", item, nil, true)
	if err := store.BindMergePlaceholder(id, full); err != nil {
		t.Fatal(err)
	}
	if !store.MergeMemberAllowed(id, "server", "movie", "source") || !store.MergeMemberAllowed(id, "server", "movie", "cut") {
		t.Fatal("placeholder did not retain all versions")
	}
	media, err := store.GetMergeMediaSourceID("server", "movie", "source")
	if err != nil {
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
	if group := reopened.ResolveMergeGroup(id); group == nil || group.Policy != mergePolicyWork {
		t.Fatalf("restart group %+v", group)
	}
	mapped := reopened.ResolveVirtualID(media)
	if mapped == nil || mapped.MediaItemID != "movie" || mapped.OriginalID != "source" {
		t.Fatalf("restart source %+v", mapped)
	}
}

func TestTask6HTTPHLSKeepsSelectedVersion(t *testing.T) {
	a := task6HTTPMovie("movie-a", task6HTTPSource("same", 1000))
	b := task6HTTPMovie("movie-b", task6HTTPSource("same", 1000), task6HTTPSource("cut", 2000))
	withTask6HTTPFixture(t, []map[string]any{a}, []map[string]any{b}, func(f *task6HTTPFixture) {
		f.list(t, "Movie")
		shared := task6HTTPGroup(t, f, "server-a", "movie-a", "same")
		selected, err := f.app.IDStore.GetMergeMediaSourceID("server-b", "movie-b", "same")
		if err != nil {
			t.Fatal(err)
		}
		rr := f.request(t, http.MethodGet, "/Videos/"+shared+"/master.m3u8?MediaSourceId="+selected, nil, "")
		if rr.Code != 200 {
			t.Fatalf("manifest %d %s", rr.Code, rr.Body.String())
		}
		lines := strings.Split(rr.Body.String(), "\n")
		segment := ""
		for _, line := range lines {
			if strings.HasPrefix(line, "/Videos/") {
				segment = line
			}
		}
		u, err := url.Parse(segment)
		if err != nil || segment == "" {
			t.Fatalf("segment %q %v", segment, err)
		}
		if u.Query().Get("MediaSourceId") != selected {
			t.Fatalf("version lost: %s", segment)
		}
		rr = f.request(t, http.MethodGet, segment, nil, "")
		if rr.Code != 200 || rr.Body.String() != "b:movie-b:same" {
			t.Fatalf("segment misrouted %d %s", rr.Code, rr.Body.String())
		}
	})
}

func TestTask6HTTPResumeAndFallbackKeepVersion(t *testing.T) {
	a := task6HTTPMovie("movie-a", task6HTTPSource("a100", 1000), task6HTTPSource("cut", 2000))
	b := task6HTTPMovie("movie-b", task6HTTPSource("b100", 1000))
	withTask6HTTPFixture(t, []map[string]any{a}, []map[string]any{b}, func(f *task6HTTPFixture) {
		f.list(t, "Movie")
		shared := task6HTTPGroup(t, f, "server-a", "movie-a", "a100")
		if err := f.app.WatchStore.RecordProgress(&WatchProgress{ProxyUserID: f.user, VirtualItemID: shared, ServerID: "server-a", OriginalItemID: "movie-a", ItemType: "Movie", PositionTicks: 200, RuntimeTicks: 1000, UpdatedAt: 1000}); err != nil {
			t.Fatal(err)
		}
		rows := asItems(task6HTTPJSON(t, f.request(t, http.MethodGet, "/Users/"+f.user+"/Items/Resume", nil, "")))
		if len(rows) != 1 || rows[0]["Id"] != shared {
			t.Fatalf("resume %+v", rows)
		}
		sources := asItems(rows[0]["MediaSources"])
		if len(sources) != 2 {
			t.Fatalf("resume cuts %+v", sources)
		}
		source, _ := sources[0]["Id"].(string)
		mapped := f.app.IDStore.ResolveVirtualID(source)
		if mapped == nil || mapped.OriginalID != "a100" {
			t.Fatalf("resume source %+v", mapped)
		}
		parent := f.app.IDStore.GetOrCreateVirtualID("library-a", "server-a")
		rows = asItems(task6HTTPJSON(t, f.request(t, http.MethodGet, "/Items?ParentId="+parent+"&IncludeItemTypes=Movie", nil, "")))
		if len(rows) != 1 {
			t.Fatalf("single-source fallback tiles %+v", rows)
		}
		for _, row := range rows {
			if len(asItems(row["MediaSources"])) != 2 {
				t.Fatalf("fallback mixed cuts %+v", row)
			}
		}
	})
}

func TestTask6HTTPNextUpKeepsCurrentCut(t *testing.T) {
	series := map[string]any{"Id": "series-a", "Type": "Series", "Name": "Example", "ProductionYear": 2024, "ProviderIds": map[string]any{"Tmdb": "99"}}
	ep := task6HTTPMovie("episode-a", task6HTTPSource("main", 1000), task6HTTPSource("cut", 2000))
	ep["Type"] = "Episode"
	ep["SeriesId"] = "series-a"
	ep["ParentIndexNumber"] = 1
	ep["IndexNumber"] = 1
	withTask6HTTPFixture(t, []map[string]any{series, ep}, nil, func(f *task6HTTPFixture) {
		f.list(t, "Episode")
		selected := task6HTTPGroup(t, f, "server-a", "episode-a", "cut")
		parent := task6HTTPGroup(t, f, "server-a", "series-a", "")
		if err := f.app.WatchStore.RecordProgress(&WatchProgress{ProxyUserID: f.user, VirtualItemID: selected, ServerID: "server-a", OriginalItemID: "episode-a", ItemType: "Episode", SeriesVirtualID: parent, SeriesOriginalID: "series-a", ParentIndexNumber: 1, IndexNumber: 1, PositionTicks: 200, RuntimeTicks: 2000, UpdatedAt: 1000}); err != nil {
			t.Fatal(err)
		}
		rows := asItems(task6HTTPJSON(t, f.request(t, http.MethodGet, "/Shows/NextUp?SeriesId="+parent, nil, "")))
		if len(rows) != 1 || rows[0]["Id"] != selected {
			t.Fatalf("nextup %+v", rows)
		}
		sources := asItems(rows[0]["MediaSources"])
		if len(sources) != 2 {
			t.Fatalf("nextup cuts %+v", sources)
		}
		source, _ := sources[0]["Id"].(string)
		mapped := f.app.IDStore.ResolveVirtualID(source)
		if mapped == nil || !containsString([]string{"main", "cut"}, mapped.OriginalID) {
			t.Fatalf("nextup source %+v", mapped)
		}
	})
}

func TestTask6HTTPAuthorizationAndOfflineVersions(t *testing.T) {
	a := task6HTTPMovie("movie-a", task6HTTPSource("a100", 1000))
	b := task6HTTPMovie("movie-b", task6HTTPSource("b100", 1000), task6HTTPSource("bcut", 2000))
	withTask6HTTPFixture(t, []map[string]any{a}, []map[string]any{b}, func(f *task6HTTPFixture) {
		f.list(t, "Movie")
		shared := task6HTTPGroup(t, f, "server-a", "movie-a", "a100")
		cut := task6HTTPGroup(t, f, "server-b", "movie-b", "bcut")
		source, err := f.app.IDStore.GetMergeMediaSourceID("server-b", "movie-b", "b100")
		if err != nil {
			t.Fatal(err)
		}
		admin := loginTokenAs(t, f.handler, "admin", "secret")
		rr := doAuthJSON(t, f.handler, http.MethodPost, "/admin/api/users", map[string]any{"username": "restricted", "password": "password123", "allowedServers": []string{"server-a"}}, admin)
		if rr.Code != 201 {
			t.Fatalf("restricted user %d %s", rr.Code, rr.Body.String())
		}
		token := loginTokenAs(t, f.handler, "restricted", "password123")
		before := f.upstream[1].count("/Items")
		detail := task6HTTPJSON(t, f.request(t, http.MethodGet, "/Items/"+shared, nil, token))
		if len(asItems(detail["MediaSources"])) != 1 {
			t.Fatalf("unauthorized detail %+v", detail)
		}
		if cut != shared {
			t.Fatal("versions split")
		}
		for _, path := range []string{"/Items/" + shared + "/PlaybackInfo?MediaSourceId=" + source, "/Videos/" + shared + "/stream.mp4?MediaSourceId=" + source} {
			rr = f.request(t, http.MethodGet, path, nil, token)
			if rr.Code < 400 {
				t.Fatalf("unauthorized %s %d", path, rr.Code)
			}
		}
		if after := f.upstream[1].count("/Items"); after != before {
			t.Fatalf("unauthorized metadata requests %d -> %d", before, after)
		}
		f.app.Upstream.ClientByID("server-b").setOffline("task6 fixture offline")
		before = f.upstream[1].count("/Items")
		detail = task6HTTPJSON(t, f.request(t, http.MethodGet, "/Items/"+shared, nil, ""))
		if len(asItems(detail["MediaSources"])) != 1 {
			t.Fatalf("offline detail %+v", detail)
		}
		rr = f.request(t, http.MethodGet, "/Items/"+shared+"/PlaybackInfo?MediaSourceId="+source, nil, "")
		if rr.Code < 400 {
			t.Fatalf("offline selection %d %s", rr.Code, rr.Body.String())
		}
		if after := f.upstream[1].count("/Items"); after != before {
			t.Fatalf("offline metadata requests %d -> %d", before, after)
		}
	})
}

func TestTask6HTTPAliasStoppedEndsExactLease(t *testing.T) {
	a := task6HTTPMovie("movie-a", task6HTTPSource("a100", 1000))
	b := task6HTTPMovie("movie-b", task6HTTPSource("b100", nil))
	b["RunTimeTicks"] = nil
	upA, upB := newTask6HTTPUpstream(t, "a", []map[string]any{a}), newTask6HTTPUpstream(t, "b", []map[string]any{b})
	withTempAppConfig(t, phase1EDualPlaybackConfig(upA.server.URL, upB.server.URL), func(app *App, handler http.Handler) {
		token := phase1ECreateAuthorizedUser(t, handler, []string{"server-a", "server-b"})
		f := &task6HTTPFixture{app: app, handler: handler, token: token, user: app.Auth.ValidateToken(token).UserID, upstream: [2]*task6HTTPUpstream{upA, upB}}
		task6SeedSeparateItems(t, f, a, b)
		alias := task6HTTPGroup(t, f, "server-b", "movie-b", "b100")
		media, err := app.IDStore.GetMergeMediaSourceID("server-b", "movie-b", "b100")
		if err != nil {
			t.Fatal(err)
		}
		rr := f.request(t, http.MethodGet, "/Items/"+alias+"/PlaybackInfo?MediaSourceId="+media, nil, "")
		session := phase1EPlaySessionID(t, rr)
		body := map[string]any{"ItemId": alias, "MediaSourceId": media, "PlaySessionId": session, "PositionTicks": 100, "RunTimeTicks": 1000}
		rr = f.request(t, http.MethodPost, "/Sessions/Playing", body, "")
		if rr.Code != 204 {
			t.Fatalf("alias started %d %s", rr.Code, rr.Body.String())
		}
		b["MediaSources"] = []any{task6HTTPSource("b100", 1000)}
		upB.update(b)
		f.list(t, "Movie")
		if app.IDStore.CanonicalMergeID(alias) == alias {
			t.Fatal("expected absorbed alias")
		}

		req := httptest.NewRequest(http.MethodGet, "/Items/"+alias+"/PlaybackInfo?MediaSourceId="+media, nil)
		req.Header.Set("X-Emby-Token", token)
		req.Header.Set("X-Emby-Device-Id", "other-device")
		probe := httptest.NewRecorder()
		handler.ServeHTTP(probe, req)
		if probe.Code != 429 {
			t.Fatalf("B lease not held %d %s", probe.Code, probe.Body.String())
		}
		rr = f.request(t, http.MethodPost, "/Sessions/Playing/Stopped", body, "")
		if rr.Code != 204 {
			t.Fatalf("alias stopped %d %s", rr.Code, rr.Body.String())
		}
		probe = httptest.NewRecorder()
		handler.ServeHTTP(probe, req.Clone(req.Context()))
		if probe.Code != 200 {
			t.Fatalf("alias lease not released %d %s", probe.Code, probe.Body.String())
		}
	})
}

func TestTask6HTTPMissingTypeCannotCreateLegacyScope(t *testing.T) {
	item := task6HTTPMovie("unknown", task6HTTPSource("source", 1000))
	delete(item, "Type")
	withTask6HTTPFixture(t, []map[string]any{item}, nil, func(f *task6HTTPFixture) {
		rows := f.list(t, "")
		if len(rows) != 0 {
			t.Fatalf("unqualified rows %+v", rows)
		}
		if _, _, found := f.app.IDStore.ResolveOriginalIDForServer("unknown", "server-a"); found {
			t.Fatal("new legacy scope from missing type")
		}
		item["Type"] = "Movie"
		f.upstream[0].update(item)
		if rows = f.list(t, "Movie"); len(rows) != 1 {
			t.Fatalf("retry %+v", rows)
		}
		id := task6HTTPGroup(t, f, "server-a", "unknown", "source")
		if group := f.app.IDStore.ResolveMergeGroup(id); group == nil || group.Policy != mergePolicyWork {
			t.Fatalf("retry policy %+v", group)
		}
	})
}

func TestTask6HTTPIdentityEntrypoints(t *testing.T) {
	cases := []struct {
		name        string
		left, right map[string]any
		want        int
	}{
		{"imdb", map[string]any{"Imdb": "tt123"}, map[string]any{"Imdb": "tt123"}, 1},
		{"tvdb", map[string]any{"Tvdb": "123"}, map[string]any{"Tvdb": "123"}, 1},
		{"name_year", map[string]any{"Tmdb": "1"}, map[string]any{"Imdb": "tt123"}, 1},
		{"direct_conflict", map[string]any{"Tmdb": "1", "Imdb": "tt1"}, map[string]any{"Tmdb": "1", "Imdb": "tt2"}, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := task6HTTPMovie("movie-a", task6HTTPSource("a100", 1000))
			b := task6HTTPMovie("movie-b", task6HTTPSource("b100", 1000))
			a["ProviderIds"] = tc.left
			b["ProviderIds"] = tc.right
			b["Name"] = "EXAMPLE"
			withTask6HTTPFixture(t, []map[string]any{a}, []map[string]any{b}, func(f *task6HTTPFixture) {
				if rows := f.list(t, "Movie"); len(rows) != tc.want {
					t.Fatalf("identity tiles %+v want %d", rows, tc.want)
				}
			})
		})
	}
	t.Run("same_source_structure", func(t *testing.T) {
		a := task6HTTPMovie("movie-a", task6HTTPSource("a100", 1000))
		duplicate := task6HTTPMovie("duplicate-a", task6HTTPSource("dup100", 1000))
		b := task6HTTPMovie("movie-b", task6HTTPSource("b100", 1000))
		withTask6HTTPFixture(t, []map[string]any{a, duplicate}, []map[string]any{b}, func(f *task6HTTPFixture) {
			if rows := f.list(t, "Movie"); len(rows) != 1 {
				t.Fatalf("same-source duplicates lost %+v", rows)
			}
			if task6HTTPGroup(t, f, "server-a", "movie-a", "a100") != task6HTTPGroup(t, f, "server-a", "duplicate-a", "dup100") {
				t.Fatal("same-source items not merged")
			}
		})
	})
}

func TestTask6HTTPDetailExplicitVersion(t *testing.T) {
	a := task6HTTPMovie("movie-a", task6HTTPSource("a100", 1000), task6HTTPSource("cut", 2000))
	b := task6HTTPMovie("movie-b", task6HTTPSource("b100", 1000))
	withTask6HTTPFixture(t, []map[string]any{a}, []map[string]any{b}, func(f *task6HTTPFixture) {
		f.list(t, "Movie")
		shared := task6HTTPGroup(t, f, "server-a", "movie-a", "a100")
		selected, err := f.app.IDStore.GetMergeMediaSourceID("server-b", "movie-b", "b100")
		if err != nil {
			t.Fatal(err)
		}
		detail := task6HTTPJSON(t, f.request(t, http.MethodGet, "/Items/"+shared+"?MediaSourceId="+selected, nil, ""))
		sources := asItems(detail["MediaSources"])
		if len(sources) != 1 || sources[0]["Id"] != selected {
			t.Fatalf("explicit detail %+v", detail)
		}
		cut, err := f.app.IDStore.GetMergeMediaSourceID("server-a", "movie-a", "cut")
		if err != nil {
			t.Fatal(err)
		}
		rr := f.request(t, http.MethodGet, "/Items/"+shared+"?MediaSourceId="+cut, nil, "")
		selectedCut := task6HTTPJSON(t, rr)
		if sources := asItems(selectedCut["MediaSources"]); len(sources) != 1 || sources[0]["Id"] != cut {
			t.Fatalf("selected cut detail %+v", selectedCut)
		}
	})
}

func TestTask6HTTPPersistenceFailureIsNotPublished(t *testing.T) {
	a := task6HTTPMovie("movie-a", task6HTTPSource("a100", 1000))
	withTask6HTTPFixture(t, []map[string]any{a}, nil, func(f *task6HTTPFixture) {
		if err := f.app.IDStore.db.exec("CREATE TRIGGER t6_http_fail BEFORE INSERT ON media_merge_groups BEGIN SELECT RAISE(ABORT,'test write failure'); END"); err != nil {
			t.Fatal(err)
		}
		rows := f.list(t, "Movie")
		if len(rows) != 0 {
			t.Fatalf("uncommitted tiles %+v", rows)
		}
		if id := f.app.IDStore.ResolveMergeMember("server-a", "movie-a", "a100"); id != "" {
			t.Fatal("uncommitted member")
		}
		if err := f.app.IDStore.db.exec("DROP TRIGGER t6_http_fail"); err != nil {
			t.Fatal(err)
		}
		if len(f.list(t, "Movie")) != 1 {
			t.Fatal("retry did not converge")
		}
	})
}
