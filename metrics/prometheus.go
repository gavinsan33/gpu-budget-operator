// Package metrics queries Prometheus to determine cumulative GPU-hours
// consumed, broken out by GPU type, for a namespace's current billing
// period.
package metrics

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	promapi "github.com/prometheus/client_golang/api"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
)

// DefaultGPUHoursQueryTemplate computes cumulative GPU-hours consumed by
// namespace __NAMESPACE__ since the period started (__RANGE__ ago), broken
// out per GPU type, based on GPU *requests* (reservation time) rather than
// utilization - matching typical GPU-cluster billing methodology (you're
// billed for what you reserved, not what you used).
//
// This assumes kube-state-metrics exposes kube_pod_resource_request and
// kube_node_labels with the node's GPU product allow-listed under
// "nvidia.com/gpu.product" (the label NVIDIA's GPU Operator / node feature
// discovery commonly sets) - VERIFY this matches your cluster before
// relying on it, and override via spec.query if not. The GPU-hours
// computation itself (avg reservation count over the range * range length
// in hours) is resolution-independent - it doesn't assume any particular
// Prometheus scrape/recording interval.
const DefaultGPUHoursQueryTemplate = `label_replace(
  sum by (product) (
    avg_over_time(
      (
        kube_pod_resource_request{resource=~"nvidia.com/.+", namespace="__NAMESPACE__"}
        * on(node) group_left(product) label_replace(kube_node_labels, "product", "$1", "label_nvidia_com_gpu_product", "(.+)")
      )[__RANGE__:5m]
    )
  ) * __RANGE_HOURS__,
  "gpuType", "$1", "product", "NVIDIA-(.+)"
)`

// namespacePlaceholder, rangePlaceholder, and rangeHoursPlaceholder are
// substituted into query templates by BuildGPUHoursQuery.
const (
	namespacePlaceholder  = "__NAMESPACE__"
	rangePlaceholder      = "__RANGE__"
	rangeHoursPlaceholder = "__RANGE_HOURS__"
)

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

// BuildGPUHoursQuery renders a GPU-hours-by-type query template for the
// given namespace and elapsed time since the period started, falling back
// to DefaultGPUHoursQueryTemplate when template is empty. elapsed is
// clamped to at least one minute to avoid a degenerate zero-length PromQL
// range right at a period boundary.
func BuildGPUHoursQuery(template, namespace string, elapsed time.Duration) string {
	if template == "" {
		template = DefaultGPUHoursQueryTemplate
	}
	if elapsed < time.Minute {
		elapsed = time.Minute
	}
	q := strings.ReplaceAll(template, namespacePlaceholder, namespace)
	q = strings.ReplaceAll(q, rangePlaceholder, model.Duration(elapsed).String())
	q = strings.ReplaceAll(q, rangeHoursPlaceholder, strconv.FormatFloat(elapsed.Hours(), 'f', -1, 64))
	return q
}

// GPUHoursByType runs query and returns cumulative GPU-hours consumed so
// far, keyed by the "gpuType" label of each result sample. Samples with no
// "gpuType" label are skipped, since there's no rate to attribute them to.
func (c *Client) GPUHoursByType(ctx context.Context, query string) (map[string]float64, error) {
	value, warnings, err := c.api.Query(ctx, query, time.Now())
	if err != nil {
		return nil, fmt.Errorf("querying prometheus: %w", err)
	}
	for _, w := range warnings {
		_ = w // surfaced via logging by the caller if desired
	}

	vector, ok := value.(model.Vector)
	if !ok {
		return nil, fmt.Errorf("unexpected prometheus result type %T for query %q (expected vector)", value, query)
	}

	hoursByType := make(map[string]float64, len(vector))
	for _, sample := range vector {
		gpuType := string(sample.Metric["gpuType"])
		if gpuType == "" {
			continue
		}
		hoursByType[gpuType] += float64(sample.Value)
	}
	return hoursByType, nil
}
