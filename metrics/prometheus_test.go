package metrics

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func writeTokenFile(t *testing.T, token string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// withServiceCACertFile points the package-private serviceCACertFile var at
// path for the duration of the test, restoring it afterward. There is
// deliberately no exported way to do this outside of tests - production
// code always reads the fixed, mounted path.
func withServiceCACertFile(t *testing.T, path string) {
	t.Helper()
	original := serviceCACertFile
	serviceCACertFile = path
	t.Cleanup(func() { serviceCACertFile = original })
}

// withServiceAccountTokenFile is the TokenFile equivalent of
// withServiceCACertFile: points the package-private serviceAccountTokenFile
// var at path for the duration of the test.
func withServiceAccountTokenFile(t *testing.T, path string) {
	t.Helper()
	original := serviceAccountTokenFile
	serviceAccountTokenFile = path
	t.Cleanup(func() { serviceAccountTokenFile = original })
}

func writeCAFile(t *testing.T, cert *x509.Certificate) string {
	t.Helper()
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
	path := filepath.Join(t.TempDir(), "service-ca.crt")
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestActiveGPUCount_AutoTrustsMountedServiceCAAndSendsBearerToken(t *testing.T) {
	var gotAuth string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1700000000,"3"]}]}}`)
	}))
	defer server.Close()

	// httptest.NewTLSServer signs with its own self-signed cert; point the
	// (test-only) serviceCACertFile override at it to exercise the same
	// code path that trusts the real mounted service-ca bundle in-cluster.
	withServiceCACertFile(t, writeCAFile(t, server.Certificate()))
	withServiceAccountTokenFile(t, writeTokenFile(t, "test-token"))

	c, err := NewClient(Config{Address: server.URL})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	count, err := c.ActiveGPUCount(context.Background(), "irrelevant")
	if err != nil {
		t.Fatalf("ActiveGPUCount: %v", err)
	}
	if count != 3 {
		t.Fatalf("expected count 3, got %d", count)
	}
	if gotAuth != "Bearer test-token" {
		t.Fatalf("expected Authorization header %q, got %q", "Bearer test-token", gotAuth)
	}
}

func TestActiveGPUCount_MissingServiceCAFallsBackToSystemTrustStore(t *testing.T) {
	// Point at a path that doesn't exist, simulating running outside
	// OpenShift where the service-ca ConfigMap is never mounted. Querying a
	// self-signed TLS server should then fail cert verification, proving
	// the fallback is "use the system pool", not "trust nothing checked" or
	// "trust everything".
	withServiceCACertFile(t, filepath.Join(t.TempDir(), "does-not-exist.crt"))

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
	}))
	defer server.Close()

	c, err := NewClient(Config{Address: server.URL})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := c.ActiveGPUCount(context.Background(), "irrelevant"); err == nil {
		t.Fatal("expected TLS trust failure against a self-signed server with no mounted service CA, got nil error")
	}
}

func TestActiveGPUCount_MissingTokenFileSendsNoAuthHeader(t *testing.T) {
	withServiceAccountTokenFile(t, filepath.Join(t.TempDir(), "does-not-exist-token"))

	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
	}))
	defer server.Close()

	c, err := NewClient(Config{Address: server.URL})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := c.ActiveGPUCount(context.Background(), "irrelevant"); err != nil {
		t.Fatalf("ActiveGPUCount: %v", err)
	}
	if gotAuth != "" {
		t.Fatalf("expected no Authorization header when the token file is absent, got %q", gotAuth)
	}
}
