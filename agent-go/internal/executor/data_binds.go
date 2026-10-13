package executor

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Previously, manifest.runtime.data_dir.owner was applied to ./data BEFORE
// `docker compose up`, and Docker then created the missing bind sources
// (./data/state, ./data/workspace, ...) as root:root. An image that runs as
// its own uid (OpenClaw, uid 1000) could not write its state and
// crash-looped. The agent now creates those bind sources itself, under
// ./data only, before the ownership pass, so the pass covers them.
//
// The ownership pass also stops following symbolic links: the data tree is
// written by the containers, and a link planted there (./data/x ->
// /etc/shadow) must never make a redeploy chown a host file.

// dataBindSources returns the relative bind-mount sources of composeYAML
// that resolve strictly inside appDir/data, cleaned and sorted. Named
// volumes, absolute paths, interpolated paths (`$`), and anything that
// leaves ./data are ignored: the agent only prepares what it owns.
func dataBindSources(appDir, composeYAML string) ([]string, error) {
	var doc struct {
		Services map[string]struct {
			Volumes []yaml.Node `yaml:"volumes"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(composeYAML), &doc); err != nil {
		return nil, fmt.Errorf("parse compose for bind sources: %w", err)
	}
	dataDir := filepath.Join(appDir, "data")
	seen := map[string]bool{}
	for _, svc := range doc.Services {
		for _, v := range svc.Volumes {
			src := ""
			switch v.Kind {
			case yaml.ScalarNode:
				// short syntax: SOURCE:TARGET[:MODE]
				parts := strings.SplitN(v.Value, ":", 2)
				if len(parts) == 2 {
					src = parts[0]
				}
			case yaml.MappingNode:
				var long struct {
					Type   string `yaml:"type"`
					Source string `yaml:"source"`
				}
				if err := v.Decode(&long); err == nil && long.Type == "bind" {
					src = long.Source
				}
			}
			if !strings.HasPrefix(src, "./") || strings.Contains(src, "$") {
				continue
			}
			abs := filepath.Join(appDir, filepath.FromSlash(src))
			rel, err := filepath.Rel(dataDir, abs)
			if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
				continue
			}
			seen[abs] = true
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}

// prepareDataBinds creates each bind source inside dataDir that does not
// exist yet, one path component at a time, refusing any component that is
// a symbolic link or not a directory. An existing source is left as it is
// (it may be a file bind).
func prepareDataBinds(dataDir string, sources []string) error {
	for _, src := range sources {
		rel, err := filepath.Rel(dataDir, src)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("bind source %s is outside the data directory", src)
		}
		if err := ensureBindSource(dataDir, src, strings.Split(rel, string(filepath.Separator))); err != nil {
			return err
		}
	}
	return nil
}

// lchown is the ownership call of the data pass (a seam for tests that
// cannot run as root). It never follows a symbolic link.
var lchown = os.Lchown
