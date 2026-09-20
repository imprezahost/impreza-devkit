package executor

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

func TestTrafficSwitchPayloadBoundaries(t *testing.T) {
	source := "dpl_" + strings.Repeat("a", 16)
	target := "dpl_" + strings.Repeat("b", 16)
	valid := sdkclient.TrafficSwitchPayload{
		Protocol:           sdkclient.TrafficSwitchProtocol,
		SwitchID:           "tsw_" + strings.Repeat("1", 24),
		DeploymentID:       source,
		TargetDeploymentID: target,
		Hostname:           "app.example.test",
		Upstream:           target + "-app:8080",
	}
	marshal := func(p sdkclient.TrafficSwitchPayload) json.RawMessage {
		raw, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	for name, change := range map[string]func(*sdkclient.TrafficSwitchPayload){
		"protocol":         func(p *sdkclient.TrafficSwitchPayload) { p.Protocol = "traffic-switch-v0" },
		"switch identity":  func(p *sdkclient.TrafficSwitchPayload) { p.SwitchID = "bnd_" + strings.Repeat("1", 24) },
		"same deployment":  func(p *sdkclient.TrafficSwitchPayload) { p.TargetDeploymentID = p.DeploymentID },
		"source identity":  func(p *sdkclient.TrafficSwitchPayload) { p.DeploymentID = "app-" + strings.Repeat("a", 16) },
		"target identity":  func(p *sdkclient.TrafficSwitchPayload) { p.TargetDeploymentID = "app-" + strings.Repeat("b", 16) },
		"hostname shape":   func(p *sdkclient.TrafficSwitchPayload) { p.Hostname = "bad host" },
		"hostname path":    func(p *sdkclient.TrafficSwitchPayload) { p.Hostname = "evil.example.test/x" },
		"upstream foreign": func(p *sdkclient.TrafficSwitchPayload) { p.Upstream = "dpl_" + strings.Repeat("9", 16) + "-app:8080" },
		"upstream host":    func(p *sdkclient.TrafficSwitchPayload) { p.Upstream = "example.test:443" },
	} {
		t.Run(name, func(t *testing.T) {
			p := valid
			change(&p)
			result := (&Docker{}).trafficSwitch(context.Background(), &sdkclient.PollCommand{ID: "cmd_switch", Kind: sdkclient.CommandTrafficSwitch, Payload: marshal(p)})
			if result.Status != "failed" || result.TrafficSwitch != nil {
				t.Fatal("unreviewed traffic switch payload reached the proxy")
			}
		})
	}
	// A reviewed payload without a proxy refuses rather than guessing.
	result := (&Docker{}).trafficSwitch(context.Background(), &sdkclient.PollCommand{ID: "cmd_switch", Kind: sdkclient.CommandTrafficSwitch, Payload: marshal(valid)})
	if result.Status != "failed" || !strings.Contains(result.Error, "proxy") {
		t.Fatal("traffic switch ran without the host proxy")
	}
}
