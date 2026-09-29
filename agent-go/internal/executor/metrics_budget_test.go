//go:build !windows

package executor

import (
	"testing"
	"time"
)

// Only the Windows tests widen the collector's budgets, for their slower
// docker double (metrics_budget_windows_test.go); every other build,
// release included, keeps the production ones.
func TestMetricsBudgetsAreTheProductionOnes(t *testing.T) {
	if metricsBudgetScale != 1 {
		t.Fatalf("metricsBudgetScale is %d outside the Windows tests", metricsBudgetScale)
	}
	for _, d := range []time.Duration{25 * time.Second, 4 * time.Second, 8 * time.Second} {
		if got := metricsBudget(d); got != d {
			t.Fatalf("metricsBudget(%v) = %v, want %v", d, got, d)
		}
	}
}
