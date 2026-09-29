// Package dockertest is a Docker CLI double for the agent's tests; only
// test files import it. A `docker` shim first on PATH re-executes the
// running test binary, which answers from a JSON state file instead of a
// daemon. `--format` templates run through text/template over structs with
// Docker's field names, the way the CLI evaluates them, so a test pins what
// the agent does rather than the exact command it sends. `docker compose`
// resolves its project with Compose's precedence (-p, COMPOSE_PROJECT_NAME
// from the environment or the .env, the file's top-level name, the
// directory name) and `compose down` removes that project's containers, so
// acting on the wrong project shows up as another app's containers gone.
// Every invocation is recorded with its working directory. It runs on Unix
// (a shell shim) and on Windows (the test binary as docker.exe).
package dockertest

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
	"text/template"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

const stateEnv = "IMPREZA_DOCKERTEST_STATE"

// State is everything the double knows. Tests seed it and read it back.
type State struct {
	Containers []Container
	Networks   []string
	// Stats answers `docker stats --no-stream`, by container name.
	Stats map[string]Stats
	// ComposeConfig answers `docker compose config --format json` for the
	// project directory it is keyed by: the resolved model Compose prints.
	ComposeConfig map[string]string
	// MissingImages makes `docker image inspect` fail for these references;
	// any other image exists.
	MissingImages []string
	// Volumes answers `docker volume ls` and `docker system df -v`.
	Volumes []Volume
	// ImageEnv answers `docker image inspect --format {{json .Config.Env}}`
	// for these image references: the pinned caddy image's defaults.
	ImageEnv map[string][]string
	Calls    []Call
}

// Volume is one named volume: its labels for `volume ls --filter` and the
// size `system df -v` prints.
type Volume struct {
	Name   string
	Labels map[string]string
	Size   string
}

// Call is one recorded invocation.
type Call struct {
	Dir  string
	Args []string
}

// Container has the `docker inspect` field names the agent's templates use.
type Container struct {
	ID           string
	Name         string // with the leading slash, as Docker reports it
	Image        string // image ID
	RestartCount int
	State        ContainerState
	Config       ContainerConfig
	HostConfig   HostConfig
	Mounts       []Mount
}

type ContainerState struct {
	Status   string
	ExitCode int
	Health   *Health
}

type Health struct{ Status string }

type ContainerConfig struct {
	Image  string
	Env    []string
	Labels map[string]string
}

type HostConfig struct {
	Binds  []string
	Memory int64 // the container's own limit, 0 when it has none
}

// Mount is one entry of the container's Mounts, as inspect lists them.
type Mount struct {
	Type        string
	Source      string
	Destination string
}

// Stats is one `docker stats` row.
type Stats struct {
	CPUPerc  string
	MemUsage string
}

// ComposeContainer builds a container the way Compose labels one.
func ComposeContainer(project, service, status string, exitCode int) Container {
	return Container{
		ID:    newID(),
		Name:  "/" + project + "-" + service + "-1",
		Image: imageID(service),
		State: ContainerState{Status: status, ExitCode: exitCode},
		Config: ContainerConfig{Image: service, Labels: map[string]string{
			"com.docker.compose.project": project,
			"com.docker.compose.service": service,
			"com.docker.compose.oneoff":  "False",
		}},
	}
}

// Double is one installed shim and its state file.
type Double struct {
	t    *testing.T
	path string
}

// Install puts the docker shim first on PATH, answering from state. On Unix
// the shim is a shell script that sets the state path for the one exec; on
// Windows, where a batch file would mangle the templates' quotes and pipes,
// the test binary itself becomes docker.exe and the state path goes in the
// test's environment. That only reaches processes the test starts: MaybeRun
// runs at init, before the test sets it. A test that re-executes its own
// binary for another purpose (the preparation worker) must not Install.
func Install(t *testing.T, state State) *Double {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	d := &Double{t: t, path: filepath.Join(dir, "state.json")}
	d.Save(state)
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "windows" {
		// A copy, not a link: Windows refuses to remove a link to the image
		// of the running test, and the temporary directory must go away.
		raw, err := os.ReadFile(self)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(bin, "docker.exe"), raw, 0o700); err != nil {
			t.Fatal(err)
		}
		t.Setenv(stateEnv, d.path)
	} else {
		shim := "#!/bin/sh\n" + stateEnv + "=" + shellQuote(d.path) + " exec " + shellQuote(self) + ` "$@"` + "\n"
		if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(shim), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return d
}

