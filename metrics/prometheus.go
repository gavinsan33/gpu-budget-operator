// Package metrics queries Prometheus (fed by dcgm-exporter) to determine how
// many GPUs are currently active in a given namespace.
package metrics

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	promapi "github.com/prometheus/client_golang/api"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
)

// DefaultQueryTemplate counts distinct GPUs (by UUID) reporting non-zero
// utilization within the namespace. It assumes dcgm-exporter is deployed
// with pod-resource mapping enabled, so DCGM_FI_DEV_GPU_UTIL samples carry a
// "namespace" label. __NAMESPACE__ is substituted with the target namespace.
const DefaultQueryTemplate = `count(max by (UUID) (DCGM_FI_DEV_GPU_UTIL{namespace="__NAMESPACE__"} > 0))`

// namespacePlaceholder is substituted into query templates.
const namespacePlaceholder = "__NAMESPACE__"

// Client queries Prometheus for per-namespace GPU usage.
type Client struct {
	api promv1.API
}

// serviceCACertFile is the path the operator's Deployment mounts an
// OpenShift-injected service-serving CA bundle to (see
// manager/deploy/service-ca-configmap.yaml and manager/deploy/deployment.yaml).
// serviceAccountTokenFile is the path Kubernetes auto-mounts a bearer token
// into every pod at. Neither has a flag or Config field to override -
// trusting/sending them, when present, happens automatically. If either
// file is missing (e.g. running outside a cluster, or against a plain-HTTP
// dev Prometheus that doesn't require auth), the client falls back to no
// custom CA / no Authorization header respectively, rather than erroring.
var (
	serviceCACertFile       = "/etc/gpu-quota-operator/service-ca/service-ca.crt"
	serviceAccountTokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token"
)

// Config configures which Prometheus a Client talks to. TLS trust and
// Bearer-token auth are always automatic (see serviceCACertFile/
// serviceAccountTokenFile) - there is nothing else to configure per-client.
type Config struct {
	// Address is the base URL of the Prometheus/Thanos endpoint, e.g.
	// "https://thanos-querier.openshift-monitoring.svc:9091".
	Address string
}

// NewClient builds a Prometheus client from cfg.
func NewClient(cfg Config) (*Client, error) {
	transport, err := buildTransport()
	if err != nil {
		return nil, fmt.Errorf("building transport for %q: %w", cfg.Address, err)
	}
	c, err := promapi.NewClient(promapi.Config{Address: cfg.Address, RoundTripper: transport})
	if err != nil {
		return nil, fmt.Errorf("creating prometheus client for %q: %w", cfg.Address, err)
	}
	return &Client{api: promv1.NewAPI(c)}, nil
}

func buildTransport() (http.RoundTripper, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()

	pemBytes, err := os.ReadFile(serviceCACertFile)
	switch {
	case err == nil:
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pemBytes) {
			return nil, fmt.Errorf("no PEM certificates found in %q", serviceCACertFile)
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: pool}
	case os.IsNotExist(err):
		// Not mounted - fall back to the system trust store.
	default:
		return nil, fmt.Errorf("reading service CA bundle %q: %w", serviceCACertFile, err)
	}

	return &bearerTokenRoundTripper{base: transport}, nil
}

// bearerTokenRoundTripper attaches a Bearer token read fresh from
// serviceAccountTokenFile on every request, since Kubernetes rotates
// projected ServiceAccount tokens in place roughly hourly - caching the
// token at startup would cause auth to silently start failing well into the
// operator's lifetime. If the token file isn't present at all (e.g. running
// outside a pod), requests go out with no Authorization header rather than
// failing outright.
type bearerTokenRoundTripper struct {
	base http.RoundTripper
}

func (t *bearerTokenRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	token, err := os.ReadFile(serviceAccountTokenFile)
	switch {
	case err == nil:
		req = req.Clone(req.Context())
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	case os.IsNotExist(err):
		// Not running in a pod - send the request unauthenticated.
	default:
		return nil, fmt.Errorf("reading bearer token file %q: %w", serviceAccountTokenFile, err)
	}
	return t.base.RoundTrip(req)
}

// BuildQuery renders a query template for the given namespace, falling back
// to DefaultQueryTemplate when template is empty.
func BuildQuery(template, namespace string) string {
	if template == "" {
		template = DefaultQueryTemplate
	}
	return strings.ReplaceAll(template, namespacePlaceholder, namespace)
}

// ActiveGPUCount runs the query and returns the number of active GPUs as
// reported by the first sample in the result vector. Returns 0 with no error
// if the query yields no samples (i.e. no GPU activity in the namespace).
func (c *Client) ActiveGPUCount(ctx context.Context, query string) (int32, error) {
	value, warnings, err := c.api.Query(ctx, query, time.Now())
	if err != nil {
		return 0, fmt.Errorf("querying prometheus: %w", err)
	}
	for _, w := range warnings {
		_ = w // surfaced via logging by the caller if desired
	}

	switch v := value.(type) {
	case model.Vector:
		if len(v) == 0 {
			return 0, nil
		}
		return int32(v[0].Value), nil
	case *model.Scalar:
		return int32(v.Value), nil
	default:
		return 0, fmt.Errorf("unexpected prometheus result type %T for query %q", value, query)
	}
}
