package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// prepareDeployImages deliberately cannot start or remove a container.
// Build services must not be pulled: their image can exist only locally.
// Always run build so manifests with inline build definitions are covered too.
func prepareDeployImages(ctx context.Context, compose func(context.Context, ...string) ([]byte, error)) error {
	steps := [][]string{{"config", "--quiet"}, {"pull", "--ignore-buildable"}, {"build"}}
	for _, args := range steps {
		budget := composeQueryTimeout
		if args[0] == "pull" {
			budget = composePullTimeout
		}
		if args[0] == "build" {
			budget = composeBuildTimeout
		}
		stepCtx, cancel := context.WithTimeout(ctx, budget)
		out, err := compose(stepCtx, args...)
		cancel()
		if err != nil {
			return fmt.Errorf("docker compose %s (preparation): %w\n%s", args[0], err, tail(out, 1536))
		}
	}
	return nil
}

type deployConfigFile struct {
	name   string
	data   []byte
	mode   os.FileMode
	exists bool
}

type deployConfigSnapshot []deployConfigFile

func captureDeployConfig(dir string, enabled bool) (deployConfigSnapshot, error) {
	if !enabled {
		return nil, nil
	}
	var snapshot deployConfigSnapshot
	for _, name := range []string{"compose.yaml", ".env", "startup.json"} {
		path := filepath.Join(dir, name)
		info, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			snapshot = append(snapshot, deployConfigFile{name: name})
			continue
		}
		if err != nil {
			return nil, err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		snapshot = append(snapshot, deployConfigFile{name: name, data: data, mode: info.Mode().Perm(), exists: true})
	}
	return snapshot, nil
}

func (snapshot deployConfigSnapshot) restore(dir string) error {
	var failures []error
	for _, file := range snapshot {
		path := filepath.Join(dir, file.name)
		var err error
		if file.exists {
			err = writeAtomic(path, file.data, file.mode)
		} else {
			err = os.Remove(path)
			if errors.Is(err, os.ErrNotExist) {
				err = nil
			}
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", file.name, err))
		}
	}
	return errors.Join(failures...)
}