// State reads the double's current state.
func (d *Double) State() State {
	d.t.Helper()
	s, err := load(d.path)
	if err != nil {
		d.t.Fatal(err)
	}
	return s
}

// Save replaces the double's state.
func (d *Double) Save(s State) {
	d.t.Helper()
	if err := save(d.path, s); err != nil {
		d.t.Fatal(err)
	}
}

// Container returns the container with this name (without the slash).
func (d *Double) Container(name string) (Container, bool) {
	s := d.State()
	if c := s.find(name); c != nil {
		return *c, true
	}
	return Container{}, false
}

// Calls returns the recorded invocations whose arguments start with prefix.
func (d *Double) Calls(prefix ...string) []Call {
	var out []Call
	for _, c := range d.State().Calls {
		if len(c.Args) >= len(prefix) && equal(c.Args[:len(prefix)], prefix) {
			out = append(out, c)
		}
	}
	return out
}

// MaybeRun acts as docker when this process was started through the shim,
// and never returns then. Call it from an init() in the test package.
func MaybeRun() {
	path := os.Getenv(stateEnv)
	if path == "" {
		return
	}
	os.Exit(run(path, os.Args[1:]))
}

func run(path string, args []string) int {
	s, err := load(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dockertest:", err)
		return 2
	}
	dir, _ := os.Getwd()
	s.Calls = append(s.Calls, Call{Dir: dir, Args: append([]string{}, args...)})
	code := s.dispatch(dir, args)
	if err := save(path, s); err != nil {
		fmt.Fprintln(os.Stderr, "dockertest:", err)
		return 2
	}
	return code
}

func (s *State) dispatch(dir string, args []string) int {
	if len(args) == 0 {
		return 0
	}
	switch args[0] {
	case "inspect":
		return s.inspect(args[1:])
	case "ps":
		return s.ps(args[1:])
	case "container":
		if len(args) > 1 {
			switch args[1] {
			case "ls", "list", "ps":
				return s.ps(args[2:])
			case "inspect":
				return s.inspect(args[2:])
			}
		}
	case "run":
		return s.runContainer(args[1:])
	case "rm":
		return s.rm(args[1:])
	case "start", "restart":
		return s.setStatus(args[1:], "running")
	case "stop", "kill":
		return s.setStatus(args[1:], "exited")
	case "network":
		return s.network(args[1:])
	case "stats":
		return s.stats(args[1:])
	case "system":
		if len(args) > 1 && args[1] == "df" {
			type dfVolume struct{ Name, Size string }
			volumes := []dfVolume{}
			for _, v := range s.Volumes {
				volumes = append(volumes, dfVolume{Name: v.Name, Size: v.Size})
			}
			return s.format(args[2:], struct{ Volumes []dfVolume }{Volumes: volumes})
		}
	case "volume":
		if len(args) > 1 && (args[1] == "ls" || args[1] == "list") {
			return s.volumes(args[2:])
		}
	case "image":
		if len(args) > 1 && args[1] == "inspect" {
			for _, ref := range args[2:] {
				if strings.HasPrefix(ref, "-") {
					continue
				}
				for _, missing := range s.MissingImages {
					if ref == missing {
						fmt.Fprintf(os.Stderr, "Error response from daemon: No such image: %s\n", ref)
						return 1
					}
				}
			}
			// The pinned image's default environment, when the test provides
			// it (ImageEnv); anything else answers with an empty object, as
			// before.
			format := ""
			var refs []string
			for i := 2; i < len(args); i++ {
				if v, ok := flagValue(args, &i, "--format", "-f"); ok {
					format = v
				} else if !strings.HasPrefix(args[i], "-") {
					refs = append(refs, args[i])
				}
			}
			if format == "{{json .Config.Env}}" && len(refs) == 1 {
				if env, ok := s.ImageEnv[refs[0]]; ok {
					raw, _ := json.Marshal(env)
					fmt.Println(string(raw))
					return 0
				}
			}
			fmt.Println("[{}]")
		}
	case "compose":
		return s.compose(dir, args[1:])
	}
	return 0
}

