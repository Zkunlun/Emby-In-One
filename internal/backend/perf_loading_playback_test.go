package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPerfImageCacheControlTagAndNoTag(t *testing.T) {
	imgBytes := []byte("fake-jpeg-payload-12345")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "tok-1", "User": map[string]any{"Id": "u-1"}})
		case r.Method == http.MethodGet && r.URL.Path == "/Items/orig-1/Images/Primary":
			w.Header().Set("Content-Type", "image/jpeg")
			w.Header().Set("ETag", `"etag-v1"`)
			w.Header().Set("Last-Modified", "Wed, 21 Oct 2015 07:28:00 GMT")
			w.Header().Set("Expires", "Thu, 21 Oct 2027 07:28:00 GMT")
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(imgBytes)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(imgBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	cfg := fmt.Sprintf("server:\n  port: 8096\n  name: \"Test\"\n  id: \"srv-1\"\nadmin:\n  username: \"admin\"\n  password: \"secret\"\nplayback:\n  mode: \"proxy\"\ntimeouts:\n  api: 10000\n  global: 10000\n  login: 5000\n  healthCheck: 5000\n  healthInterval: 60000\nproxies: []\nupstream:\n  - name: \"U1\"\n    url: %q\n    username: \"u\"\n    password: \"p\"\n", upstream.URL)

	withTempAppConfig(t, cfg, func(app *App, handler http.Handler) {
		vid := app.IDStore.GetOrCreateVirtualID("orig-1", app.Upstream.Clients()[0].ID)

		// With tag -> immutable 30d
		req := httptest.NewRequest(http.MethodGet, "/Items/"+vid+"/Images/Primary?tag=abc123", nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("tagged image status = %d, want 200", rr.Code)
		}
		if got := rr.Header().Get("Cache-Control"); got != "public, max-age=2592000, immutable" {
			t.Fatalf("tagged Cache-Control = %q, want immutable 30d", got)
		}
		if rr.Header().Get("ETag") != `"etag-v1"` || rr.Header().Get("Last-Modified") == "" || rr.Header().Get("Expires") == "" {
			t.Fatalf("expected ETag/Last-Modified/Expires forwarded, got %v", rr.Header())
		}
		if rr.Body.String() != string(imgBytes) {
			t.Fatalf("body mismatch: got %q", rr.Body.String())
		}

		// Without tag -> 1d
		req2 := httptest.NewRequest(http.MethodGet, "/Items/"+vid+"/Images/Primary", nil)
		rr2 := httptest.NewRecorder()
		handler.ServeHTTP(rr2, req2)
		if rr2.Code != http.StatusOK {
			t.Fatalf("untagged image status = %d, want 200", rr2.Code)
		}
		if got := rr2.Header().Get("Cache-Control"); got != "public, max-age=86400" {
			t.Fatalf("untagged Cache-Control = %q, want 1d", got)
		}
	})
}

