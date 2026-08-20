package controllers

import (
	"strings"

	gpubudgetv1alpha1 "github.com/gsanders/gpu-budget-operator/v1alpha1"
)

// GPURates maps a GPU family name (e.g. "A100", "H100") to its USD-per-
// GPU-hour rate - keys are stored upper-cased. There is no per-namespace
// override - rates are a finance/org decision, not something individual
// namespaces should set for themselves.
//
// Using a map instead of one struct field per family means adding a rate
// for a new GPU type, or changing an existing one, is purely a
// GpuBudgetOperatorConfig edit - it never requires touching this file or
// rebuilding the operator.
type GPURates map[string]float64

// ratesFromSpec converts a GpuBudgetOperatorConfig's spec.gpuRates list into
// a GPURates lookup table. A later entry for the same family (case-
// insensitively) overwrites an earlier one, matching how a repeated
// --gpu-rate flag used to behave.
func ratesFromSpec(entries []gpubudgetv1alpha1.GPURate) GPURates {
	rates := make(GPURates, len(entries))
	for _, e := range entries {
		rates[strings.ToUpper(e.Family)] = e.DollarsPerGPUHour
	}
	return rates
}

// RateFor returns the USD-per-GPU-hour rate for gpuType and whether it's
// configured. A zero/unset rate is treated as "not configured" rather than
// "free" - a GpuBudget with spec.dollarsLimit whose usage includes an
// unpriced type fails loudly instead of silently undercounting cost.
//
// Matches by family prefix, not exact equality: the default GPU-hours
// query's gpuType label comes from label_replace(..., "product",
// "NVIDIA-(.+)") against a node's full product label (e.g.
// "NVIDIA-A100-SXM4-80GB"), so gpuType is typically a full SKU like
// "A100-SXM4-80GB", not the bare family name "A100" a rate is configured
// under. If more than one configured family prefixes gpuType, the longest
// one wins, so a more specific family name (e.g. "A100-SXM4" alongside
// "A100") isn't shadowed by a shorter, less specific one.
func (r GPURates) RateFor(gpuType string) (float64, bool) {
	gpuType = strings.ToUpper(gpuType)
	var bestFamily string
	var bestRate float64
	for family, rate := range r {
		if rate <= 0 {
			continue
		}
		family = strings.ToUpper(family)
		if strings.HasPrefix(gpuType, family) && len(family) > len(bestFamily) {
			bestFamily, bestRate = family, rate
		}
	}
	return bestRate, bestFamily != ""
}
