package control

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
)

// A proxy with HTTP/2 enabled, such as nginx with http2 on, may answer in
// HTTP/2 when the client names no protocol. Transfers must name HTTP/1.1.
func TestTransferOffersHTTP1(t *testing.T) {
	var mu sync.Mutex
	var offered []string
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.EnableHTTP2 = true
	srv.TLS = &tls.Config{GetConfigForClient: func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		mu.Lock()
		defer mu.Unlock()
		offered = hello.SupportedProtos
		return nil, nil
	}}
	srv.StartTLS()
	defer srv.Close()

	tr := transferTransport()
	if tr.TLSClientConfig == nil {
		tr.TLSClientConfig = &tls.Config{}
	}
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	tr.TLSClientConfig.RootCAs = roots
	resp, err := (&http.Client{Transport: tr}).Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(offered, []string{"http/1.1"}) {
		t.Errorf("offered %q, want only http/1.1", offered)
	}
	if resp.Proto != "HTTP/1.1" {
		t.Errorf("spoke %s", resp.Proto)
	}
}