func TestPerfImageConditional304AndIfNoneMatchForward(t *testing.T) {
	var seenINM atomic.Value
	var seenIMS atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "tok-1", "User": map[string]any{"Id": "u-1"}})
		case r.Method == http.MethodGet && r.URL.Path == "/Items/orig-304/Images/Primary":
			seenINM.Store(r.Header.Get("If-None-Match"))
			seenIMS.Store(r.Header.Get("If-Modified-Since"))
			w.Header().Set("ETag", `"etag-304"`)
			w.Header().Set("Last-Modified", "Wed, 21 Oct 2015 07:28:00 GMT")
			w.Header().Set("Expires", "Thu, 21 Oct 2027 07:28:00 GMT")
			w.WriteHeader(http.StatusNotModified)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	cfg := fmt.Sprintf("server:\n  port: 8096\n  name: \"Test\"\n  id: \"srv-1\"\nadmin:\n  username: \"admin\"\n  password: \"secret\"\nplayback:\n  mode: \"proxy\"\ntimeouts:\n  api: 10000\n  global: 10000\n  login: 5000\n  healthCheck: 5000\n  healthInterval: 60000\nproxies: []\nupstream:\n  - name: \"U1\"\n    url: %q\n    username: \"u\"\n    password: \"p\"\n", upstream.URL)

	withTempAppConfig(t, cfg, func(app *App, handler http.Handler) {
		vid := app.IDStore.GetOrCreateVirtualID("orig-304", app.Upstream.Clients()[0].ID)

		req := httptest.NewRequest(http.MethodGet, "/Items/"+vid+"/Images/Primary?tag=t304", nil)
		req.Header.Set("If-None-Match", `"etag-304"`)
		req.Header.Set("If-Modified-Since", "Wed, 21 Oct 2015 07:28:00 GMT")
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusNotModified {
			t.Fatalf("status = %d, want 304", rr.Code)
		}
		if rr.Body.Len() != 0 {
			t.Fatalf("304 body length = %d, want 0", rr.Body.Len())
		}
		if got := rr.Header().Get("Cache-Control"); got != "public, max-age=2592000, immutable" {
			t.Fatalf("304 Cache-Control = %q, want immutable 30d (not no-store)", got)
		}
		if rr.Header().Get("ETag") != `"etag-304"` {
			t.Fatalf("304 ETag = %q, want %q", rr.Header().Get("ETag"), `"etag-304"`)
		}
		if inm, _ := seenINM.Load().(string); inm != `"etag-304"` {
			t.Fatalf("upstream If-None-Match = %q, want %q", inm, `"etag-304"`)
		}
		if ims, _ := seenIMS.Load().(string); ims != "Wed, 21 Oct 2015 07:28:00 GMT" {
			t.Fatalf("upstream If-Modified-Since = %q", ims)
		}
	})
}

func TestPerfImageHeadNoBody(t *testing.T) {
	var seenMethod atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "tok-1", "User": map[string]any{"Id": "u-1"}})
		case r.URL.Path == "/Items/orig-head/Images/Primary":
			seenMethod.Store(r.Method)
			w.Header().Set("Content-Type", "image/webp")
			w.Header().Set("Content-Length", "1234")
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	cfg := fmt.Sprintf("server:\n  port: 8096\n  name: \"Test\"\n  id: \"srv-1\"\nadmin:\n  username: \"admin\"\n  password: \"secret\"\nplayback:\n  mode: \"proxy\"\ntimeouts:\n  api: 10000\n  global: 10000\n  login: 5000\n  healthCheck: 5000\n  healthInterval: 60000\nproxies: []\nupstream:\n  - name: \"U1\"\n    url: %q\n    username: \"u\"\n    password: \"p\"\n", upstream.URL)

	withTempAppConfig(t, cfg, func(app *App, handler http.Handler) {
		vid := app.IDStore.GetOrCreateVirtualID("orig-head", app.Upstream.Clients()[0].ID)

		req := httptest.NewRequest(http.MethodHead, "/Items/"+vid+"/Images/Primary?tag=h1", nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Fatalf("HEAD status = %d, want 200", rr.Code)
		}
		if rr.Body.Len() != 0 {
			t.Fatalf("HEAD body len = %d, want 0", rr.Body.Len())
		}
		if got, _ := seenMethod.Load().(string); got != http.MethodHead {
			t.Fatalf("upstream method = %q, want HEAD", got)
		}
		if got := rr.Header().Get("Cache-Control"); got != "public, max-age=2592000, immutable" {
			t.Fatalf("HEAD Cache-Control = %q", got)
		}
	})
}

