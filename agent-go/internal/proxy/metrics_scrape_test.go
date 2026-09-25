package proxy

import (
	"math"
	"testing"
)

const expositionA = `# HELP impreza_proxy_requests_total Proxy requests by deployment and HTTP status class.
# TYPE impreza_proxy_requests_total counter
impreza_proxy_requests_total{class="2xx",deployment="dpl_aaaa111122223333"} 40
impreza_proxy_requests_total{class="5xx",deployment="dpl_aaaa111122223333"} 2
impreza_proxy_requests_total{class="2xx",deployment="dpl_bbbb111122223333"} 7
impreza_proxy_latency_ms_bucket{deployment="dpl_aaaa111122223333",le="25"} 10
impreza_proxy_latency_ms_bucket{deployment="dpl_aaaa111122223333",le="100"} 30
impreza_proxy_latency_ms_bucket{deployment="dpl_aaaa111122223333",le="500"} 42
impreza_proxy_latency_ms_bucket{deployment="dpl_aaaa111122223333",le="+Inf"} 42
impreza_proxy_latency_ms_sum{deployment="dpl_aaaa111122223333"} 3100
impreza_proxy_latency_ms_count{deployment="dpl_aaaa111122223333"} 42
impreza_proxy_bytes_in_total{deployment="dpl_aaaa111122223333"} 1024
impreza_proxy_bytes_out_total{deployment="dpl_aaaa111122223333"} 2048
impreza_shield_pow_challenges_total{deployment="dpl_aaaa111122223333"} 3
impreza_shield_pow_passed_total{deployment="dpl_aaaa111122223333"} 3
impreza_shield_ratelimit_rejected_total{deployment="dpl_aaaa111122223333"} 1
caddy_http_requests_total{server="srv0"} 999
`

const expositionB = `impreza_proxy_requests_total{class="2xx",deployment="dpl_aaaa111122223333"} 100
impreza_proxy_requests_total{class="5xx",deployment="dpl_aaaa111122223333"} 5
impreza_proxy_requests_total{class="2xx",deployment="dpl_bbbb111122223333"} 7
impreza_proxy_latency_ms_bucket{deployment="dpl_aaaa111122223333",le="25"} 12
impreza_proxy_latency_ms_bucket{deployment="dpl_aaaa111122223333",le="100"} 70
impreza_proxy_latency_ms_bucket{deployment="dpl_aaaa111122223333",le="500"} 105
impreza_proxy_latency_ms_bucket{deployment="dpl_aaaa111122223333",le="+Inf"} 105
impreza_proxy_latency_ms_sum{deployment="dpl_aaaa111122223333"} 9000
impreza_proxy_latency_ms_count{deployment="dpl_aaaa111122223333"} 105
impreza_proxy_bytes_in_total{deployment="dpl_aaaa111122223333"} 4096
impreza_proxy_bytes_out_total{deployment="dpl_aaaa111122223333"} 8192
impreza_shield_pow_challenges_total{deployment="dpl_aaaa111122223333"} 6
impreza_shield_pow_passed_total{deployment="dpl_aaaa111122223333"} 5
impreza_shield_ratelimit_rejected_total{deployment="dpl_aaaa111122223333"} 4
`

// Counter reset: the proxy restarted and everything went backwards.
const expositionReset = `impreza_proxy_requests_total{class="2xx",deployment="dpl_aaaa111122223333"} 1
impreza_proxy_requests_total{class="5xx",deployment="dpl_aaaa111122223333"} 0
impreza_proxy_latency_ms_bucket{deployment="dpl_aaaa111122223333",le="25"} 1
impreza_proxy_latency_ms_bucket{deployment="dpl_aaaa111122223333",le="100"} 1
impreza_proxy_latency_ms_bucket{deployment="dpl_aaaa111122223333",le="500"} 1
impreza_proxy_latency_ms_bucket{deployment="dpl_aaaa111122223333",le="+Inf"} 1
impreza_proxy_latency_ms_sum{deployment="dpl_aaaa111122223333"} 10
impreza_proxy_latency_ms_count{deployment="dpl_aaaa111122223333"} 1
impreza_proxy_bytes_in_total{deployment="dpl_aaaa111122223333"} 10
impreza_proxy_bytes_out_total{deployment="dpl_aaaa111122223333"} 20
impreza_shield_pow_challenges_total{deployment="dpl_aaaa111122223333"} 0
impreza_shield_pow_passed_total{deployment="dpl_aaaa111122223333"} 0
impreza_shield_ratelimit_rejected_total{deployment="dpl_aaaa111122223333"} 0
`

