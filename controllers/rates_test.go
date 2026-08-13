package controllers

import "testing"

// TestGPURates_RateFor_MatchesFullSKUsByFamilyPrefix reproduces the bug
// found running against a real cluster: the default GPU-hours query
// extracts gpuType from a node's full product label (e.g.
// "NVIDIA-A100-SXM4-80GB"), so gpuType is typically a full SKU like
// "A100-SXM4-80GB", not the bare family name "A100". An exact-match
// RateFor left every such SKU permanently reported as unpriced regardless
// of --gpu-rate=A100=<usd> being set.
func TestGPURates_RateFor_MatchesFullSKUsByFamilyPrefix(t *testing.T) {
	rates := GPURates{"A100": 1.70, "H100": 1.10, "V100": 0.30}

	cases := []struct {
		gpuType   string
		wantRate  float64
		wantFound bool
	}{
		{"A100", 1.70, true},
		{"A100-SXM4-80GB", 1.70, true},
		{"a100-sxm4-80gb", 1.70, true}, // case-insensitive
		{"H100", 1.10, true},
		{"H100-PCIE-80GB", 1.10, true},
		{"V100", 0.30, true},
		{"V100-SXM2-32GB", 0.30, true},
		{"T4", 0, false},
		{"", 0, false},
		// Must not prefix-match a different family that happens to share a
		// substring.
		{"A10", 0, false},
	}
	for _, c := range cases {
		rate, found := rates.RateFor(c.gpuType)
		if rate != c.wantRate || found != c.wantFound {
			t.Errorf("RateFor(%q) = (%v, %v), want (%v, %v)", c.gpuType, rate, found, c.wantRate, c.wantFound)
		}
	}
}

// TestGPURates_RateFor_UnconfiguredRateIsNotFound confirms a zero/unset
// rate is still treated as "not configured" (not "free"), even for a
// family-prefix match.
func TestGPURates_RateFor_UnconfiguredRateIsNotFound(t *testing.T) {
	var rates GPURates // all zero
	if rate, found := rates.RateFor("A100-SXM4-80GB"); found {
		t.Fatalf("expected an unconfigured A100 rate to be not-found, got rate=%v found=%v", rate, found)
	}
}
