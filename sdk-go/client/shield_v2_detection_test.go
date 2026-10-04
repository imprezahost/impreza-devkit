package client

import (
	"strings"
	"testing"
)

func TestShieldExclusionsDetectionOnly(t *testing.T) {
	for _, id := range []int{949110, 959100, 901001, 901100} {
		for _, prefix := range []string{"", "/control-path/"} {
			err := ValidateShieldExclusions([]ShieldExclusion{{RuleID: id, PathPrefix: prefix}})
			if err == nil || !strings.Contains(err.Error(), "Only detection rules") {
				t.Fatalf("control rule %d (%q) must be refused: %v", id, prefix, err)
			}
		}
	}
	for _, prefix := range []string{"", "/allowed_path-v1.0/"} {
		if err := ValidateShieldExclusions([]ShieldExclusion{{RuleID: 942100, PathPrefix: prefix}}); err != nil {
			t.Fatalf("detection rule 942100 (%q) must remain accepted: %v", prefix, err)
		}
	}
}
