package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/imprezahost/impreza-devkit/agent-go/internal/ingress"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

// IngressAvailable reports whether this host can enforce ingress allowlists
// (the capability is only advertised then; the control plane refuses to
// store a restricted rule for an agent that cannot apply it).
func IngressAvailable() bool {
	_, err := exec.LookPath("iptables-restore")
	return err == nil
}

func (d *Docker) ingressManager() *ingress.Manager { return ingress.NewManager(d.StateDir) }

func ingressFail(cmdID string, st ingress.Status, message string) sdkclient.DeployResult {
	res := failResult(cmdID, message)
	res.DeploymentID = st.DeploymentID
	res.Ingress = &sdkclient.IngressResult{DeploymentID: st.DeploymentID, Revision: st.Revision, Reason: st.Reason}
	return res
}

// ingressUpdate enforces the complete desired allowlist of one deployment.
// The payload is validated again here (the server validated it too), and
// every restricted port must be one this deployment publishes: a rule can
// never reach another deployment's port or a host port.
func (d *Docker) ingressUpdate(ctx context.Context, cmd *sdkclient.PollCommand) sdkclient.DeployResult {
	var p sdkclient.IngressUpdatePayload
	if err := cmd.As(&p); err != nil {
		return failResult(cmd.ID, "decode ingress_update payload failed")
	}
	policy := ingress.Policy{DeploymentID: p.DeploymentID, Revision: p.Revision, Rules: []ingress.Rule{}}
	for _, r := range p.Rules {
		policy.Rules = append(policy.Rules, ingress.Rule{Port: r.Port, Protocol: r.Protocol, Sources: r.Sources})
	}
	normalized, err := ingress.Normalize(policy)
	if err != nil {
		return ingressFail(cmd.ID, ingress.Status{DeploymentID: p.DeploymentID, Revision: p.Revision, Reason: ingress.ReasonInvalidPolicy},
			"Ingress allowlist refused by the agent: "+err.Error())
	}
	if len(normalized.Rules) > 0 {
		if err := d.ingressPortsPublished(ctx, normalized); err != nil {
			return ingressFail(cmd.ID, ingress.Status{DeploymentID: p.DeploymentID, Revision: p.Revision, Reason: ingress.ReasonNotPublished}, err.Error())
		}
	}
	st, err := d.ingressManager().Update(ctx, normalized)
	if err != nil && st.DeploymentID == "" {
		st = ingress.Status{DeploymentID: p.DeploymentID, Revision: p.Revision, Reason: ingress.ReasonApplyFailed}
	}
	result := sdkclient.IngressResult{DeploymentID: st.DeploymentID, Revision: st.Revision, Enforced: st.Enforced,
		Fingerprint: st.Fingerprint, Reason: st.Reason}
	if err != nil || !st.Enforced {
		res := failResult(cmd.ID, "Ingress allowlist not enforced ("+nonEmpty(st.Reason, ingress.ReasonApplyFailed)+"). The previous rules stay in place.")
		res.DeploymentID = p.DeploymentID
		res.Ingress = &result
		return res
	}
	return sdkclient.DeployResult{CommandID: cmd.ID, Status: "success", DeploymentID: p.DeploymentID, Ingress: &result}
}

func nonEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

type ingressContainer struct {
	Project     string                                 `json:"project"`
	NetworkMode string                                 `json:"network_mode"`
	Bindings    map[string][]struct{ HostPort string } `json:"bindings"`
}

// ingressPortsPublished checks each restricted port against the live
// containers: published by this deployment (HostConfig.PortBindings, so a
// stopped container still counts), or, for a deployment on the host network,
// not published by any other deployment. Only an explicit projection of the
// container is read, never its environment.
func (d *Docker) ingressPortsPublished(ctx context.Context, p ingress.Policy) error {
	idsOut, err := limitedRuntimeOutput(d.dockerCmd(ctx, "ps", "-aq"), 65536)
	if err != nil {
		return errors.New("Ingress allowlist not applied: the published ports could not be read from Docker.")
	}
	ids := strings.Fields(string(idsOut))
	if len(ids) == 0 || len(ids) > 500 {
		return errors.New("Ingress allowlist not applied: this deployment has no container on the host.")
	}
	const format = `{"project":{{json (index .Config.Labels "com.docker.compose.project")}},"network_mode":{{json .HostConfig.NetworkMode}},"bindings":{{json .HostConfig.PortBindings}}}`
	out, err := limitedRuntimeOutput(d.dockerCmd(ctx, append([]string{"inspect", "--format", format}, ids...)...), 1<<20)
	if err != nil {
		return errors.New("Ingress allowlist not applied: the published ports could not be read from Docker.")
	}
	own := strings.ToLower(p.DeploymentID)
	ownPorts, otherPorts := map[string]bool{}, map[string]bool{}
	hostNetwork, present := false, false
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		var c ingressContainer
		if json.Unmarshal([]byte(line), &c) != nil {
			continue
		}
		mine := c.Project == own
		if mine {
			present = true
			if c.NetworkMode == "host" {
				hostNetwork = true
			}
		}
		for key, binds := range c.Bindings {
			parts := strings.SplitN(key, "/", 2)
			if len(parts) != 2 {
				continue
			}
			for _, b := range binds {
				port, err := strconv.Atoi(b.HostPort)
				if err != nil || port < 1 || port > 65535 {
					continue
				}
				k := fmt.Sprintf("%s/%d", parts[1], port)
				if mine {
					ownPorts[k] = true
				} else {
					otherPorts[k] = true
				}
			}
		}
	}
	if !present {
		return errors.New("Ingress allowlist not applied: this deployment has no container on the host.")
	}
	for _, r := range p.Rules {
		k := fmt.Sprintf("%s/%d", r.Protocol, r.Port)
		if ownPorts[k] {
			continue
		}
		if hostNetwork && !otherPorts[k] {
			continue
		}
		return fmt.Errorf("Ingress allowlist not applied: port %s is not published by this deployment.", k)
	}
	return nil
}

// CollectIngress is the heartbeat view: revision, enforcement and
// fingerprint per deployment, never a source.
func (d *Docker) CollectIngress() *sdkclient.IngressReport {
	statuses, err := d.ingressManager().Report()
	if err != nil || len(statuses) == 0 {
		return nil
	}
	report := &sdkclient.IngressReport{Protocol: sdkclient.IngressProtocol}
	for _, st := range statuses {
		report.Deployments = append(report.Deployments, sdkclient.IngressResult{DeploymentID: st.DeploymentID,
			Revision: st.Revision, Enforced: st.Enforced, Fingerprint: st.Fingerprint, Reason: st.Reason})
	}
	return report
}

// removeDeploymentIngress drops the deployment's allowlist at uninstall; the
// last one removes the chains.
func (d *Docker) removeDeploymentIngress(ctx context.Context, deploymentID string) error {
	if err := d.ingressManager().Remove(ctx, deploymentID); err != nil {
		return errors.New("ingress allowlist of the deployment could not be removed")
	}
	return nil
}
