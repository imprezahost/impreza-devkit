package executor

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/imprezahost/impreza-devkit/agent-go/internal/proxy"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

var (
	switchHostnamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,252}[a-z0-9]$`)
	switchUpstreamPattern = regexp.MustCompile(`^dpl_[a-f0-9]{16,24}-app:\d{1,5}$`)
	switchIDPattern       = regexp.MustCompile(`^tsw_[a-f0-9]{24}$`)
)

// A reviewed hostname move. Nothing is stopped: the source keeps running, the
// target must prove health first, and any failed step restores both fragments
// and reloads the proxy back to the previous routing.
func (d *Docker) trafficSwitch(ctx context.Context, cmd *sdkclient.PollCommand) sdkclient.DeployResult {
	var p sdkclient.TrafficSwitchPayload
	if err := cmd.As(&p); err != nil {
		return failResult(cmd.ID, "decode traffic switch payload: "+err.Error())
	}
	if p.Protocol != sdkclient.TrafficSwitchProtocol || !recoveryDeploymentID.MatchString(p.DeploymentID) || !recoveryDeploymentID.MatchString(p.TargetDeploymentID) || p.DeploymentID == p.TargetDeploymentID ||
		!switchIDPattern.MatchString(p.SwitchID) || !switchHostnamePattern.MatchString(p.Hostname) || strings.Contains(p.Hostname, "..") ||
		!switchUpstreamPattern.MatchString(p.Upstream) || !strings.HasPrefix(p.Upstream, p.TargetDeploymentID+"-app:") {
		return failResult(cmd.ID, "unreviewed traffic switch payload")
	}
	if d.Proxy == nil {
		return failResult(cmd.ID, "traffic switch requires the host proxy")
	}
	if err := d.deploymentCheckpoint(ctx, cmd, "preparing"); err != nil {
		return preparationResult(cmd.ID, err)
	}
	if err := d.verifySwitchTarget(ctx, p); err != nil {
		return failResult(cmd.ID, err.Error())
	}
	// The boundary that wins the race with a customer cancellation: fragments
	// are only written after the server grants the replacement phase.
	if err := d.deploymentCheckpoint(ctx, cmd, "replacing"); err != nil {
		return preparationResult(cmd.ID, err)
	}
	rollback, err := d.Proxy.SwitchHostname(ctx, p.Hostname, p.DeploymentID, p.TargetDeploymentID, p.Upstream)
	if err != nil {
		status := "rolled_back"
		if errors.Is(err, proxy.ErrRoutingRecoveryRequired) {
			status = "recovery_required"
		}
		return failedTrafficSwitch(cmd.ID, p, status, "traffic switch: "+err.Error())
	}
	if err := d.probeSwitchedRoute(ctx, p); err != nil {
		if rbErr := rollback(ctx); rbErr != nil {
			return failedTrafficSwitch(cmd.ID, p, "recovery_required", "traffic switch probe failed AND rollback failed: "+err.Error()+" / "+rbErr.Error())
		}
		return failedTrafficSwitch(cmd.ID, p, "rolled_back", "traffic switch probe failed; previous routing restored: "+err.Error())
	}
	if err := d.Proxy.CompleteHostnameSwitch(p.Hostname, p.DeploymentID, p.TargetDeploymentID); err != nil {
		return failedTrafficSwitch(cmd.ID, p, "recovery_required", "Verified route could not finalize its recovery record; reconciliation required")
	}
	return sdkclient.DeployResult{
		CommandID:     cmd.ID,
		Status:        "success",
		DeploymentID:  p.DeploymentID,
		TrafficSwitch: &sdkclient.TrafficSwitchResult{SwitchID: p.SwitchID, Hostname: p.Hostname, Status: "switched"},
	}
}

func failedTrafficSwitch(command string, p sdkclient.TrafficSwitchPayload, status, message string) sdkclient.DeployResult {
	r := failResult(command, message)
	r.DeploymentID = p.DeploymentID
	r.TrafficSwitch = &sdkclient.TrafficSwitchResult{SwitchID: p.SwitchID, Hostname: p.Hostname, Status: status}
	return r
}

// The target proves it can serve before any fragment moves: one container,
// running, owned by the target project, and healthy when the image defines a
// healthcheck.
func (d *Docker) verifySwitchTarget(ctx context.Context, p sdkclient.TrafficSwitchPayload) error {
	check, cancel := context.WithTimeout(ctx, composeQueryTimeout)
	defer cancel()
	ids, err := d.recoveryContainers(check, p.TargetDeploymentID)
	if err != nil || len(ids) != 1 {
		return errors.New("target application identity cannot be verified")
	}
	raw, err := limitedRuntimeOutput(d.dockerCmd(check, "inspect", ids[0]), 1024*1024)
	var containers []struct {
		ID     string
		Config struct {
			Labels map[string]string
		}
		State struct {
			Running bool
			Health  *struct {
				Status string
			}
		}
	}
	if err != nil || json.Unmarshal(raw, &containers) != nil || len(containers) != 1 {
		return errors.New("target application cannot be inspected")
	}
	c := containers[0]
	if c.ID != ids[0] || !c.State.Running || c.Config.Labels["com.docker.compose.project"] != p.TargetDeploymentID || c.Config.Labels["com.docker.compose.service"] != "app" {
		return errors.New("target application ownership cannot be verified")
	}
	if c.State.Health != nil && c.State.Health.Status != "healthy" {
		return errors.New("target application is not healthy")
	}
	return nil
}

// The switched hostname, pinned to the proxy itself: TLS handshake, SNI and
// the upstream's answer prove the move end to end. A 5xx, a refused
// connection or a handshake failure all mean the move did not hold.
func (d *Docker) probeSwitchedRoute(ctx context.Context, p sdkclient.TrafficSwitchPayload) error {
	probe, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	raw, err := limitedRuntimeOutput(d.dockerCmd(probe, "inspect", proxy.ContainerName), 1024*1024)
	var containers []struct {
		NetworkSettings struct {
			Networks map[string]struct {
				IPAddress string `json:"IPAddress"`
			}
		}
	}
	if err != nil || json.Unmarshal(raw, &containers) != nil || len(containers) != 1 {
		return errors.New("proxy identity cannot be inspected for the probe")
	}
	proxyIP := containers[0].NetworkSettings.Networks[proxy.NetworkName].IPAddress
	if proxyIP == "" {
		return errors.New("proxy is absent from its own network")
	}
	out, err := d.dockerCmd(probe, "run", "--rm", "--network", proxy.NetworkName, switchProbeImage, "-sk", "-o", "/dev/null", "-w", "%{http_code}", "-m", "10", "--resolve", p.Hostname+":443:"+proxyIP, "https://"+p.Hostname+"/").CombinedOutput()
	code := strings.TrimSpace(string(out))
	if err != nil || code == "" || code == "000" || strings.HasPrefix(code, "5") {
		return errors.New("switched route did not answer through the proxy")
	}
	return nil
}

const switchProbeImage = "curlimages/curl:8.11.1"
