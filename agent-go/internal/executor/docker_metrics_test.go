package executor

import (
	"testing"
)

func TestParseHumanBytes(t *testing.T) {
	for in, want := range map[string]int64{
		"0B": 0, "512MiB": 536870912, "12.5MiB": 13107200, "1.2GB": 1200000000,
		"100kB": 100000, "2KiB": 2048, "": 0, "42": 42, "garbage": 0,
	} {
		if got := parseHumanBytes(in); got != want {
			t.Errorf("parseHumanBytes(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestParseMemUsage(t *testing.T) {
	used, total := parseMemUsage("12.5MiB / 512MiB")
	if used != 13107200 || total != 536870912 {
		t.Fatalf("parseMemUsage split: %d %d", used, total)
	}
	if used, total := parseMemUsage("garbage"); used != 0 || total != 0 {
		t.Fatal("a malformed usage must parse to zero, never guess")
	}
}

func TestParsePercent(t *testing.T) {
	if got := parsePercent("0.07%"); got < 0.06 || got > 0.08 {
		t.Fatalf("parsePercent: %v", got)
	}
	if got := parsePercent("not-a-number"); got != 0 {
		t.Fatal("a malformed percent parses to zero")
	}
}

func TestMergeMetricState(t *testing.T) {
	if mergeMetricState("running", "restarting") != "restarting" {
		t.Fatal("restarting outranks running — it is the alertable state")
	}
	if mergeMetricState("restarting", "running") != "restarting" {
		t.Fatal("running must not hide a restarting sibling")
	}
	if mergeMetricState("exited", "running") != "running" {
		t.Fatal("running outranks exited")
	}
	if mergeMetricState("", "exited") != "exited" {
		t.Fatal("exited fills an empty state")
	}
}
