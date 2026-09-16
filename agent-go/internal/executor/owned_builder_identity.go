package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// ownedBuilderIdentity is private operation state, never customer-supplied input.
// A name is useful for diagnostics; only the complete bound container ID authorizes removal.
type ownedBuilderIdentity struct {
	Version       int    `json:"version"`
	WorkID        string `json:"work_id"`
	CommandID     string `json:"command_id"`
	DeploymentID  string `json:"deployment_id"`
	RequestSHA256 string `json:"request_sha256"`
	ContainerID   string `json:"container_id"`
	ImageID       string `json:"image_id"`
	StateVolume   string `json:"state_volume"`
}

func (b *ownedBuilderIdentity) validate(w *PreparationWork, deploymentID string) error {
	if b == nil || w == nil || w.validate() != nil || w.Step != "build" || b.Version != 1 ||
		b.WorkID != w.ID || b.CommandID != w.CommandID || b.RequestSHA256 != w.RequestSHA256 ||
		b.DeploymentID != deploymentID || !recoveryDeploymentID.MatchString(deploymentID) ||
		!recoveryContainerID.MatchString(b.ContainerID) || !strings.HasPrefix(b.ImageID, "sha256:") ||
		!workHashPattern.MatchString(strings.TrimPrefix(b.ImageID, "sha256:")) ||
		!workHashPattern.MatchString(b.StateVolume) {
		return errors.New("builder identity does not match the bound build operation")
	}
	return nil
}

type ownedBuilderInspection struct {
	ID     string `json:"Id"`
	Name   string `json:"Name"`
	Image  string `json:"Image"`
	Config *struct {
		User   string            `json:"User"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	HostConfig *struct {
		Privileged      bool                       `json:"Privileged"`
		CapAdd          []string                   `json:"CapAdd"`
		Devices         []json.RawMessage          `json:"Devices"`
		DeviceRequests  []json.RawMessage          `json:"DeviceRequests"`
		SecurityOpt     []string                   `json:"SecurityOpt"`
		PidsLimit       int64                      `json:"PidsLimit"`
		Memory          int64                      `json:"Memory"`
		NanoCPUs        int64                      `json:"NanoCpus"`
		Binds           []string                   `json:"Binds"`
		Mounts          []json.RawMessage          `json:"Mounts"`
		PortBindings    map[string]json.RawMessage `json:"PortBindings"`
		PublishAllPorts bool                       `json:"PublishAllPorts"`
		NetworkMode     string                     `json:"NetworkMode"`
		PidMode         string                     `json:"PidMode"`
		IpcMode         string                     `json:"IpcMode"`
		RestartPolicy   struct {
			Name string `json:"Name"`
		} `json:"RestartPolicy"`
	} `json:"HostConfig"`
	Mounts []struct {
		Type        string `json:"Type"`
		Name        string `json:"Name"`
		Destination string `json:"Destination"`
	} `json:"Mounts"`
}

func (b *ownedBuilderIdentity) verifyInspection(raw []byte) error {
	var rows []ownedBuilderInspection
	if len(raw) > 1024*1024 || json.Unmarshal(raw, &rows) != nil || len(rows) != 1 {
		return errors.New("invalid builder inspection")
	}
	r := rows[0]
	if r.ID != b.ContainerID || r.Name != "/impreza-builder-"+b.WorkID || r.Image != b.ImageID || r.Config == nil || r.HostConfig == nil {
		return errors.New("builder container identity changed")
	}
	labels := r.Config.Labels
	if labels["impreza.builder.work"] != b.WorkID || labels["impreza.builder.command"] != b.CommandID ||
		labels["impreza.builder.deployment"] != b.DeploymentID || labels["impreza.builder.request"] != b.RequestSHA256 {
		return errors.New("builder ownership labels changed")
	}
	h := r.HostConfig
	if r.Config.User != "1000:1000" || h.Privileged || len(h.CapAdd) != 0 || len(h.Devices) != 0 || len(h.DeviceRequests) != 0 || h.PidsLimit != 512 || h.Memory != 768*1024*1024 || h.NanoCPUs != 1_000_000_000 {
		return errors.New("builder privilege or resource boundary changed")
	}
	// Rootless feasibility requires these exceptions within the dedicated container.
	// A scoped profile is a prerequisite; never disable the host userns restriction.
	security := slices.Clone(h.SecurityOpt)
	slices.Sort(security)
	if !slices.Equal(security, []string{"apparmor=impreza-builder-" + b.WorkID, "seccomp=unconfined"}) {
		return errors.New("builder security profile changed")
	}
	if len(h.Binds) != 0 || len(h.Mounts) != 0 || len(h.PortBindings) != 0 || h.PublishAllPorts ||
		h.NetworkMode != "bridge" || h.PidMode != "" || (h.IpcMode != "private" && h.IpcMode != "") || h.RestartPolicy.Name != "no" {
		return errors.New("builder execution boundary changed")
	}
	if len(r.Mounts) != 1 || r.Mounts[0].Type != "volume" || r.Mounts[0].Name != b.StateVolume || r.Mounts[0].Destination != "/home/user/.local/share/buildkit" {
		return errors.New("builder state volume changed")
	}
	return nil
}

type ownedBuilderCommand func(context.Context, ...string) ([]byte, error)

// removeOwnedBuilder is the Docker-side guard, not a cancellation grant. Its caller
// must load the private bound operation, confirm local endpoint and authenticated
// preparing/cancel_requested state, and persist stop intent BEFORE invoking this.
// Any uncertainty retains the journal; absence of a client process proves nothing.
func removeOwnedBuilder(ctx context.Context, b *ownedBuilderIdentity, w *PreparationWork, deploymentID string, run ownedBuilderCommand) error {
	if err := b.validate(w, deploymentID); err != nil {
		return err
	}
	if run == nil {
		return errors.New("builder command transport unavailable")
	}
	raw, err := run(ctx, "container", "inspect", b.ContainerID)
	if err != nil {
		return fmt.Errorf("builder inspection uncertain: %w", err)
	}
	if err = b.verifyInspection(raw); err != nil {
		return err
	}
	if _, err = run(ctx, "container", "rm", "--force", "--volumes", b.ContainerID); err != nil {
		return fmt.Errorf("builder removal uncertain: %w", err)
	}
	// A successful removal response is followed by a complete, unfiltered inventory.
	// Do not interpret a failed inspect or human-readable 'not found' message as proof.
	raw, err = run(ctx, "container", "ls", "--all", "--quiet", "--no-trunc")
	if err != nil {
		return fmt.Errorf("builder absence could not be verified: %w", err)
	}
	if len(raw) > 1024*1024 {
		return errors.New("builder inventory exceeds verification limit")
	}
	seen := map[string]bool{}
	for _, id := range strings.Fields(string(raw)) {
		if !recoveryContainerID.MatchString(id) || seen[id] {
			return errors.New("invalid builder inventory")
		}
		if id == b.ContainerID {
			return errors.New("builder still exists after removal")
		}
		seen[id] = true
	}
	return nil
}