// ── plain docker ────────────────────────────────────────────────────────

func (s *State) find(ref string) *Container {
	for i := range s.Containers {
		c := &s.Containers[i]
		if c.Name == "/"+ref || c.Name == ref || c.ID == ref || (len(ref) >= 4 && strings.HasPrefix(c.ID, ref)) {
			return c
		}
	}
	return nil
}

func (s *State) remove(ref string) {
	for i := range s.Containers {
		if c := &s.Containers[i]; c.Name == "/"+ref || c.Name == ref || c.ID == ref || (len(ref) >= 4 && strings.HasPrefix(c.ID, ref)) {
			s.Containers = append(s.Containers[:i], s.Containers[i+1:]...)
			return
		}
	}
}

var templateFuncs = template.FuncMap{
	"json": func(v any) (string, error) {
		raw, err := json.Marshal(v)
		return string(raw), err
	},
	"join":  strings.Join,
	"split": strings.Split,
	"lower": strings.ToLower,
	"upper": strings.ToUpper,
}

func render(format string, v any) (string, error) {
	tmpl, err := template.New("").Funcs(templateFuncs).Parse(format)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, v); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// flagValue reads `--name value` and `--name=value` forms.
func flagValue(args []string, i *int, names ...string) (string, bool) {
	a := args[*i]
	for _, n := range names {
		if a == n && *i+1 < len(args) {
			*i++
			return args[*i], true
		}
		if strings.HasPrefix(n, "--") && strings.HasPrefix(a, n+"=") {
			return strings.TrimPrefix(a, n+"="), true
		}
	}
	return "", false
}

func (s *State) inspect(args []string) int {
	format := ""
	var refs []string
	for i := 0; i < len(args); i++ {
		if v, ok := flagValue(args, &i, "--format", "-f"); ok {
			format = v
		} else if _, ok := flagValue(args, &i, "--type"); ok {
		} else if args[i] == "-s" || args[i] == "--size" {
		} else {
			refs = append(refs, args[i])
		}
	}
	code := 0
	var found []Container
	for _, ref := range refs {
		if c := s.find(ref); c != nil {
			found = append(found, *c)
		} else {
			fmt.Fprintf(os.Stderr, "Error: No such object: %s\n", ref)
			code = 1
		}
	}
	if format == "" {
		raw, _ := json.MarshalIndent(found, "", "    ")
		fmt.Println(string(raw))
		return code
	}
	for _, c := range found {
		out, err := render(format, c)
		if err != nil {
			fmt.Fprintln(os.Stderr, "template parsing error:", err)
			return 1
		}
		fmt.Println(out)
	}
	return code
}

type psRow struct {
	ID     string
	Names  string
	Image  string
	State  string
	Labels string
}

