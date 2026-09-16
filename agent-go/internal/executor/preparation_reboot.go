package executor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var bootIDPattern = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)

func currentBootID() string {
	raw, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	id := strings.TrimSpace(string(raw))
	if err != nil || !bootIDPattern.MatchString(id) {
		return ""
	}
	return id
}

// Only the local daemon is covered by host reboot evidence. A remote builder
// may still be running, so ambiguous/remote Docker configuration requires review.
func localPreparationDaemon(request *preparationWorkRequest) error {
	for _, value := range request.Env {
		key, val, _ := strings.Cut(value, "=")
		if (key == "DOCKER_HOST" && val != "" && val != "unix:///var/run/docker.sock") || (key == "DOCKER_CONTEXT" && val != "" && val != "default") || ((key == "BUILDX_BUILDER" || key == "BUILDKIT_HOST" || key == "BUILDX_CONFIG") && val != "") || (key == "COMPOSE_BAKE" && val != "" && val != "false" && val != "0") {
			return errors.New("remote or selected builder requires manual review")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, request.Docker, "context", "inspect", "--format", "{{.Endpoints.docker.Host}}")
	command.Env = request.Env
	out, err := command.Output()
	if err != nil || strings.TrimSpace(string(out)) != "unix:///var/run/docker.sock" {
		return errors.New("local Docker endpoint could not be verified")
	}
	if request.Step == "build" {
		command = exec.CommandContext(ctx, request.Docker, "buildx", "ls", "--format", "json")
		command.Env = request.Env
		out, err = command.Output()
		if err == nil {
			if err := verifyLocalBuilderListing(out); err != nil {
				return err
			}
		} else {
			// Ubuntu's Compose package can use the built-in daemon builder
			// without a Buildx CLI plugin. Verify actual plugin absence rather
			// than infer it from an arbitrary command error or timeout.
			command = exec.CommandContext(ctx, request.Docker, "info", "--format", "{{json .ClientInfo.Plugins}}")
			command.Env = request.Env
			out, err = command.Output()
			var plugins []struct {
				Name string `json:"Name"`
			}
			if err != nil || json.Unmarshal(out, &plugins) != nil || plugins == nil {
				return errors.New("Docker client plugins could not be verified")
			}
			for _, plugin := range plugins {
				if plugin.Name == "buildx" {
					return errors.New("installed Buildx could not prove a local builder")
				}
			}
		}
	}
	return nil
}

// Buildx inspect has no --format option. Its supported JSON listing identifies
// the selected builder explicitly; a failed/ambiguous listing is never absence.
func verifyLocalBuilderListing(raw []byte) error {
	if len(raw) > 1024*1024 {
		return errors.New("builder listing exceeds limit")
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	selected := 0
	seen := map[string]bool{}
	for count := 0; ; count++ {
		var row struct {
			Name, Driver string
			Current      *bool
			Error        json.RawMessage
			Nodes        []struct {
				Name, Endpoint, Status string
				Error                  json.RawMessage
			}
		}
		err := decoder.Decode(&row)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil || count >= 256 || row.Name == "" || seen[row.Name] || row.Current == nil {
			return errors.New("invalid builder listing")
		}
		seen[row.Name] = true
		if !*row.Current {
			continue
		}
		selected++
		noError := func(value json.RawMessage) bool {
			return len(value) == 0 || string(value) == "null" || string(value) == `""`
		}
		if row.Name != "default" || row.Driver != "docker" || !noError(row.Error) || len(row.Nodes) != 1 {
			return errors.New("selected builder is not the local built-in daemon")
		}
		node := row.Nodes[0]
		if node.Name != "default" || (node.Endpoint != "default" && node.Endpoint != "unix:///var/run/docker.sock") || node.Status != "running" || !noError(node.Error) {
			return errors.New("local builder node could not be verified")
		}
	}
	if selected != 1 {
		return errors.New("exactly one selected builder is required")
	}
	return nil
}

func (d *Docker) preparationAfterReboot(r *PreparationRecovery) (*PreparationRecovery, error) {
	_, request, err := d.loadPreparationWork(r.Work, r.DeploymentID)
	if err != nil {
		return nil, err
	}
	if request.OwnedBuilder {
		return nil, errors.New("owned builder requires authenticated executor recovery")
	}
	now := currentBootID()
	if !request.LocalBootRecovery || request.BootID == "" || now == "" || request.BootID == now {
		return nil, errors.New("no verified host reboot")
	}
	if err := localPreparationDaemon(request); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemctl", "show", "impreza-preparation-"+r.Work.ID+".service", "--property=ActiveState", "--property=MainPID").Output()
	if err != nil {
		return nil, errors.New("cannot verify interrupted worker state")
	}
	properties := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		key, val, ok := strings.Cut(line, "=")
		if !ok || properties[key] != "" {
			return nil, errors.New("invalid worker state")
		}
		properties[key] = val
	}
	if properties["MainPID"] != "0" || (properties["ActiveState"] != "inactive" && properties["ActiveState"] != "failed") {
		return nil, errors.New("worker may still be active")
	}
	app, err := d.recoveryPath(r.DeploymentID)
	if err != nil {
		return nil, err
	}
	hash, err := preparationConfigHash(app)
	if err != nil || hash != request.ConfigSHA256 {
		return nil, errors.New("preparation inputs changed since interrupted build")
	}
	// A corrupt receipt is not absence. The caller checked ErrPreparationPending;
	// check again so a late/changed file cannot be silently discarded.
	if _, err := os.Lstat(filepath.Join(d.StateDir, "operations", "preparation-"+r.Work.ID, "result.json")); !os.IsNotExist(err) {
		return nil, errors.New("receipt state changed during reboot reconciliation")
	}
	next := *r
	next.Phase = "aborted"
	return &next, next.Validate()
}
