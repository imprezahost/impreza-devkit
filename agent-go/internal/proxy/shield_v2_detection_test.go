package proxy

import (
	"context"
	"fmt"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestShieldV2DetectionExclusions(t *testing.T) {
	for _, id := range []int{949110, 959100, 901001, 901100} {
		for _, prefix := range []string{"", "/control-path/"} {
			t.Run(fmt.Sprintf("control-%d-%q", id, prefix), func(t *testing.T) {
				c := New(t.TempDir(), slog.Default())
				sc := &ShieldConfig{Profile: "hardened", Mode: "enforce", Protocol: sdkclient.ShieldV2Protocol, Exclusions: []sdkclient.ShieldExclusion{{RuleID: id, PathPrefix: prefix}}}
				err := c.ApplyDeploymentRoutes(context.Background(), "dpl_0123456789abcdef", []Route{{Hostname: "shield.example.invalid", Upstream: "app:80", TLSMode: "none", Shield: sc}})
				if err == nil || !strings.Contains(err.Error(), "Only detection rules") {
					t.Fatalf("control rule reached render instead of detection rejection: %v", err)
				}
				if _, err := os.Stat(filepath.Join(c.StateDir, "deployments", "dpl_0123456789abcdef.caddy")); !os.IsNotExist(err) {
					t.Fatal("control rule fragment reached disk")
				}
			})
		}
	}
	for _, prefix := range []string{"", "/allowed_path-v1.0/"} {
		sc := &ShieldConfig{Profile: "hardened", Mode: "enforce", Protocol: sdkclient.ShieldV2Protocol, Exclusions: []sdkclient.ShieldExclusion{{RuleID: 942100, PathPrefix: prefix}}}
		if err := sc.validate(); err != nil {
			t.Fatalf("detection rule incorrectly refused: %v", err)
		}
		fragment := renderFragment("dpl_0123456789abcdef", []Route{{Hostname: "shield.example.invalid", Upstream: "app:80", TLSMode: "none", Shield: sc}})
		wanted := "SecRuleRemoveById 942100"
		if prefix != "" {
			wanted = "ctl:ruleRemoveById=942100"
		}
		if !strings.Contains(fragment, wanted) {
			t.Fatal("valid detection exclusion was not rendered")
		}
	}
}