func (s *State) ps(args []string) int {
	all, quiet, noTrunc, format := false, false, false, ""
	var filters []string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-a" || a == "--all":
			all = true
		case a == "-q" || a == "--quiet":
			quiet = true
		case a == "-aq" || a == "-qa":
			all, quiet = true, true
		case a == "--no-trunc":
			noTrunc = true
		default:
			if v, ok := flagValue(args, &i, "--filter", "-f"); ok {
				filters = append(filters, v)
			} else if v, ok := flagValue(args, &i, "--format"); ok {
				format = v
			}
		}
	}
	for _, c := range s.Containers {
		if (!all && c.State.Status != "running") || !matches(c, filters) {
			continue
		}
		id := c.ID
		if !noTrunc {
			id = id[:12]
		}
		if quiet || format == "" {
			fmt.Println(id)
			continue
		}
		var labels []string
		for k, v := range c.Config.Labels {
			labels = append(labels, k+"="+v)
		}
		sort.Strings(labels)
		out, err := render(format, psRow{ID: id, Names: strings.TrimPrefix(c.Name, "/"), Image: c.Config.Image, State: c.State.Status, Labels: strings.Join(labels, ",")})
		if err != nil {
			fmt.Fprintln(os.Stderr, "template parsing error:", err)
			return 1
		}
		fmt.Println(out)
	}
	return 0
}

func matches(c Container, filters []string) bool {
	for _, f := range filters {
		kind, value, _ := strings.Cut(f, "=")
		switch kind {
		case "label":
			key, want, hasValue := strings.Cut(value, "=")
			got, ok := c.Config.Labels[key]
			if !ok || (hasValue && got != want) {
				return false
			}
		case "name":
			if ok, _ := regexp.MatchString(value, c.Name); !ok {
				return false
			}
		case "status":
			if c.State.Status != value {
				return false
			}
		}
	}
	return true
}

var runValueFlags = map[string]bool{
	"--name": true, "--restart": true, "--network": true, "--net": true, "-p": true, "--publish": true,
	"--env-file": true, "-e": true, "--env": true, "-v": true, "--volume": true, "--label": true, "-l": true,
	"-u": true, "--user": true, "-m": true, "--memory": true, "--cap-drop": true, "--cap-add": true,
	"--security-opt": true, "--entrypoint": true, "-w": true, "--workdir": true, "--hostname": true,
	"--tmpfs": true, "--pids-limit": true, "--cpus": true, "--log-driver": true, "--log-opt": true,
	"--mount": true, "--add-host": true, "--dns": true, "--ulimit": true, "--stop-timeout": true,
}

func (s *State) runContainer(args []string) int {
	c := Container{ID: newID(), State: ContainerState{Status: "running"}, Config: ContainerConfig{
		Env:    []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"},
		Labels: map[string]string{},
	}}
	i := 0
	for ; i < len(args) && strings.HasPrefix(args[i], "-"); i++ {
		name, value, inline := strings.Cut(args[i], "=")
		if !strings.HasPrefix(name, "--") {
			name, inline = args[i], false
		}
		if !runValueFlags[name] {
			continue
		}
		if !inline {
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "flag needs an argument:", name)
				return 125
			}
			i++
			value = args[i]
		}
		switch name {
		case "--name":
			c.Name = "/" + value
		case "--env-file":
			entries, err := readEnvFile(value)
			if err != nil {
				fmt.Fprintln(os.Stderr, "docker:", err)
				return 125
			}
			c.Config.Env = setEnv(c.Config.Env, entries...)
		case "-e", "--env":
			c.Config.Env = setEnv(c.Config.Env, value)
		case "-v", "--volume":
			c.HostConfig.Binds = append(c.HostConfig.Binds, value)
		case "--label", "-l":
			k, v, _ := strings.Cut(value, "=")
			c.Config.Labels[k] = v
		}
	}
	if i >= len(args) {
		fmt.Fprintln(os.Stderr, "docker: \"run\" requires at least 1 argument.")
		return 125
	}
	c.Config.Image = args[i]
	c.Image = imageID(args[i])
	if c.Name == "" {
		c.Name = "/container-" + c.ID[:8]
	}
	if s.find(strings.TrimPrefix(c.Name, "/")) != nil {
		fmt.Fprintf(os.Stderr, "docker: Error response from daemon: Conflict. The container name %q is already in use.\n", c.Name)
		return 125
	}
	s.Containers = append(s.Containers, c)
	fmt.Println(c.ID)
	return 0
}

