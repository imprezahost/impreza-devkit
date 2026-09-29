package executor

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// composeFileName is the only Compose file an app directory has: every
// deploy writes it, and nothing else does.
const composeFileName = "compose.yaml"

// managedComposeProject is the shape of a project the agent names itself:
// an app's, or one of its internal jobs'.
var managedComposeProject = regexp.MustCompile(`^(?:dpl|bkpjob|rstjob|tskjob|rdjob|clijob|pitrjob)_`)

// composeCommand prefixes a Compose subcommand with the app directory's
// pinned project and file. Without them Compose took both from the
// environment, the .env included: COMPOSE_PROJECT_NAME or COMPOSE_FILE
// among an app's variables pointed its Compose at another app of the same
// host, and that app's uninstall (`down --volumes`) removed the other
// app's containers and volumes.
func composeCommand(appDir string, args ...string) ([]string, error) {
	project, err := composeProject(appDir)
	if err != nil {
		return nil, err
	}
	return append([]string{"compose", "-p", project, "-f", filepath.Join(appDir, composeFileName)}, args...), nil
}

// composeProject is the project Compose itself derived for the directory
// before it was pinned, so existing stacks keep their containers and named
// volumes: the literal top-level name of compose.yaml when it declares one,
// else the directory name, normalized as Compose normalizes both. No
// variable takes part. A name built from one is refused, since Compose
// would interpolate it from the .env, and so is a name shaped like another
// app's or an internal job's project. A file that is not a YAML mapping, or
// a name that is not text, keeps the directory name: Compose refuses a file
// that is not a mapping, and under -p it never reads the name.
func composeProject(appDir string) (string, error) {
	own := normalizeComposeProject(filepath.Base(appDir))
	if own == "" {
		return "", errors.New("the app directory has no usable Compose project name")
	}
	raw, err := readComposeFile(filepath.Join(appDir, composeFileName))
	if errors.Is(err, os.ErrNotExist) {
		return own, nil
	}
	if err != nil {
		return "", err
	}
	var doc struct {
		Name yaml.Node `yaml:"name"`
	}
	if yaml.Unmarshal(raw, &doc) != nil {
		return own, nil
	}
	name := &doc.Name
	if name.Kind == yaml.AliasNode && name.Alias != nil {
		name = name.Alias
	}
	if name.Kind != yaml.ScalarNode || name.Tag != "!!str" {
		return own, nil
	}
	if strings.Contains(name.Value, "$") {
		return "", errors.New("the top-level name of compose.yaml is built from a variable, and a variable must never choose the Compose project an app acts on; remove the name or make it literal")
	}
	project := normalizeComposeProject(name.Value)
	if project == "" {
		// Compose falls back to the directory name.
		return own, nil
	}
	if project != own && managedComposeProject.MatchString(project) {
		return "", errors.New("the top-level name of compose.yaml is the Compose project of another app or job on this server")
	}
	return project, nil
}

func readComposeFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 1024*1024 {
		return nil, errors.New("compose.yaml is not a regular file within the 1 MiB limit")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 1024*1024+1))
	if err != nil || len(raw) > 1024*1024 {
		return nil, errors.New("compose.yaml could not be read within the 1 MiB limit")
	}
	return raw, nil
}

// normalizeComposeProject is Compose's own normalization: lower case, only
// [a-z0-9_-], no leading '_' or '-'.
func normalizeComposeProject(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			b.WriteRune(r)
		}
	}
	return strings.TrimLeft(b.String(), "_-")
}

var envVariableName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// validateEnvNames refuses the variables no app .env may carry: a
// name Compose or the Docker CLI reads for itself (COMPOSE_*, DOCKER_*, in
// any case, as the server refuses them) and a name that is not a plain
// variable name, which renderEnv would write verbatim — a newline in it
// became a second line of the .env. Engine names are named in the error;
// a malformed name is only counted, never echoed.
func validateEnvNames(vars map[string]any) error {
	var engine []string
	malformed := 0
	for key := range vars {
		upper := strings.ToUpper(key)
		switch {
		case !envVariableName.MatchString(key):
			malformed++
		case strings.HasPrefix(upper, "COMPOSE_"), strings.HasPrefix(upper, "DOCKER_"):
			engine = append(engine, key)
		}
	}
	if len(engine) == 0 && malformed == 0 {
		return nil
	}
	sort.Strings(engine)
	var named []string
	if len(engine) > 0 {
		named = append(named, strings.Join(engine, ", "))
	}
	if malformed == 1 {
		named = append(named, "1 invalid name")
	} else if malformed > 1 {
		named = append(named, fmt.Sprintf("%d invalid names", malformed))
	}
	return fmt.Errorf("the app variables include %s, which a deploy never carries: COMPOSE_* and DOCKER_* steer the container engine and can act on other apps of this server, and a name must be letters, digits and underscore. Nothing was changed; remove them from the app variables, then retry", strings.Join(named, " and "))
}
