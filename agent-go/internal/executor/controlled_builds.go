package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const ControlledBuildProtocol = "controlled-build-v1"

type controlledBuildPolicy struct {
	Version int    `json:"version"`
	Enabled bool   `json:"enabled"`
	Image   string `json:"image"`
}

// The host administrator opts in; a deployment payload cannot enable this mode.
func (d *Docker) ControlledBuildsEnabled() (bool, error) {
	if err := realWorkDirectory(d.StateDir); err != nil {
		return false, err
	}
	var policy controlledBuildPolicy
	_, err := readWorkJSON(filepath.Join(d.StateDir, "controlled-builds.json"), &policy)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if policy.Version != 1 || policy.Image != ownedBuilderImage {
		return false, errors.New("controlled build policy needs explicit preparation for this agent version")
	}
	return policy.Enabled, nil
}

func (d *Docker) controlledBuildCommand(ctx context.Context, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "docker", args...)
	command.Env = preparationEnvironment(d)
	out := &ownedCommandOutput{}
	command.Stdout = out
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("Docker prerequisite check failed: %w", err)
	}
	if out.overflow {
		return nil, errors.New("Docker prerequisite output exceeded limit")
	}
	return out.data, nil
}

func (d *Docker) controlledBuildHost() error {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" || !supportsPreparationWorkers() {
		return errors.New("controlled builds require Linux amd64 with systemd")
	}
	if os.Geteuid() != 0 {
		return errors.New("controlled build administration requires root")
	}
	binary, err := os.Executable()
	if err != nil {
		return err
	}
	binary, err = filepath.EvalSymlinks(binary)
	if err != nil {
		return err
	}
	stateDir, err := filepath.EvalSymlinks(d.StateDir)
	if err != nil {
		return err
	}
	if err = controlledBuildPaths(binary, stateDir); err != nil {
		return err
	}
	raw, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return err
	}
	values := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			values[k] = strings.Trim(v, "\"")
		}
	}
	if values["ID"] != "ubuntu" || values["VERSION_ID"] != "24.04" {
		return errors.New("controlled builds currently support Ubuntu 24.04 amd64 only")
	}
	if _, err = os.Stat("/usr/sbin/apparmor_parser"); err != nil {
		return errors.New("install the AppArmor parser before enabling controlled builds")
	}
	if _, err = ownedBuilderProfilePresent(strings.Repeat("0", 32)); err != nil {
		return errors.New("AppArmor profile inventory is unavailable")
	}
	docker, err := exec.LookPath("docker")
	if err != nil {
		return err
	}
	return localPreparationDaemon(&preparationWorkRequest{Docker: docker, Env: preparationEnvironment(d), Step: "build"})
}

// Read-only verification, also repeated before each owned work item is persisted.
func (d *Docker) CheckControlledBuilds(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := d.controlledBuildHost(); err != nil {
		return err
	}
	_, err := ownedBuilderPrerequisites(ctx, d.controlledBuildCommand)
	if err != nil {
		return fmt.Errorf("controlled build prerequisites unavailable; install Docker Buildx/Compose and run builder prepare: %w", err)
	}
	return nil
}

// Explicit administrative action: verify the pinned archive and image, then
// atomically opt in. Never rewrite credentials, start a build, or change a sysctl.
func (d *Docker) PrepareControlledBuilds(ctx context.Context) error {
	if err := realWorkDirectory(d.StateDir); err != nil {
		return err
	}
	if err := d.controlledBuildHost(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if _, err := d.controlledBuildCommand(ctx, "buildx", "version"); err != nil {
		return errors.New("install the Docker Buildx plugin before running builder prepare")
	}
	if raw, inspectErr := inspectOwnedBuilderImage(ctx, d.controlledBuildCommand); inspectErr != nil {
		archive, err := downloadBuilderArtifact(ctx, d.StateDir, ownedBuilderArtifactURL, ownedBuilderArtifactSHA256)
		if err != nil {
			return err
		}
		defer os.Remove(archive)
		if _, err = d.controlledBuildCommand(ctx, "load", "--input", archive); err != nil {
			return err
		}
	} else if _, err := verifyOwnedBuilderImage(raw); err != nil {
		return err
	}
	if err := d.CheckControlledBuilds(ctx); err != nil {
		return err
	}
	return writeWorkJSON(d.StateDir, "controlled-builds.json", controlledBuildPolicy{Version: 1, Enabled: true, Image: ownedBuilderImage})
}

// Existing work retains its immutable request and completes its own cleanup.
func (d *Docker) DisableControlledBuilds() error {
	if err := realWorkDirectory(d.StateDir); err != nil {
		return err
	}
	return writeWorkJSON(d.StateDir, "controlled-builds.json", controlledBuildPolicy{Version: 1, Enabled: false, Image: ownedBuilderImage})
}

// Match the worker filesystem restrictions before activation rather than
// weakening PrivateTmp, ProtectHome or ProtectSystem for hidden inputs.
func controlledBuildPaths(binary, stateDir string) error {
	within := func(path, base string) bool { return path == base || strings.HasPrefix(path, base+"/") }
	for _, base := range []string{"/tmp", "/var/tmp", "/home", "/root", "/run/user"} {
		if within(binary, base) || within(stateDir, base) {
			return errors.New("controlled build executable or state is hidden by the supervised worker; install under /usr/local/bin with state under /var/lib")
		}
	}
	for _, base := range []string{"/usr", "/etc", "/boot"} {
		if within(stateDir, base) {
			return errors.New("controlled build state must be writable outside system directories; use /var/lib")
		}
	}
	return nil
}
