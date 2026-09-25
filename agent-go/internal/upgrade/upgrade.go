// Package upgrade prepares a customer-requested agent update for execution
// outside the agent's systemd service cgroup. The control plane acknowledges
// the job before the transient unit stops and replaces the agent binary.
package upgrade

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/imprezahost/impreza-devkit/agent-go/packaging"
)

const Protocol = "agent-upgrade-v1"

var commandPattern = regexp.MustCompile(`^cmd_[a-f0-9]{16}$`)
var versionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

const wrapper = `#!/bin/sh
set -eu
# The receipt and operation journal must be cleared before stopping the agent.
sleep 8
export IMPREZA_AGENT_REQUIRE_MANIFEST=1
export IMPREZA_AGENT_CHANNEL="$1"
export IMPREZA_AGENT_EXPECTED_VERSION="$2"
exec /bin/sh "$3" --apply
`

type request struct {
	Protocol string `json:"protocol"`
	Channel  string `json:"channel"`
	Version  string `json:"version"`
}

// Available gates the advertised capability. The legacy customer command
// remains available on hosts without these prerequisites.
func Available() bool {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		return false
	}
	for _, tool := range []string{"systemd-run", "systemctl", "python3", "openssl", "curl", "flock", "sha256sum", "timeout", "sort", "awk"} {
		if _, err := exec.LookPath(tool); err != nil {
			return false
		}
	}
	return true
}

func validate(commandID, channel, version string) error {
	if !commandPattern.MatchString(commandID) || !versionPattern.MatchString(version) || (channel != "stable" && channel != "beta") {
		return errors.New("invalid agent update request")
	}
	return nil
}

func jobDir(stateDir, commandID string) (string, error) {
	if !commandPattern.MatchString(commandID) {
		return "", errors.New("invalid update command id")
	}
	return filepath.Join(stateDir, "upgrade-jobs", commandID), nil
}

func realDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 ||
		(runtime.GOOS == "linux" && (info.Mode().Perm()&0022 != 0 || !rootOwned(info))) {
		return errors.New("agent update directory must be a real directory")
	}
	return nil
}

// Prepare stores only the updater bundled in this agent and a validated
// channel/target. It never fetches or executes network content inside the
// agent service. The helper requires a signed manifest at execution time.
func Prepare(stateDir, commandID, channel, version string) error {
	if err := validate(commandID, channel, version); err != nil {
		return err
	}
	if !Available() {
		return errors.New("managed agent update is unavailable on this host")
	}
	if err := realDirectory(stateDir); err != nil {
		return err
	}
	base := filepath.Join(stateDir, "upgrade-jobs")
	if err := os.Mkdir(base, 0700); err != nil && !os.IsExist(err) {
		return err
	}
	if err := realDirectory(base); err != nil {
		return err
	}
	dir, _ := jobDir(stateDir, commandID)
	if err := os.Mkdir(dir, 0700); err != nil {
		return fmt.Errorf("create private update job: %w", err)
	}
	if err := realDirectory(dir); err != nil {
		return err
	}
	meta, err := json.Marshal(request{Protocol: Protocol, Channel: channel, Version: version})
	if err != nil {
		return err
	}
	for name, data := range map[string][]byte{"update.sh": packaging.UpdateScript, "run.sh": []byte(wrapper), "request.json": meta} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			return err
		}
	}
	return nil
}

type runner func(context.Context, string, ...string) ([]byte, error)

func run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// Start launches the already prepared updater in a separate systemd unit.
// Call only after the control plane acknowledges the success receipt.
func Start(stateDir, commandID string) error {
	return start(context.Background(), stateDir, commandID, run)
}

func start(ctx context.Context, stateDir, commandID string, execute runner) error {
	if err := realDirectory(stateDir); err != nil {
		return err
	}
	if err := realDirectory(filepath.Join(stateDir, "upgrade-jobs")); err != nil {
		return err
	}
	dir, err := jobDir(stateDir, commandID)
	if err != nil {
		return err
	}
	if err := realDirectory(dir); err != nil {
		return err
	}
	metaPath := filepath.Join(dir, "request.json")
	metaInfo, err := os.Lstat(metaPath)
	if err != nil || !metaInfo.Mode().IsRegular() || metaInfo.Size() > 256 {
		return errors.New("invalid prepared update request")
	}
	file, err := os.Open(metaPath)
	if err != nil {
		return errors.New("invalid prepared update request")
	}
	data, readErr := io.ReadAll(io.LimitReader(file, 257))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || len(data) > 256 {
		return errors.New("invalid prepared update request")
	}
	var r request
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil || r.Protocol != Protocol || validate(commandID, r.Channel, r.Version) != nil {
		return errors.New("invalid prepared update request")
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return errors.New("invalid prepared update request")
	}
	for name, expected := range map[string][]byte{"update.sh": packaging.UpdateScript, "run.sh": []byte(wrapper)} {
		path := filepath.Join(dir, name)
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() != int64(len(expected)) ||
			(runtime.GOOS == "linux" && (info.Mode().Perm()&0022 != 0 || !rootOwned(info))) {
			return errors.New("prepared updater was changed")
		}
		actual, err := os.ReadFile(path)
		if err != nil || sha256.Sum256(actual) != sha256.Sum256(expected) {
			return errors.New("prepared updater was changed")
		}
	}
	unit := "impreza-agent-upgrade-" + strings.TrimPrefix(commandID, "cmd_")
	launchCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	args := []string{"--unit=" + unit, "--collect", "--property=Type=exec"}
	for _, key := range []string{"IMPREZA_AGENT_RELEASE_BASE", "IMPREZA_AGENT_SOCKS_PROXY", "IMPREZA_RELEASE_PUBLIC_KEY"} {
		value := os.Getenv(key)
		if value == "" {
			continue
		}
		if len(value) > 512 || strings.ContainsAny(value, "\x00\r\n") {
			return errors.New("invalid operator update environment")
		}
		args = append(args, "--setenv="+key+"="+value)
	}
	args = append(args, "/bin/sh", filepath.Join(dir, "run.sh"), r.Channel, r.Version, filepath.Join(dir, "update.sh"))
	out, err := execute(launchCtx, "systemd-run", args...)
	if err == nil {
		return nil
	}
	// The receipt can be replayed after an agent restart. A running helper or
	// an already installed target means no second update should be scheduled.
	versionCtx, versionCancel := context.WithTimeout(ctx, 5*time.Second)
	defer versionCancel()
	installed, versionErr := execute(versionCtx, "/usr/local/bin/impreza-agent", "--version")
	if versionErr == nil && strings.TrimSpace(string(installed)) == "impreza-agent version "+r.Version {
		return nil
	}
	stateCtx, stateCancel := context.WithTimeout(ctx, 5*time.Second)
	defer stateCancel()
	state, stateErr := execute(stateCtx, "systemctl", "show", unit+".service", "-p", "ActiveState", "--value")
	if stateErr == nil && (strings.TrimSpace(string(state)) == "active" || strings.TrimSpace(string(state)) == "activating") {
		return nil
	}
	return fmt.Errorf("managed update helper did not start: %w (%s)", err, strings.TrimSpace(string(out)))
}
