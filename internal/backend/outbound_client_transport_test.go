package backend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

func TestOutboundClientTransportProxyWire(t *testing.T) {
	upstreamRecords := make(chan outboundClientHeaderRecord, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamRecords <- recordClientHeaders(r)
		if r.URL.Path == "/System/Info" {
			_, _ = io.WriteString(w, "{}")
			return
		}
		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Content-Range", "bytes 0-3/8")
		w.Header().Set("Content-Length", "4")
		w.WriteHeader(http.StatusPartialContent)
		if r.Method != http.MethodHead {
			_, _ = io.WriteString(w, "DATA")
		}
	}))
	defer upstream.Close()
	proxyRecords := make(chan outboundClientHeaderRecord, 1)
	proxyTargets := make(chan string, 1)
	relayTransport := &http.Transport{}
	defer relayTransport.CloseIdleConnections()
	relay := &http.Client{Transport: relayTransport}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyRecords <- recordClientHeaders(r)
		proxyTargets <- r.URL.Host
		outbound := r.Clone(r.Context())
		outbound.RequestURI = ""
		outbound.Header = r.Header.Clone()
		outbound.Header.Del("Proxy-Authorization")
		response, err := relay.Do(outbound)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer response.Body.Close()
		for key, values := range response.Header {
			for _, v := range values {
				w.Header().Add(key, v)
			}
		}
		w.WriteHeader(response.StatusCode)
		_, _ = io.Copy(w, response.Body)
	}))
	defer proxy.Close()
	proxyURL, _ := url.Parse(proxy.URL)
	proxyURL.User = url.UserPassword("node-user", "node-pass")
	cfg := Config{Playback: PlaybackConfig{Mode: "proxy"}, Proxies: []ProxyConfig{{ID: "node", Name: "test", URL: proxyURL.String()}}}
	client := newUpstreamClient(cfg, UpstreamConfig{URL: upstream.URL, SpoofClient: "infuse", ProxyID: "node", FollowRedirects: true}, 0, nil)
	client.setOnline("UPSTREAM-TOKEN", "UPSTREAM-USER")
	if transport, ok := client.transport.(*http.Transport); ok {
		defer transport.CloseIdleConnections()
	}
	for _, tc := range []struct {
		name, method, path string
		stream             bool
	}{
		{"api", http.MethodGet, "/System/Info", false},
		{"video-range", http.MethodGet, "/Videos/item/stream.mp4", true},
		{"head-outlet", http.MethodHead, "/Videos/item/stream.mp4", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			params := queryIdentityConflicts()
			before := copyValues(params)
			extra := realClientConflictHeaders()
			extra.Set("Range", "bytes=0-3")
			headersBefore := extra.Clone()
			response, err := client.doRequest(context.Background(), nil, tc.method, tc.path, params, nil, extra, tc.stream)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(response.Body)
			_ = response.Body.Close()
			wantStatus, wantBody := http.StatusOK, "{}"
			if tc.stream {
				wantStatus = http.StatusPartialContent
				wantBody = "DATA"
			}
			if tc.method == http.MethodHead {
				wantBody = ""
			}
			if response.StatusCode != wantStatus || string(body) != wantBody {
				t.Fatalf("response changed: %d %q", response.StatusCode, body)
			}
			if tc.stream && response.Header.Get("Content-Range") != "bytes 0-3/8" {
				t.Fatal("Content-Range changed")
			}
			atProxy, atUpstream := <-proxyRecords, <-upstreamRecords
			want := infuseClientHeaderWant()
			if tc.stream {
				want["DeviceId"] = "infuse-spoof-id"
			}
			for _, record := range []outboundClientHeaderRecord{atProxy, atUpstream} {
				assertClientHeaders(t, record.headers, want, "UPSTREAM-USER", "UPSTREAM-TOKEN")
				assertClientQuery(t, record.query, want)
				if record.headers.Get("Range") != "bytes=0-3" {
					t.Fatal("Range changed")
				}
			}
			authRequest := &http.Request{Header: http.Header{}}
			authRequest.SetBasicAuth("node-user", "node-pass")
			if atProxy.headers.Get("Proxy-Authorization") != authRequest.Header.Get("Authorization") {
				t.Fatal("proxy-node credentials changed")
			}
			target, _ := url.Parse(upstream.URL)
			if <-proxyTargets != target.Host {
				t.Fatal("proxy target changed")
			}
			if !reflect.DeepEqual(params, before) || !reflect.DeepEqual(extra, headersBefore) {
				t.Fatal("source params/headers mutated")
			}
		})
	}
}

