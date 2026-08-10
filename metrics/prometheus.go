// Package metrics queries Prometheus (fed by dcgm-exporter) to determine how
// many GPUs are currently active in a given namespace.
package metrics

import (
	"context"
	"fmt"
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

// NewClient builds a Prometheus client for the given base URL (e.g.
// "http://prometheus-k8s.monitoring.svc:9090").
func NewClient(address string) (*Client, error) {
	c, err := promapi.NewClient(promapi.Config{Address: address})
	if err != nil {
		return nil, fmt.Errorf("creating prometheus client for %q: %w", address, err)
	}
	return &Client{api: promv1.NewAPI(c)}, nil
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
