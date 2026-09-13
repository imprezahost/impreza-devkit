package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

var releaseIDPattern = regexp.MustCompile(`^rel_[A-Za-z0-9._-]{1,100}$`)

func loadRelease(dir, id string) (*runtimeRelease, error) {
	if !releaseIDPattern.MatchString(id) {
		return nil, fmt.Errorf("invalid release id")
	}
	path := filepath.Join(dir, "releases", id+".json")
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("release is no longer retained: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > 16*1024*1024 {
		return nil, fmt.Errorf("invalid release snapshot")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var release runtimeRelease
	if err := json.Unmarshal(raw, &release); err != nil {
		return nil, fmt.Errorf("invalid release snapshot: %w", err)
	}
	if release.Metadata.ID != id {
		return nil, fmt.Errorf("release identity mismatch")
	}
	var model struct {
		Services map[string]struct{ Image string }
	}
	if err := json.Unmarshal(release.Compose, &model); err != nil {
		return nil, err
	}
	if len(model.Services) == 0 || len(model.Services) != len(release.Metadata.ImageIDs) {
		return nil, fmt.Errorf("incomplete release image identities")
	}
	for name, service := range model.Services {
		if service.Image != release.Metadata.ImageIDs[name] {
			return nil, fmt.Errorf("release image identity mismatch")
		}
	}
	if _, err := pinReleaseCompose(release.Compose, release.Metadata.ImageIDs); err != nil {
		return nil, err
	}
	return &release, nil
}

// An image rollback must not silently move ports, storage or proxy targets.
func releaseContract(raw []byte) (map[string]any, error) {
	var model map[string]any
	if err := json.Unmarshal(raw, &model); err != nil {
		return nil, err
	}
	services, ok := model["services"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("no service contract")
	}
	contract := map[string]any{"name": model["name"], "networks": model["networks"], "volumes": model["volumes"], "secrets": model["secrets"], "configs": model["configs"]}
	byService := map[string]any{}
	for name, value := range services {
		service, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("invalid service contract")
		}
		fields := map[string]any{}
		for _, key := range []string{"container_name", "hostname", "ports", "networks", "network_mode", "volumes", "secrets", "configs"} {
			fields[key] = service[key]
		}
		env, _ := service["environment"].(map[string]any)
		for _, key := range []string{"DOMAIN", "DOMAIN_URL", "HOST_PORT"} {
			fields["env_"+key] = env[key]
		}
		byService[name] = fields
	}
	contract["services"] = byService
	return contract, nil
}

func (d *Docker) rollbackRelease(ctx context.Context, cmd *sdkclient.PollCommand) sdkclient.DeployResult {
	var payload sdkclient.RollbackPayload
	if err := cmd.As(&payload); err != nil {
		return failResult(cmd.ID, "decode rollback: "+err.Error())
	}
	if payload.DeploymentID == "" {
		return failResult(cmd.ID, "missing deployment_id")
	}
	dir := d.appDir(payload.DeploymentID)
	target, err := loadRelease(dir, payload.TargetVersion)
	if err != nil {
		return failResult(cmd.ID, err.Error()+"; containers were not changed")
	}
	checkCtx, cancel := context.WithTimeout(ctx, composeQueryTimeout)
	defer cancel()
	raw, err := d.compose(checkCtx, dir, "config", "--format", "json")
	if err != nil {
		return failResult(cmd.ID, "cannot resolve current configuration; containers were not changed")
	}
	currentContract, err := releaseContract(raw)
	if err != nil {
		return failResult(cmd.ID, err.Error())
	}
	targetContract, err := releaseContract(target.Compose)
	if err != nil || !reflect.DeepEqual(currentContract, targetContract) {
		return failResult(cmd.ID, "release changes ports, storage or routing; redeploy explicitly instead; containers were not changed")
	}
	for _, image := range target.Metadata.ImageIDs {
		if _, err := d.dockerCmd(checkCtx, "image", "inspect", image).Output(); err != nil {
			return failResult(cmd.ID, "release image is unavailable; containers were not changed")
		}
	}
	// Preserve the selected archive during retention while saving a recovery target.
	previous, err := d.captureRelease(ctx, dir, payload.DeploymentID, target.Metadata.ID)
	if err != nil || previous == nil {
		return failResult(cmd.ID, "current runtime must pass startup checks before manual rollback; containers were not changed")
	}
	recovery, cancelRecovery := context.WithTimeout(ctx, composeUpTimeout+settleBudget+composeQueryTimeout)
	defer cancelRecovery()
	if err := d.restoreRelease(recovery, dir, payload.DeploymentID, target); err != nil {
		result := d.recoverStartup(ctx, dir, payload.DeploymentID, previous, true, "manual rollback failed: "+err.Error())
		result.CommandID = cmd.ID
		result.DeploymentID = payload.DeploymentID
		return result
	}
	target.Metadata.RollbackProtocol = "release-v1"
	return sdkclient.DeployResult{CommandID: cmd.ID, DeploymentID: payload.DeploymentID, Status: "success", Release: &target.Metadata,
		Rollback: &sdkclient.DeploymentRollback{Status: "restored", ReleaseID: target.Metadata.ID, RuntimeState: "healthy"},
		LogsTail: "Selected release restored and Docker startup checks passed. Database and mutable data were not reverted."}
}