// setEnv applies entries over an environment, the last one winning.
func setEnv(env []string, entries ...string) []string {
	for _, entry := range entries {
		key, _, _ := strings.Cut(entry, "=")
		replaced := false
		for i, cur := range env {
			if k, _, _ := strings.Cut(cur, "="); k == key {
				env[i], replaced = entry, true
			}
		}
		if !replaced {
			env = append(env, entry)
		}
	}
	return env
}

// readEnvFile follows the Docker CLI's env-file reader.
func readEnvFile(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var entries []string
	scanner := bufio.NewScanner(f)
	for n := 1; scanner.Scan(); n++ {
		raw := scanner.Bytes()
		if !utf8.Valid(raw) {
			return nil, fmt.Errorf("env file %s contains invalid utf8 bytes at line %d", path, n)
		}
		if n == 1 {
			raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})
		}
		line := strings.TrimLeftFunc(string(raw), unicode.IsSpace)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, hasValue := strings.Cut(line, "=")
		key = strings.TrimLeft(key, " \t")
		if key == "" || strings.ContainsAny(key, " \t") {
			return nil, fmt.Errorf("variable %q is invalid", key)
		}
		if hasValue {
			entries = append(entries, key+"="+value)
		} else if value, ok := os.LookupEnv(key); ok {
			entries = append(entries, key+"="+value)
		}
	}
	return entries, scanner.Err()
}

func (s *State) rm(args []string) int {
	force, code := false, 0
	for _, a := range args {
		switch a {
		case "-f", "--force":
			force = true
		case "-v", "--volumes", "-fv", "-vf":
			force = force || strings.Contains(a, "f")
		default:
			c := s.find(a)
			if c == nil {
				fmt.Fprintf(os.Stderr, "Error response from daemon: No such container: %s\n", a)
				code = 1
				continue
			}
			if c.State.Status == "running" && !force {
				fmt.Fprintf(os.Stderr, "Error response from daemon: cannot remove container %q: container is running\n", c.Name)
				code = 1
				continue
			}
			s.remove(a)
			fmt.Println(a)
		}
	}
	return code
}

func (s *State) setStatus(args []string, status string) int {
	code := 0
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			continue
		}
		c := s.find(a)
		if c == nil {
			fmt.Fprintf(os.Stderr, "Error response from daemon: No such container: %s\n", a)
			code = 1
			continue
		}
		c.State.Status, c.State.ExitCode = status, 0
		fmt.Println(a)
	}
	return code
}

func (s *State) network(args []string) int {
	if len(args) == 0 {
		return 0
	}
	has := func(name string) int {
		for i, n := range s.Networks {
			if n == name {
				return i
			}
		}
		return -1
	}
	switch args[0] {
	case "ls":
		for _, a := range args[1:] {
			if a == "--filter" || a == "-f" || strings.HasPrefix(a, "--filter=") {
				return 0 // networks carry no labels here
			}
		}
		for _, n := range s.Networks {
			fmt.Println(n)
		}
	case "inspect":
		if len(args) < 2 || has(args[len(args)-1]) < 0 {
			fmt.Fprintln(os.Stderr, "Error response from daemon: network not found")
			return 1
		}
		fmt.Println("[{}]")
	case "create":
		name := args[len(args)-1]
		if has(name) < 0 {
			s.Networks = append(s.Networks, name)
		}
		fmt.Println(newID())
	case "rm":
		for _, name := range args[1:] {
			if i := has(name); i >= 0 {
				s.Networks = append(s.Networks[:i], s.Networks[i+1:]...)
			}
		}
	}
	return 0
}

type statsRow struct {
	ID       string
	Name     string
	CPUPerc  string
	MemUsage string
}

