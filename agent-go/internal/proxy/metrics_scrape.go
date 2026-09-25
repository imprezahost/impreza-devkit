// Proxy metrics scraping.
//
// Once per agent metrics cycle the proxy container's loopback admin
// endpoint is scraped for the impreza_proxy_* / impreza_shield_* counters
// and turned into per-deployment DELTAS since the previous scrape. The
// counters behind them only ever carry a deployment label — this scrape
// path never sees, stores or transmits a visitor identity. A counter that
// went backwards means the proxy container restarted; the previous snapshot
// is treated as zero for that series (the lost sub-minute window is not
// recoverable and is not invented).
package proxy

import (
	"context"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

// scrapeKey identifies one exposition series.
type scrapeKey struct {
	metric     string
	deployment string
	label      string // class / le / "" — never an identity-bearing label
}

// ScrapeMetrics returns per-deployment proxy deltas for this cycle. A missing
// or old proxy container yields an empty map — proxy observability degrades
// silently, it never breaks the container metrics report.
func (c *Caddy) ScrapeMetrics(ctx context.Context) map[string]*sdkclient.AppProxyMetrics {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "exec", ContainerName,
		"wget", "-qO-", "http://127.0.0.1:2019/metrics").Output()
	if err != nil || len(out) > 8<<20 {
		return nil
	}
	current := parseExposition(string(out))
	if len(current) == 0 {
		return nil
	}

	c.scrapeMu.Lock()
	prev := c.lastScrape
	c.lastScrape = current
	c.scrapeMu.Unlock()

	delta := diffSamples(prev, current)
	return aggregateDeltas(delta)
}

// parseExposition reads ONLY the Impreza series; everything else in the
// endpoint's output is ignored.
func parseExposition(text string) map[scrapeKey]float64 {
	out := map[scrapeKey]float64{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, labels, value, ok := splitSample(line)
		if !ok {
			continue
		}
		var key scrapeKey
		switch {
		case strings.HasPrefix(name, "impreza_proxy_requests_total"):
			key = scrapeKey{metric: "requests", deployment: labels["deployment"], label: labels["class"]}
		case strings.HasPrefix(name, "impreza_proxy_latency_ms_bucket"):
			key = scrapeKey{metric: "latency_bucket", deployment: labels["deployment"], label: labels["le"]}
		case name == "impreza_proxy_latency_ms_sum":
			key = scrapeKey{metric: "latency_sum", deployment: labels["deployment"]}
		case name == "impreza_proxy_latency_ms_count":
			key = scrapeKey{metric: "latency_count", deployment: labels["deployment"]}
		case name == "impreza_proxy_bytes_in_total":
			key = scrapeKey{metric: "bytes_in", deployment: labels["deployment"]}
		case name == "impreza_proxy_bytes_out_total":
			key = scrapeKey{metric: "bytes_out", deployment: labels["deployment"]}
		case name == "impreza_shield_pow_challenges_total":
			key = scrapeKey{metric: "shield_challenges", deployment: labels["deployment"]}
		case name == "impreza_shield_pow_passed_total":
			key = scrapeKey{metric: "shield_passed", deployment: labels["deployment"]}
		case name == "impreza_shield_ratelimit_rejected_total":
			key = scrapeKey{metric: "shield_rate_limited", deployment: labels["deployment"]}
		default:
			continue
		}
		if key.deployment == "" {
			continue
		}
		out[key] = value
	}
	return out
}

// splitSample parses `name{a="1",b="2"} 3.5` (labels optional).
func splitSample(line string) (string, map[string]string, float64, bool) {
	var name, rest string
	if i := strings.IndexByte(line, '{'); i > 0 {
		name = line[:i]
		j := strings.IndexByte(line[i:], '}')
		if j < 0 {
			return "", nil, 0, false
		}
		rest = line[i : i+j+1]
		line = line[i+j+1:]
	} else if i := strings.IndexByte(line, ' '); i > 0 {
		name = line[:i]
		rest = ""
		line = line[i:]
	} else {
		return "", nil, 0, false
	}
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return "", nil, 0, false
	}
	value, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return "", nil, 0, false
	}
	labels := map[string]string{}
	rest = strings.Trim(rest, "{}")
	if rest != "" {
		for _, pair := range strings.Split(rest, ",") {
			k, v, found := strings.Cut(pair, "=")
			if !found {
				continue
			}
			labels[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"`)
		}
	}
	return name, labels, value, true
}

