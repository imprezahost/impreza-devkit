package caddyshield

import (
	"github.com/caddyserver/caddy/v2"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
	"github.com/prometheus/client_golang/prometheus"
	"regexp"
	"strconv"
)

var wafRequests = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "impreza_shield_waf_requests_total", Help: "WAF request outcomes by deployment only."}, []string{"deployment", "outcome"})
var wafRules = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "impreza_shield_waf_rules_total", Help: "Static CRS rule findings per deployment; no request metadata."}, []string{"deployment", "rule_id", "category", "outcome"})
var wafDeploymentPattern = regexp.MustCompile(`^dpl_[a-f0-9]{16}(?:[a-f0-9]{8})?$`)

func RegisterWAFAggregates(ctx caddy.Context) {
	reg := ctx.GetMetricsRegistry()
	if reg == nil {
		return
	}
	for _, collector := range []prometheus.Collector{wafRequests, wafRules} {
		if err := reg.Register(collector); err != nil {
			if _, ok := err.(prometheus.AlreadyRegisteredError); !ok {
				panic(err)
			}
		}
	}
}

// This boundary cannot accept a visitor address or matched value. Category is
// looked up locally by integer ID, never taken from request text or CRS macros.
func RecordWAFAggregates(deployment string, ids []int, outcome string) {
	if !wafDeploymentPattern.MatchString(deployment) {
		return
	}
	if outcome != "matched" && outcome != "would_block" && outcome != "blocked" {
		return
	}
	if outcome != "matched" {
		wafRequests.WithLabelValues(deployment, outcome).Inc()
	}
	seen := map[int]bool{}
	for _, id := range ids {
		meta, ok := sdkclient.ShieldRule(id)
		if !ok || !meta.Detection || seen[id] {
			continue
		}
		seen[id] = true
		wafRules.WithLabelValues(deployment, strconv.Itoa(id), meta.Category, outcome).Inc()
	}
}
