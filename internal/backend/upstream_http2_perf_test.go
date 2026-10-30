package backend

import (
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSharedUpstreamTransportNegotiatesHTTP2(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Upstream-Protocol", r.Proto)
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = io.WriteString(w, "sample-image-bytes")
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	transport := sharedTransport.Clone()
	defer transport.CloseIdleConnections()
	transport.TLSClientConfig = &tls.Config{RootCAs: srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs}

	resp, err := (&http.Client{Transport: transport}).Get(srv.URL + "/Items/demo/Images/Primary")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.ProtoMajor != 2 || resp.Header.Get("X-Upstream-Protocol") != "HTTP/2.0" {
		t.Fatalf("upstream negotiated %s (origin %s), want HTTP/2", resp.Proto, resp.Header.Get("X-Upstream-Protocol"))
	}
	if string(body) != "sample-image-bytes" || resp.Header.Get("Content-Type") != "image/jpeg" {
		t.Fatalf("image changed during HTTP/2 transfer: content-type=%q body=%q", resp.Header.Get("Content-Type"), body)
	}
	if !transport.DisableCompression {
		t.Fatal("must preserve DisableCompression to avoid upstream Content-Length mismatch")
	}
}

func TestSharedUpstreamTransportFallsBackToHTTP11(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Upstream-Protocol", r.Proto)
		_, _ = io.WriteString(w, "plain-v1")
	}))
	defer srv.Close()

	transport := sharedTransport.Clone()
	defer transport.CloseIdleConnections()
	transport.TLSClientConfig = &tls.Config{RootCAs: srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs}
	resp, err := (&http.Client{Transport: transport}).Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.ProtoMajor != 1 || resp.Header.Get("X-Upstream-Protocol") != "HTTP/1.1" {
		t.Fatalf("HTTP/1-only server negotiated %s, want HTTP/1.1", resp.Proto)
	}
}

func TestProxyUpstreamTransportAttemptsHTTP2(t *testing.T) {
	transport, ok := buildProxyTransport("http://127.0.0.1:10001", nil, "test")
	if !ok {
		t.Fatal("valid HTTP proxy rejected")
	}
	tr, ok := transport.(*http.Transport)
	if !ok {
		t.Fatalf("unexpected proxy transport type %T", transport)
	}
	defer tr.CloseIdleConnections()
	if !tr.ForceAttemptHTTP2 || !tr.DisableCompression {
		t.Fatalf("proxy transport must attempt HTTP/2 without changing compression: h2=%v compressionDisabled=%v", tr.ForceAttemptHTTP2, tr.DisableCompression)
	}
}

// The transport is shared by image and video APIs. Enabling h2 must not
// silently strip the video Range header or change the HEAD response semantics.
func TestSharedUpstreamTransportPreservesRangeAndHeadOverHTTP2(t *testing.T) {
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			t.Errorf("upstream protocol = %s, want HTTP/2", r.Proto)
		}
		switch r.Method {
		case http.MethodGet:
			if got := r.Header.Get("Range"); got != "bytes=0-3" {
				t.Errorf("Range = %q, want bytes=0-3", got)
			}
			w.Header().Set("Content-Range", "bytes 0-3/8")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = io.WriteString(w, "ABCD")
		case http.MethodHead:
			w.Header().Set("Content-Length", "8")
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	transport := sharedTransport.Clone()
	defer transport.CloseIdleConnections()
	transport.TLSClientConfig = &tls.Config{RootCAs: srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs}
	client := &http.Client{Transport: transport}

	getReq, err := http.NewRequest(http.MethodGet, srv.URL+"/Videos/demo/stream.mkv", nil)
	if err != nil {
		t.Fatal(err)
	}
	getReq.Header.Set("Range", "bytes=0-3")
	getResp, err := client.Do(getReq)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(getResp.Body)
	getResp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if getResp.StatusCode != http.StatusPartialContent ||
		getResp.Header.Get("Content-Range") != "bytes 0-3/8" || string(data) != "ABCD" {
		t.Fatalf("Range response: status=%d content-range=%q bytes=%q",
			getResp.StatusCode, getResp.Header.Get("Content-Range"), data)
	}
	headReq, err := http.NewRequest(http.MethodHead, srv.URL+"/Videos/demo/stream.mkv", nil)
	if err != nil {
		t.Fatal(err)
	}
	headResp, err := client.Do(headReq)
	if err != nil {
		t.Fatal(err)
	}
	defer headResp.Body.Close()
	if headResp.StatusCode != http.StatusOK || headResp.ContentLength != 8 {
		t.Fatalf("HEAD response: status=%d ContentLength=%d", headResp.StatusCode, headResp.ContentLength)
	}
}