func (s *State) stats(args []string) int {
	format := ""
	var refs []string
	for i := 0; i < len(args); i++ {
		if v, ok := flagValue(args, &i, "--format"); ok {
			format = v
		} else if !strings.HasPrefix(args[i], "-") {
			refs = append(refs, args[i])
		}
	}
	for _, ref := range refs {
		c := s.find(ref)
		if c == nil {
			fmt.Fprintf(os.Stderr, "Error response from daemon: No such container: %s\n", ref)
			return 1
		}
		name := strings.TrimPrefix(c.Name, "/")
		row := statsRow{ID: c.ID[:12], Name: name, CPUPerc: "0.00%", MemUsage: "0B / 0B"}
		if c.State.Status == "running" {
			row.CPUPerc, row.MemUsage = "0.50%", "10MiB / 1GiB"
		}
		if st, ok := s.Stats[name]; ok {
			row.CPUPerc, row.MemUsage = st.CPUPerc, st.MemUsage
		}
		if format == "" {
			format = "{{.Name}} {{.CPUPerc}} {{.MemUsage}}"
		}
		out, err := render(format, row)
		if err != nil {
			fmt.Fprintln(os.Stderr, "template parsing error:", err)
			return 1
		}
		fmt.Println(out)
	}
	return 0
}

// volumes lists volume names; a name filter matches a substring, as Docker's does.
func (s *State) volumes(args []string) int {
	var filters []string
	for i := 0; i < len(args); i++ {
		if v, ok := flagValue(args, &i, "--filter", "-f"); ok {
			filters = append(filters, v)
		}
	}
	for _, v := range s.Volumes {
		keep := true
		for _, f := range filters {
			kind, value, _ := strings.Cut(f, "=")
			switch kind {
			case "label":
				key, want, hasValue := strings.Cut(value, "=")
				got, ok := v.Labels[key]
				keep = keep && ok && (!hasValue || got == want)
			case "name":
				keep = keep && strings.Contains(v.Name, value)
			}
		}
		if keep {
			fmt.Println(v.Name)
		}
	}
	return 0
}

func (s *State) format(args []string, v any) int {
	for i := 0; i < len(args); i++ {
		if format, ok := flagValue(args, &i, "--format"); ok {
			out, err := render(format, v)
			if err != nil {
				fmt.Fprintln(os.Stderr, "template parsing error:", err)
				return 1
			}
			fmt.Println(out)
		}
	}
	return 0
}

// ── compose ─────────────────────────────────────────────────────────────

// composeProject is what Compose resolved for one invocation.
type composeProject struct {
	Name     string
	Dir      string
	Files    []string
	Services []string
}

// resolve follows Compose's precedence for the file and the project name.
func resolveCompose(cwd string, global map[string][]string) (composeProject, error) {
	cwdEnv := readDotenv(filepath.Join(cwd, ".env"))
	lookup := func(env map[string]string, key string) string {
		if v, ok := os.LookupEnv(key); ok {
			return v
		}
		return env[key]
	}
	p := composeProject{Dir: cwd}
	for _, f := range global["-f"] {
		if !filepath.IsAbs(f) {
			f = filepath.Join(cwd, f)
		}
		p.Files = append(p.Files, f)
	}
	if len(p.Files) == 0 {
		if v := lookup(cwdEnv, "COMPOSE_FILE"); v != "" {
			for _, f := range strings.Split(v, string(os.PathListSeparator)) {
				if !filepath.IsAbs(f) {
					f = filepath.Join(cwd, f)
				}
				p.Files = append(p.Files, f)
			}
		} else {
			for _, name := range []string{"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"} {
				if _, err := os.Stat(filepath.Join(cwd, name)); err == nil {
					p.Files = []string{filepath.Join(cwd, name)}
					break
				}
			}
		}
	}
	if len(p.Files) > 0 {
		p.Dir = filepath.Dir(p.Files[0])
	}
	projectEnv := readDotenv(filepath.Join(p.Dir, ".env"))
	yamlName := ""
	for _, f := range p.Files {
		raw, err := os.ReadFile(f)
		if err != nil {
			return p, err
		}
		var doc struct {
			Name     string               `yaml:"name"`
			Services map[string]yaml.Node `yaml:"services"`
		}
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			return p, err
		}
		if doc.Name != "" {
			yamlName = interpolate(doc.Name, projectEnv)
		}
		for name := range doc.Services {
			p.Services = append(p.Services, name)
		}
	}
	sort.Strings(p.Services)
	switch {
	case len(global["-p"]) > 0:
		p.Name = global["-p"][len(global["-p"])-1]
	case lookup(cwdEnv, "COMPOSE_PROJECT_NAME") != "":
		p.Name = lookup(cwdEnv, "COMPOSE_PROJECT_NAME")
	case normalizeProject(yamlName) != "":
		p.Name = normalizeProject(yamlName)
	default:
		p.Name = normalizeProject(filepath.Base(p.Dir))
	}
	return p, nil
}

