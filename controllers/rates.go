package controllers

import "strings"

// GPURates holds the operator-wide USD-per-GPU-hour rate for each supported
// GPU type, set once via the operator's --gpu-rate-* flags (see main.go).
// There is no per-namespace override - rates are a finance/org decision, not
// something individual namespaces should set for themselves.
type GPURates struct {
	A100 float64
	H100 float64
	V100 float64
}

// RateFor returns the USD-per-GPU-hour rate for gpuType (case-insensitive)
// and whether it's configured. A zero/unset rate is treated as "not
// configured" rather than "free" - a GpuQuota with spec.dollarsLimit whose
// usage includes an unpriced type fails loudly instead of silently
// undercounting cost.
func (r GPURates) RateFor(gpuType string) (float64, bool) {
	switch strings.ToUpper(gpuType) {
	case "A100":
		return r.A100, r.A100 > 0
	case "H100":
		return r.H100, r.H100 > 0
	case "V100":
		return r.V100, r.V100 > 0
	default:
		return 0, false
	}
}
