package executor

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

const retainedReleases = 5

// Snapshots contain resolved environment values. Never return their contents
// to the control plane; persist them only in the root-owned agent state dir.
type runtimeRelease struct {
	Metadata  sdkclient.DeploymentRelease `json:"metadata"`
	Compose   json.RawMessage             `json:"compose"`
	Env       []byte                      `json:"env"`
	EnvExists bool                        `json:"env_exists"`
	Tags      []string                    `json:"tags"`
}

func pinReleaseCompose(raw []byte, images map[string]string) ([]byte, error) {
	var model map[string]any
	if err := json.Unmarshal(raw, &model); err != nil {
		return nil, err
	}
	services, ok := model["services"].(map[string]any)
	if !ok || len(services) == 0 {
		return nil, fmt.Errorf("release has no services")
	}
	for name, value := range services {
		service, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("invalid service %s", name)
		}
		image := images[name]
		if !strings.HasPrefix(image, "sha256:") || len(image) != 71 {
			return nil, fmt.Errorf("no running image identity for service %s", name)
		}
		if _, err := hex.DecodeString(strings.TrimPrefix(image, "sha256:")); err != nil {
			return nil, fmt.Errorf("invalid image identity for service %s", name)
		}
		service["image"] = image
		delete(service, "build")
		service["pull_policy"] = "never"
	}
	// `compose config` already escapes literal dollars for replay. Preserve
	// its values verbatim: escaping again changes the restored environment.
	return json.Marshal(model)
}

// Capture BEFORE pulling or building, since both can move mutable image tags.
// A broken or absent previous runtime is not an eligible recovery target.
func (d *Docker) captureRelease(ctx context.Context, dir, id string, protected ...string) (*runtimeRelease, error) {
	ctx, cancel := context.WithTimeout(ctx, composeQueryTimeout)
	defer cancel()
	states, err := d.inspectProjectContainers(ctx, id)
	if err != nil {
		return nil, err
	}
	if len(states) == 0 {
		return nil, nil
	}
	running := false
	for _, state := range states {
		if !state.ok() {
			return nil, nil
		}
		running = running || state.Status == "running"
	}
	if !running {
		return nil, nil
	}
	raw, err := d.compose(ctx, dir, "config", "--format", "json")
	if err != nil {
		return nil, fmt.Errorf("resolve previous Compose: %w", err)
	}
	ids, err := d.dockerCmd(ctx, "ps", "-aq", "--filter", "label=com.docker.compose.project="+strings.ToLower(id)).Output()
	if err != nil {
		return nil, err
	}
	if len(strings.Fields(string(ids))) == 0 {
		return nil, fmt.Errorf("previous containers disappeared")
	}
	args := append([]string{"inspect"}, strings.Fields(string(ids))...)
	data, err := d.dockerCmd(ctx, args...).Output()
	if err != nil {
		return nil, err
	}
	var containers []struct {
		Image  string
		Config struct{ Labels map[string]string }
	}
	if err := json.Unmarshal(data, &containers); err != nil {
		return nil, err
	}
	images := map[string]string{}
	for _, c := range containers {
		name := c.Config.Labels["com.docker.compose.service"]
		if name == "" || strings.EqualFold(c.Config.Labels["com.docker.compose.oneoff"], "true") {
			continue
		}
		if existing := images[name]; existing != "" && existing != c.Image {
			return nil, fmt.Errorf("service %s has multiple running images", name)
		}
		images[name] = c.Image
	}
	pinned, err := pinReleaseCompose(raw, images)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, 8)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	release := &runtimeRelease{Metadata: sdkclient.DeploymentRelease{ID: "rel_" + now.Format("20060102T150405.000000000") + "_" + hex.EncodeToString(nonce), CreatedAt: now.Format(time.RFC3339Nano), ImageIDs: images}, Compose: pinned}
	release.Env, err = os.ReadFile(filepath.Join(dir, ".env"))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	release.EnvExists = err == nil
	// Private tags retain old images even when a later build moves their tag.
	// External `docker image prune -a` can still remove unused releases.
	complete := false
	defer func() {
		if !complete {
			for _, tag := range release.Tags {
				_, _ = d.dockerCmd(ctx, "image", "rm", tag).CombinedOutput()
			}
		}
	}()
	for service, image := range images {
		hash := sha256.Sum256([]byte(id + "/" + release.Metadata.ID + "/" + service))
		tag := "impreza-release:" + hex.EncodeToString(hash[:])
		if out, err := d.dockerCmd(ctx, "image", "tag", image, tag).CombinedOutput(); err != nil {
			return nil, fmt.Errorf("retain release image: %w: %s", err, tail(out, 512))
		}
		release.Tags = append(release.Tags, tag)
	}
	archive := filepath.Join(dir, "releases")
	if err := os.MkdirAll(archive, 0700); err != nil {
		return nil, err
	}
	release.Metadata.RollbackProtocol = "release-v1"
	encoded, err := json.Marshal(release)
	if err != nil {
		return nil, err
	}
	if err := writeAtomic(filepath.Join(archive, release.Metadata.ID+".json"), encoded, 0600); err != nil {
		return nil, err
	}
	complete = true
	d.pruneReleases(ctx, archive, retainedReleases, protected...)
	return release, nil
}

