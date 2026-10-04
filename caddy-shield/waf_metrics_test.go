package caddyshield

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"strings"
	"testing"
)

func TestWAFAggregatesPrivacyAndDistinctRequests(t *testing.T) {
	wafRequests.Reset()
	wafRules.Reset()
	reg := prometheus.NewRegistry()
	reg.MustRegister(wafRequests, wafRules)
	dep := "dpl_0123456789abcdef"
	RecordWAFAggregates(dep, []int{942100, 942100, 941100, 901001, 949110, 999999}, "would_block")
	RecordWAFAggregates(dep, []int{942100}, "would_block")
	RecordWAFAggregates(dep, []int{942100}, "blocked")
	RecordWAFAggregates("<VISITOR_ID>", []int{942100}, "blocked")
	RecordWAFAggregates(dep, []int{942100}, "<VISITOR_ID>")
	if got := testutil.ToFloat64(wafRequests.WithLabelValues(dep, "would_block")); got != 2 {
		t.Fatalf("distinct would_block=%v", got)
	}
	if got := testutil.ToFloat64(wafRequests.WithLabelValues(dep, "blocked")); got != 1 {
		t.Fatalf("distinct blocked=%v", got)
	}
	if got := testutil.ToFloat64(wafRules.WithLabelValues(dep, "942100", "SQLi", "would_block")); got != 2 {
		t.Fatalf("same rule counted twice on one request: %v", got)
	}
	metrics, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range metrics {
		for _, point := range family.Metric {
			if len(point.Label) != 2 && len(point.Label) != 4 {
				t.Fatal("identity-bearing label cardinality")
			}
			for _, label := range point.Label {
				if !strings.Contains("|deployment|outcome|rule_id|category|", "|"+label.GetName()+"|") || strings.Contains(label.GetValue(), "VISITOR") {
					t.Fatal("identity entered aggregate boundary")
				}
				if label.GetName() == "rule_id" && (label.GetValue() == "901001" || label.GetValue() == "949110" || label.GetValue() == "999999") {
					t.Fatal("control rule counted as a threat detection")
				}
			}
		}
	}
}
