package executor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

func validateForkPayload(p sdkclient.DeployPayload) error {
	if p.GitAuthMethod != "" && p.GitAuthMethod != "none" {
		return errors.New("fork preview refuses Git credentials")
	}
	if p.Manifest.Runtime.Build == nil || p.Manifest.Runtime.Build.Git == nil || p.Manifest.Runtime.Build.Git.CommitSHA == "" && p.GitCommitSHA == "" {
		return errors.New("fork preview requires a pinned Git build")
	}
	if len(p.Manifest.Runtime.Build.AuxiliaryFiles) > 0 || len(p.Manifest.Runtime.Build.SecretNames) > 0 || len(p.Manifest.Runtime.ServiceBindings) > 0 || p.Manifest.Runtime.TorEgress || p.Quiesce != nil || len(p.ServiceBindingRetirementAuthorizations) > 0 || p.RestorePlanID != "" || p.ReadySwap != nil || p.OnionImport != nil {
		return errors.New("fork preview refuses secrets, bindings and egress sidecars")
	}
	allowed := map[string]bool{"DOMAIN": true, "DOMAIN_URL": true, "DEPLOYMENT_ID": true, "CPUS": true, "MEMORY_MB": true, "TARGET_PORT": true, "HOST_PORT": true}
	for name := range p.Vars {
		if !allowed[name] {
			return fmt.Errorf("fork preview refuses non-platform variable %s", name)
		}
	}
	if len(p.Routes) != 1 || p.Routes[0].Hostname != "" || p.Routes[0].Onion == nil || !p.Routes[0].Onion.Enabled {
		return errors.New("fork preview requires one onion-only route")
	}
	if p.Manifest.Lifecycle.Rollback != "" || p.Manifest.Lifecycle.Backup != "" || p.Manifest.Lifecycle.Install != "" || p.Manifest.Lifecycle.Update != "" || p.Manifest.Lifecycle.Uninstall != "" || p.Manifest.Lifecycle.Health != "" || p.Manifest.Runtime.BackupDatabase != nil || p.Manifest.Runtime.RestoreDatabase != nil {
		return errors.New("fork preview refuses host lifecycle hooks and database operations")
	}
	return nil
}

var forkBaseImage = regexp.MustCompile(`^(?:docker.io/library/)?(?:alpine|nginx|node|python|golang|php|busybox|debian|ubuntu)(?::[a-zA-Z0-9_.-]+|@sha256:[a-f0-9]{64})?$`)

// No custom frontend or private registry can receive Docker's host credentials.
// RUN uses Compose network=none; Docker's normal unprivileged build entitlements
// remain in force. Only public official bases and previous named stages are allowed.
func validateForkDockerfile(appDir string, build *sdkclient.BuildContext) error {
	rel := build.DockerfilePath
	if rel == "" {
		rel = "Dockerfile"
	}
	if filepath.IsAbs(rel) || strings.Contains(rel, "..") {
		return errors.New("invalid fork Dockerfile path")
	}
	raw, err := os.ReadFile(filepath.Join(appDir, "build-ctx", filepath.FromSlash(rel)))
	if err != nil || len(raw) > 1<<20 {
		return errors.New("fork Dockerfile unavailable or too large")
	}
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	text = strings.ReplaceAll(text, "\\\n", " ")
	stages := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		lower := strings.ToLower(line)
		if regexp.MustCompile(`^#\s*(?:syntax|escape)\s*=`).MatchString(lower) {
			return errors.New("fork Dockerfile refuses custom frontend directives")
		}
		fields := strings.Fields(line)
		if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		switch strings.ToUpper(fields[0]) {
		case "FROM":
			if len(fields) != 2 && (len(fields) != 4 || !strings.EqualFold(fields[2], "as")) {
				return errors.New("fork Dockerfile requires a fixed official base image")
			}
			if fields[1] != "scratch" && !forkBaseImage.MatchString(fields[1]) && !stages[fields[1]] {
				return errors.New("fork Dockerfile refuses private or custom base images")
			}
			if len(fields) == 4 {
				stages[fields[3]] = true
			}
		case "VOLUME", "ADD":
			return errors.New("fork Dockerfile refuses persistent volumes and remote ADD; use COPY")
		case "COPY":
			for _, field := range fields[1:] {
				if field == "--from" {
					return errors.New("fork Dockerfile COPY requires a fixed stage flag")
				}
				if strings.HasPrefix(field, "--from=") {
					stage := strings.TrimPrefix(field, "--from=")
					if !stages[stage] && !regexp.MustCompile(`^[0-9]+$`).MatchString(stage) {
						return errors.New("fork Dockerfile COPY source must be an earlier stage")
					}
				}
			}
		}
	}
	return nil
}