// diffSamples computes prev→current deltas with restart detection.
// Keys with zero delta are omitted so idle deployments produce no block.
func diffSamples(prev, current map[scrapeKey]float64) map[scrapeKey]float64 {
	out := map[scrapeKey]float64{}
	for k, cur := range current {
		old := prev[k]
		if cur < old {
			old = 0 // counter reset (proxy restarted / reloaded state lost)
		}
		if d := cur - old; d != 0 {
			out[k] = d
		}
	}
	return out
}

// aggregateDeltas folds raw series deltas into the report block per
// deployment, including latency percentiles from the (cumulative) bucket
// deltas.
func aggregateDeltas(delta map[scrapeKey]float64) map[string]*sdkclient.AppProxyMetrics {
	byDep := map[string]*sdkclient.AppProxyMetrics{}
	get := func(dep string) *sdkclient.AppProxyMetrics {
		if m, ok := byDep[dep]; ok {
			return m
		}
		m := &sdkclient.AppProxyMetrics{}
		byDep[dep] = m
		return m
	}
	buckets := map[string][]float64{}   // dep → cumulative bucket counts
	bucketLe := map[string][]float64{}  // dep → sorted le values
	for k, v := range delta {
		switch k.metric {
		case "requests":
			m := get(k.deployment)
			m.Requests += int64(v)
			switch k.label {
			case "1xx":
				m.Status1xx += int64(v)
			case "2xx":
				m.Status2xx += int64(v)
			case "3xx":
				m.Status3xx += int64(v)
			case "4xx":
				m.Status4xx += int64(v)
			case "5xx":
				m.Status5xx += int64(v)
			}
		case "latency_sum":
			get(k.deployment).LatencyMsSum += v
		case "latency_count":
			get(k.deployment).LatencyCount += int64(v)
		case "bytes_in":
			get(k.deployment).BytesIn += int64(v)
		case "bytes_out":
			get(k.deployment).BytesOut += int64(v)
		case "shield_challenges":
			get(k.deployment).ShieldChallenges += int64(v)
		case "shield_passed":
			get(k.deployment).ShieldPassed += int64(v)
		case "shield_rate_limited":
			get(k.deployment).ShieldRateLimited += int64(v)
		case "latency_bucket":
			le, err := strconv.ParseFloat(k.label, 64)
			if err != nil {
				continue
			}
			buckets[k.deployment] = append(buckets[k.deployment], v)
			bucketLe[k.deployment] = append(bucketLe[k.deployment], le)
		}
	}
	for dep, m := range byDep {
		m.LatencyP50Ms = percentileFromBuckets(bucketLe[dep], buckets[dep], 0.50)
		m.LatencyP95Ms = percentileFromBuckets(bucketLe[dep], buckets[dep], 0.95)
		m.LatencyP99Ms = percentileFromBuckets(bucketLe[dep], buckets[dep], 0.99)
	}
	return byDep
}

// percentileFromBuckets interpolates a percentile over cumulative buckets.
// les and counts are parallel, unsorted on input.
func percentileFromBuckets(les, cumulative []float64, p float64) float64 {
	if len(les) == 0 || len(les) != len(cumulative) {
		return 0
	}
	type pt struct {
		le float64
		c  float64
	}
	pts := make([]pt, len(les))
	for i := range les {
		pts[i] = pt{les[i], cumulative[i]}
	}
	sort.Slice(pts, func(i, j int) bool { return pts[i].le < pts[j].le })
	total := pts[len(pts)-1].c
	if total <= 0 {
		return 0
	}
	target := p * total
	prevC := 0.0
	for _, b := range pts {
		if b.c >= target {
			if b.c == prevC {
				return b.le
			}
			return b.le
		}
		prevC = b.c
	}
	return pts[len(pts)-1].le
}

// scrapeStateReset clears the previous snapshot (used by tests).
func (c *Caddy) scrapeStateReset() {
	c.scrapeMu.Lock()
	c.lastScrape = nil
	c.scrapeMu.Unlock()
}