func TestPerfImageSingleflightCoalesceAndCancelIsolation(t *testing.T) {
	imgBytes := []byte("coalesced-image-body")
	var hits atomic.Int32
	gate := make(chan struct{})
	started := make(chan struct{}, 10)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "tok-1", "User": map[string]any{"Id": "u-1"}})
		case r.Method == http.MethodGet && r.URL.Path == "/Items/orig-sf/Images/Primary":
			hits.Add(1)
			started <- struct{}{}
			<-gate
			w.Header().Set("Content-Type", "image/png")
			w.Header().Set("Content-Length", fmt.Sprintf("%d", len(imgBytes)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(imgBytes)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	cfg := fmt.Sprintf("server:\n  port: 8096\n  name: \"Test\"\n  id: \"srv-1\"\nadmin:\n  username: \"admin\"\n  password: \"secret\"\nplayback:\n  mode: \"proxy\"\ntimeouts:\n  api: 10000\n  global: 10000\n  login: 5000\n  healthCheck: 5000\n  healthInterval: 60000\nproxies: []\nupstream:\n  - name: \"U1\"\n    url: %q\n    username: \"u\"\n    password: \"p\"\n", upstream.URL)

	withTempAppConfig(t, cfg, func(app *App, handler http.Handler) {
		vid := app.IDStore.GetOrCreateVirtualID("orig-sf", app.Upstream.Clients()[0].ID)

		// 1) 5 concurrent requests coalesce into 1 upstream hit.
		const n = 5
		var wg sync.WaitGroup
		recorders := make([]*httptest.ResponseRecorder, n)
		for i := 0; i < n; i++ {
			recorders[i] = httptest.NewRecorder()
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				req := httptest.NewRequest(http.MethodGet, "/Items/"+vid+"/Images/Primary?tag=sf1", nil)
				handler.ServeHTTP(recorders[idx], req)
			}(i)
		}
		<-started
		time.Sleep(40 * time.Millisecond)
		close(gate)
		wg.Wait()

		if got := hits.Load(); got != 1 {
			t.Fatalf("concurrent upstream hits = %d, want 1", got)
		}
		for i, rr := range recorders {
			if rr.Code != http.StatusOK || rr.Body.String() != string(imgBytes) {
				t.Fatalf("recorder[%d] = status %d body %q", i, rr.Code, rr.Body.String())
			}
		}

		// 2) After completion, strictly zero cache: next request hits upstream again.
		rrAfter := httptest.NewRecorder()
		handler.ServeHTTP(rrAfter, httptest.NewRequest(http.MethodGet, "/Items/"+vid+"/Images/Primary?tag=sf1", nil))
		if rrAfter.Code != http.StatusOK || hits.Load() != 2 {
			t.Fatalf("post-flight hit count = %d, status = %d, want 2/200", hits.Load(), rrAfter.Code)
		}

		// 3) Leader cancellation must not cancel waiter B.
		gate = make(chan struct{})
		leaderCtx, cancelLeader := context.WithCancel(context.Background())
		var wg2 sync.WaitGroup
		wg2.Add(2)
		go func() {
			defer wg2.Done()
			reqA := httptest.NewRequest(http.MethodGet, "/Items/"+vid+"/Images/Primary?tag=sf2", nil).WithContext(leaderCtx)
			handler.ServeHTTP(httptest.NewRecorder(), reqA)
		}()
		<-started
		rrB := httptest.NewRecorder()
		go func() {
			defer wg2.Done()
			reqB := httptest.NewRequest(http.MethodGet, "/Items/"+vid+"/Images/Primary?tag=sf2", nil)
			handler.ServeHTTP(rrB, reqB)
		}()
		time.Sleep(40 * time.Millisecond)
		cancelLeader()
		time.Sleep(20 * time.Millisecond)
		close(gate)
		wg2.Wait()

		if rrB.Code != http.StatusOK || rrB.Body.String() != string(imgBytes) {
			t.Fatalf("waiter B failed after leader cancel: status=%d body=%q", rrB.Code, rrB.Body.String())
		}
	})
}

func TestPerfIDStoreConcurrentReadWrite(t *testing.T) {
	store, err := NewIDStore(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewIDStore: %v", err)
	}
	defer store.Close()

	baseVID := store.GetOrCreateVirtualID("orig-0", "srv-a")
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				orig := fmt.Sprintf("orig-%d-%d", id, i)
				vid := store.GetOrCreateVirtualID(orig, "srv-a")
				resolved := store.ResolveVirtualID(vid)
				if resolved == nil || resolved.OriginalID != orig {
					t.Errorf("resolve mismatch for %s: %#v", orig, resolved)
					return
				}
				if got := store.ResolveVirtualID(baseVID); got == nil || got.OriginalID != "orig-0" {
					t.Errorf("base resolve mismatch: %#v", got)
					return
				}
			}
		}(g)
	}
	wg.Wait()
}

