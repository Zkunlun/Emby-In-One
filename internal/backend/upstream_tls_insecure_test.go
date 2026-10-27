package backend

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAllUpstreamTransportsSkipTLSVerification(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer origin.Close()

	client := newUpstreamClient(Config{}, UpstreamConfig{Name: "untrusted-cert", URL: origin.URL}, 0, nil)
	transport, ok := client.transport.(*http.Transport)
	if !ok || transport.TLSClientConfig == nil || !transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("ordinary upstream HTTP transport must skip TLS certificate verification")
	}
	response, err := client.httpClient.Get(origin.URL + "/")
	if err != nil {
		t.Fatalf("upstream with untrusted test certificate should be reachable: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("unexpected response status: %d", response.StatusCode)
	}

	proxied, ok := buildProxyTransport("http://127.0.0.1:8888", nil, "test")
	if !ok {
		t.Fatal("proxy transport was not created")
	}
	proxyTransport, ok := proxied.(*http.Transport)
	if !ok || proxyTransport.TLSClientConfig == nil || !proxyTransport.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("proxied upstream HTTP transport must also skip TLS certificate verification")
	}
}