func TestOutboundClientTransportFollowRedirects(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, follow := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%v/follow=%v", stream, follow), func(t *testing.T) {
				initial := make(chan outboundClientHeaderRecord, 1)
				final := make(chan outboundClientHeaderRecord, 1)
				stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/final" {
						final <- recordClientHeaders(r)
						_, _ = io.WriteString(w, "FINAL")
						return
					}
					initial <- recordClientHeaders(r)
					http.Redirect(w, r, "/final?serverValue=keep", http.StatusFound)
				}))
				defer stub.Close()
				client := newUpstreamClient(Config{}, UpstreamConfig{URL: stub.URL, SpoofClient: "infuse", FollowRedirects: follow}, 0, nil)
				client.setOnline("UPSTREAM-TOKEN", "UPSTREAM-USER")
				params := queryIdentityConflicts()
				before := copyValues(params)
				path := "/System/Info"
				if stream {
					path = "/Videos/item/stream.mp4"
				}
				response, err := client.doRequest(context.Background(), nil, http.MethodGet, path, params, nil, realClientConflictHeaders(), stream)
				got := <-initial
				want := infuseClientHeaderWant()
				if stream {
					want["DeviceId"] = "infuse-spoof-id"
				}
				assertClientHeaders(t, got.headers, want, "UPSTREAM-USER", "UPSTREAM-TOKEN")
				assertClientQuery(t, got.query, want)
				if follow {
					if err != nil {
						t.Fatal(err)
					}
					body, _ := io.ReadAll(response.Body)
					_ = response.Body.Close()
					if response.StatusCode != http.StatusOK || string(body) != "FINAL" {
						t.Fatal("followed response changed")
					}
					redirected := <-final
					// Server-owned redirect URLs remain under the existing HTTP redirect policy.
					// This task does not rebuild their queries or add a CDN per-hop policy.
					if redirected.query.Get("serverValue") != "keep" || len(redirected.query) != 1 {
						t.Fatal("server redirect URL changed")
					}
				} else {
					if response != nil {
						_ = response.Body.Close()
					}
					if err == nil {
						t.Fatal("FollowRedirects=false no longer rejects redirects")
					}
					select {
					case <-final:
						t.Fatal("refused redirect reached target")
					default:
					}
				}
				if !reflect.DeepEqual(params, before) {
					t.Fatal("source query mutated")
				}
			})
		}
	}
}

func TestOutboundClientTransportCancellation(t *testing.T) {
	started := make(chan outboundClientHeaderRecord, 1)
	stopped := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- recordClientHeaders(r)
		<-r.Context().Done()
		close(stopped)
	}))
	defer upstream.Close()
	var fallbackHits atomic.Int32
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackHits.Add(1)
		_, _ = io.WriteString(w, "unexpected fallback")
	}))
	defer fallback.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := newUpstreamClient(Config{}, UpstreamConfig{URL: upstream.URL, StreamingURLs: []string{upstream.URL, fallback.URL}, SpoofClient: "infuse", FollowRedirects: true}, 0, nil)
	client.setOnline("UPSTREAM-TOKEN", "UPSTREAM-USER")
	params := queryIdentityConflicts()
	before := copyValues(params)
	result := make(chan error, 1)
	go func() {
		response, err := client.Stream(ctx, nil, nil, "/Videos/item/stream.mp4", params, realClientConflictHeaders())
		if response != nil {
			_ = response.Body.Close()
		}
		result <- err
	}()
	select {
	case got := <-started:
		want := infuseClientHeaderWant()
		want["DeviceId"] = "infuse-spoof-id"
		assertClientHeaders(t, got.headers, want, "UPSTREAM-USER", "UPSTREAM-TOKEN")
		assertClientQuery(t, got.query, want)
	case <-time.After(3 * time.Second):
		t.Fatal("stream did not reach upstream")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel error=%v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stream request did not cancel")
	}
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream request context did not cancel")
	}
	if fallbackHits.Load() != 0 || !reflect.DeepEqual(params, before) {
		t.Fatal("cancellation triggered fallback or mutated source")
	}
}