func (s *State) compose(cwd string, args []string) int {
	global := map[string][]string{}
	i := 0
	for ; i < len(args) && strings.HasPrefix(args[i], "-"); i++ {
		if v, ok := flagValue(args, &i, "-p", "--project-name"); ok {
			global["-p"] = append(global["-p"], v)
		} else if v, ok := flagValue(args, &i, "-f", "--file"); ok {
			global["-f"] = append(global["-f"], v)
		} else if _, ok := flagValue(args, &i, "--project-directory", "--env-file", "--profile", "--ansi", "--progress", "--parallel"); ok {
		}
	}
	if i >= len(args) {
		return 0
	}
	sub, rest := args[i], args[i+1:]
	p, err := resolveCompose(cwd, global)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	switch sub {
	case "config":
		for _, a := range rest {
			if a == "--services" {
				for _, service := range p.Services {
					fmt.Println(service)
				}
				return 0
			}
		}
		if model, ok := s.ComposeConfig[p.Dir]; ok {
			fmt.Println(model)
			return 0
		}
		raw, _ := json.Marshal(map[string]any{"name": p.Name, "services": map[string]any{}})
		fmt.Println(string(raw))
	case "down":
		var kept []Container
		for _, c := range s.Containers {
			if c.Config.Labels["com.docker.compose.project"] != p.Name {
				kept = append(kept, c)
			}
		}
		s.Containers = kept
	case "stop", "restart", "start":
		status := "running"
		if sub == "stop" {
			status = "exited"
		}
		for i := range s.Containers {
			if s.Containers[i].Config.Labels["com.docker.compose.project"] == p.Name {
				s.Containers[i].State.Status, s.Containers[i].State.ExitCode = status, 0
			}
		}
	}
	return 0
}

// readDotenv reads the KEY=VALUE lines of a Compose .env, quotes removed.
func readDotenv(path string) map[string]string {
	env := map[string]string{}
	raw, err := os.ReadFile(path)
	if err != nil {
		return env
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "export "))
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.HasPrefix(key, "#") {
			continue
		}
		value = strings.TrimSpace(value)
		switch {
		case len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'':
			value = value[1 : len(value)-1]
		case len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"':
			value = strings.NewReplacer(`\\`, `\`, `\"`, `"`, "$$", "$").Replace(value[1 : len(value)-1])
		}
		env[strings.TrimSpace(key)] = value
	}
	return env
}

var interpolation = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::?-([^}]*))?\}|\$([A-Za-z_][A-Za-z0-9_]*)`)

func interpolate(s string, env map[string]string) string {
	return interpolation.ReplaceAllStringFunc(s, func(m string) string {
		parts := interpolation.FindStringSubmatch(m)
		key := parts[1] + parts[3]
		if v, ok := os.LookupEnv(key); ok {
			return v
		}
		if v, ok := env[key]; ok && v != "" {
			return v
		}
		return parts[2]
	})
}

// normalizeProject is Compose's project name normalization.
func normalizeProject(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			b.WriteRune(r)
		}
	}
	return strings.TrimLeft(b.String(), "_-")
}

// ── helpers ─────────────────────────────────────────────────────────────

func load(path string) (State, error) {
	var s State
	raw, err := os.ReadFile(path)
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(raw, &s)
}

func save(path string, s State) error {
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func newID() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func imageID(ref string) string {
	sum := sha256.Sum256([]byte(ref))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