func (d *Docker) pruneReleases(ctx context.Context, dir string, keep int, protected ...string) {
	files, err := filepath.Glob(filepath.Join(dir, "rel_*.json"))
	if err != nil {
		return
	}
	sort.Strings(files)
	remaining := len(files)
	for len(files) > 0 && remaining > keep {
		path := files[0]
		files = files[1:]
		preserve := false
		for _, id := range protected {
			preserve = preserve || filepath.Base(path) == id+".json"
		}
		if preserve {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var release runtimeRelease
		if json.Unmarshal(raw, &release) != nil {
			continue
		}
		for _, tag := range release.Tags {
			// Only remove tags minted by this feature, never caller-provided image refs.
			suffix := strings.TrimPrefix(tag, "impreza-release:")
			if len(suffix) != 64 || suffix == tag {
				continue
			}
			if _, err := hex.DecodeString(suffix); err != nil {
				continue
			}
			if _, err := d.dockerCmd(ctx, "image", "rm", tag).CombinedOutput(); err != nil {
				d.Log.Warn("release retention: tag cleanup failed", "tag", tag, "err", err)
			}
		}
		if err := os.Remove(path); err != nil {
			d.Log.Warn("release retention: archive cleanup failed", "err", err)
		} else {
			remaining--
		}
	}
}

func (d *Docker) recoverStartup(ctx context.Context, dir, id string, previous *runtimeRelease, isRedeploy bool, reason string) sdkclient.DeployResult {
	result := failResult("", reason)
	if previous == nil {
		d.rollbackFailedDeploy(ctx, dir, id, isRedeploy, reason)
		return result
	}
	result.Rollback = &sdkclient.DeploymentRollback{Status: "failed", ReleaseID: previous.Metadata.ID, RuntimeState: "unknown"}
	// Recovery needs its own bounded budget even if deployment timed out.
	recovery, cancel := context.WithTimeout(context.WithoutCancel(ctx), composeUpTimeout+settleBudget+composeQueryTimeout)
	defer cancel()
	err := d.restoreRelease(recovery, dir, id, previous)
	if err != nil {
		result.Error += "\nAutomatic rollback failed; runtime state is unknown: " + err.Error()
		return result
	}
	result.Rollback.Status = "restored"
	result.Rollback.RuntimeState = "healthy"
	result.Release = &previous.Metadata
	result.Error += "\nAutomatic rollback restored the previous release; its containers passed startup checks. The deployment attempt failed."
	return result
}

func (d *Docker) restoreRelease(ctx context.Context, dir, id string, previous *runtimeRelease) error {
	// Verify all immutable images BEFORE writing config or touching containers.
	for _, image := range previous.Metadata.ImageIDs {
		if _, err := d.dockerCmd(ctx, "image", "inspect", image).Output(); err != nil {
			return fmt.Errorf("previous image unavailable: %w", err)
		}
	}
	if err := writeAtomic(filepath.Join(dir, "compose.yaml"), previous.Compose, 0600); err != nil {
		return err
	}
	if previous.EnvExists {
		if err := writeAtomic(filepath.Join(dir, ".env"), previous.Env, 0600); err != nil {
			return err
		}
	} else if err := os.Remove(filepath.Join(dir, ".env")); err != nil && !os.IsNotExist(err) {
		return err
	}
	// No down, volume deletion, build, registry access or install hook replay.
	if out, err := d.compose(ctx, dir, "up", "-d", "--no-build", "--pull", "never", "--remove-orphans"); err != nil {
		return fmt.Errorf("restore previous containers: %w\n%s", err, tail(out, 1024))
	}
	verdict, detail := d.awaitStackSettled(ctx, id)
	if verdict != settleHealthy {
		return fmt.Errorf("previous release did not pass startup checks: %s", detail)
	}
	return nil
}