func TestPerfPlaybackInfoEarlyReturn(t *testing.T) {
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "tok-fast", "User": map[string]any{"Id": "u-fast"}})
		case r.URL.Path == "/Items/item-fast/PlaybackInfo":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"PlaySessionId": "ps-fast",
				"MediaSources": []map[string]any{{
					"Id":               "src-fast",
					"Path":             "/media/fast.mkv",
					"SupportsDirectPlay": true,
				}},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer fast.Close()

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/Users/AuthenticateByName":
			_ = json.NewEncoder(w).Encode(map[string]any{"AccessToken": "tok-slow", "User": map[string]any{"Id": "u-slow"}})
		case r.URL.Path == "/Items/item-slow/PlaybackInfo":
			select {
			case <-time.After(5 * time.Second):
			case <-r.Context().Done():
			}
			w.WriteHeader(http.StatusGatewayTimeout)
		default:
			http.NotFound(w, r)
		}
	}))
	defer slow.Close()

	cfg := fmt.Sprintf("server:\n  port: 8096\n  name: \"Test\"\n  id: \"srv-1\"\nadmin:\n  username: \"admin\"\n  password: \"secret\"\nplayback:\n  mode: \"proxy\"\ntimeouts:\n  api: 10000\n  global: 10000\n  login: 5000\n  healthCheck: 5000\n  healthInterval: 60000\nproxies: []\nupstream:\n  - name: \"Fast\"\n    url: %q\n    username: \"u1\"\n    password: \"p1\"\n  - name: \"Slow\"\n    url: %q\n    username: \"u2\"\n    password: \"p2\"\n", fast.URL, slow.URL)

	withTempAppConfig(t, cfg, func(app *App, handler http.Handler) {
		token := loginToken(t, handler, "secret")
		vid := app.IDStore.GetOrCreateVirtualID("item-fast", app.Upstream.Clients()[0].ID)
		app.IDStore.AssociateAdditionalInstance(vid, "item-slow", app.Upstream.Clients()[1].ID)

		start := time.Now()
		rr := doJSONRequest(t, handler, http.MethodPost, "/Items/"+vid+"/PlaybackInfo", map[string]any{}, token)
		elapsed := time.Since(start)

		if rr.Code != http.StatusOK {
			t.Fatalf("PlaybackInfo status = %d, body = %s", rr.Code, rr.Body.String())
		}
		if elapsed > 2*time.Second {
			t.Fatalf("PlaybackInfo took %v, expected early return well before 5s slow upstream", elapsed)
		}
	})
}
func TestPerfStreamProxySinglePassM3U8Rewrite(t *testing.T) {
	playlist := strings.Join([]string{
		"#EXTM3U",
		"#EXT-X-VERSION:3",
		"#EXTINF:6.0,",
		"seg001.ts?api_key=upstream-secret&PlaySessionId=abc",
		"#EXTINF:6.0,",
		"https://upstream.example.com/emby/Videos/99/seg002.ts?ApiKey=upstream-secret",
		"#EXT-X-ENDLIST",
	}, "\n")

	rewritten := RewriteM3U8ForItemWithSource(playlist, "https://upstream.example.com/emby/Videos/99/", "virtual-123", "proxy-tok-xyz", "srv-1::src-1")

	if strings.Contains(rewritten, "upstream-secret") {
		t.Fatalf("rewritten playlist leaked upstream token:\n%s", rewritten)
	}
	if !strings.Contains(rewritten, "#EXTM3U") || !strings.Contains(rewritten, "#EXT-X-ENDLIST") {
		t.Fatalf("rewritten playlist missing HLS directives:\n%s", rewritten)
	}
	if !strings.Contains(rewritten, "/Videos/virtual-123/seg001.ts") || !strings.Contains(rewritten, "/Videos/virtual-123/seg002.ts") {
		t.Fatalf("rewritten playlist missing rewritten virtual item paths:\n%s", rewritten)
	}
	if !strings.Contains(rewritten, "api_key=proxy-tok-xyz") {
		t.Fatalf("rewritten playlist missing proxy token:\n%s", rewritten)
	}
	if !strings.Contains(rewritten, "MediaSourceId=srv-1%3A%3Asrc-1") {
		t.Fatalf("rewritten playlist missing pinned qualified MediaSourceId:\n%s", rewritten)
	}
}