func TestScrapeDeltasAndPercentiles(t *testing.T) {
	a := parseExposition(expositionA)
	if len(a) == 0 {
		t.Fatal("nothing parsed")
	}
	// Non-impreza series must be ignored.
	if _, ok := a[scrapeKey{metric: "requests", deployment: "srv0"}]; ok {
		t.Fatal("foreign series parsed")
	}
	delta := aggregateDeltas(diffSamples(nil, a))
	d := delta["dpl_aaaa111122223333"]
	if d == nil {
		t.Fatal("deployment missing")
	}
	if d.Requests != 42 || d.Status2xx != 40 || d.Status5xx != 2 {
		t.Fatalf("counts wrong: %+v", d)
	}
	if d.LatencyCount != 42 || d.LatencyMsSum != 3100 {
		t.Fatalf("latency sums wrong: %+v", d)
	}
	if d.BytesIn != 1024 || d.BytesOut != 2048 {
		t.Fatalf("bytes wrong: %+v", d)
	}
	if d.ShieldChallenges != 3 || d.ShieldPassed != 3 || d.ShieldRateLimited != 1 {
		t.Fatalf("shield counters wrong: %+v", d)
	}
	// 42 samples: 10 ≤25ms, 30 ≤100ms, all ≤500ms → p50 inside 100, p95/p99 = 500.
	if d.LatencyP50Ms != 100 {
		t.Fatalf("p50 = %v, want 100", d.LatencyP50Ms)
	}
	if d.LatencyP95Ms != 500 || d.LatencyP99Ms != 500 {
		t.Fatalf("p95/p99 = %v/%v, want 500", d.LatencyP95Ms, d.LatencyP99Ms)
	}

	// Second cycle: deltas only.
	b := parseExposition(expositionB)
	delta2 := aggregateDeltas(diffSamples(a, b))
	d2 := delta2["dpl_aaaa111122223333"]
	if d2.Requests != 63 || d2.Status2xx != 60 || d2.Status5xx != 3 {
		t.Fatalf("cycle-2 counts wrong: %+v", d2)
	}
	if d2.LatencyCount != 63 {
		t.Fatalf("cycle-2 latency count = %d", d2.LatencyCount)
	}
	// 63 samples: ≤25:2, ≤100:40, ≤500:63 → p50=100, p95=500.
	if d2.LatencyP50Ms != 100 || d2.LatencyP95Ms != 500 {
		t.Fatalf("cycle-2 percentiles wrong: %+v", d2)
	}
	// The other deployment had no traffic in cycle 2 → no negative, no entry.
	if _, ok := delta2["dpl_bbbb111122223333"]; ok {
		t.Fatal("idle deployment produced a delta block")
	}

	// Reset: everything lower than before → treat previous as zero.
	r := parseExposition(expositionReset)
	delta3 := aggregateDeltas(diffSamples(b, r))
	d3 := delta3["dpl_aaaa111122223333"]
	if d3.Requests != 1 || d3.Status2xx != 1 {
		t.Fatalf("reset handling wrong: %+v", d3)
	}
}

func TestParseSampleShapes(t *testing.T) {
	name, labels, value, ok := splitSample(`impreza_proxy_requests_total{class="2xx",deployment="dpl_x"} 12`)
	if !ok || name != "impreza_proxy_requests_total" || labels["class"] != "2xx" || value != 12 {
		t.Fatalf("labeled sample parsed wrong: %q %v %v %v", name, labels, value, ok)
	}
	name, labels, value, ok = splitSample(`impreza_proxy_latency_ms_sum{deployment="dpl_x"} 1.5e2`)
	if !ok || name != "impreza_proxy_latency_ms_sum" || labels["deployment"] != "dpl_x" || math.Abs(value-150) > 1e-9 {
		t.Fatalf("scientific notation parsed wrong: %v %v", value, ok)
	}
	if _, _, _, ok := splitSample(`garbage`); ok {
		t.Fatal("garbage accepted")
	}
}
