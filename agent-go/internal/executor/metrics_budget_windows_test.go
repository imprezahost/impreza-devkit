//go:build windows

package executor

// On Windows the docker double is the test binary copied as docker.exe, and
// each call costs about half a second (the first on a new copy, 1.5 s), and
// seconds on a busy machine: the five calls of one app's sample overran its
// 4 s budget, and the metrics tests failed with "unknown" or zero numbers.
// Linux, where the double is a shell shim, keeps the production budgets.
func init() { metricsBudgetScale = 10 }
