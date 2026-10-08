// Package hostinventory collects a bounded allowlist of facts. Commands are
// compiled arguments with a clean environment; their stderr and raw output are
// never logged or transmitted. No shell, package refresh or private key read.
package hostinventory

import (
	"bytes"
	"context"
	"errors"
	sdk "github.com/imprezahost/impreza-devkit/sdk-go/client"
	"io"
	"os"
	"os/exec"

	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

const Limit = 128 << 10

var token = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.+:~/-]{0,127}$`)
var version = regexp.MustCompile(`\bv?([0-9]+\.[0-9]+\.[0-9]+(?:[.-][A-Za-z0-9]+)*)\b`)
var uuid = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)
var units = []string{"impreza-agent.service", "docker.service", "tor.service", "impreza-agent-ingress.service"}

// Reader is injectable for negative controls. Production uses fixed paths and
// executables only. It never accepts a path or command from a request.
type Reader interface {
	Read(string) ([]byte, error)
	Run(context.Context, string, ...string) ([]byte, error)
	FS(string) (map[string]uint64, error)
}
type local struct{}

func (local) Read(path string) ([]byte, error) {
	f, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, Limit+1))
	if len(b) > Limit {
		return nil, errors.New("bounded output exceeded")
	}
	return b, e
}

type bounded struct{ bytes.Buffer }

func (b *bounded) Write(p []byte) (int, error) {
	if b.Len()+len(p) > Limit {
		return 0, errors.New("bounded output exceeded")
	}
	return b.Buffer.Write(p)
}
func (local) Run(ctx context.Context, path string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, path, args...)
	c.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LC_ALL=C", "LANG=C", "HOME=/nonexistent"}
	c.Stdin = nil
	var out bounded
	c.Stdout = &out
	c.Stderr = io.Discard
	if e := c.Run(); e != nil {
		return nil, errors.New("command failed")
	}
	return out.Bytes(), nil
}
func Collect(ctx context.Context) *sdk.HostInventory {
	return CollectWith(ctx, local{}, time.Now().UTC(), runtime.GOARCH)
}
func CollectWith(ctx context.Context, r Reader, at time.Time, arch string) *sdk.HostInventory {
	report := &sdk.HostInventory{Protocol: sdk.HostInventoryProtocol, ObservedAt: at, Facts: map[string]sdk.InventoryFact{}}
	set := func(k, source, state, reason string, v any) {
		report.Facts[k] = sdk.InventoryFact{Source: source, ObservedAt: at, ValidUntil: at.Add(6 * time.Hour), State: state, Reason: reason, Value: v}
	}
	absent := func(k, source string) { set(k, source, "unavailable", "collection_failed", nil) }
	b, e := r.Read("/etc/os-release")
	osid, osver := "", ""
	if e == nil {
		for _, l := range strings.Split(string(b), "\n") {
			kv := strings.SplitN(l, "=", 2)
			if len(kv) != 2 {
				continue
			}
			v := strings.Trim(kv[1], `"`)
			if kv[0] == "ID" && token.MatchString(v) {
				osid = v
			}
			if kv[0] == "VERSION_ID" && regexp.MustCompile(`^[0-9]+(?:\.[0-9]+)*$`).MatchString(v) {
				osver = v
			}
		}
	}
	if osid != "" && osver != "" {
		if !contains([]string{"ubuntu", "debian", "almalinux", "rocky", "centos", "fedora", "rhel", "alpine"}, osid) {
			osid = "unknown"
		}
		state, reason := "complete", "observed"
		if !(osid == "ubuntu" && osver == "24.04" || osid == "debian" && osver == "12") {
			state, reason = "partial", "unsupported_os"
		}
		set("os", "os-release", state, reason, map[string]any{"id": osid, "version": osver, "architecture": arch})
	} else {
		absent("os", "os-release")
	}
	b, e = r.Read("/proc/sys/kernel/osrelease")
	kv := strings.TrimSpace(string(b))
	if e == nil && token.MatchString(kv) {
		set("kernel_loaded", "proc-kernel", "complete", "observed", map[string]any{"version": kv})
	} else {
		absent("kernel_loaded", "proc-kernel")
	}
	b, e = r.Read("/proc/sys/kernel/random/boot_id")
	bid := strings.TrimSpace(string(b))
	if e == nil && uuid.MatchString(bid) {
		set("boot", "proc-boot", "complete", "observed", map[string]any{"id": bid})
	} else {
		absent("boot", "proc-boot")
	}
	// dpkg database query and cached APT list: no apt update, install, network,
	// autoremove, repo discovery, lock or cache file creation.
	b, e = r.Run(ctx, "/usr/bin/dpkg-query", "-W", "-f=${db:Status-Abbrev} ${binary:Package} ${Version}\n")
	count := 0
	kernels := []string{}
	if e == nil {
		for _, l := range strings.Split(string(b), "\n") {
			f := strings.Fields(l)
			if len(f) != 3 || f[0] != "ii" {
				continue
			}
			count++
			if regexp.MustCompile(`^linux-image-(?:unsigned-)?[0-9]`).MatchString(f[1]) {
				installed := strings.TrimPrefix(strings.TrimPrefix(f[1], "linux-image-"), "unsigned-")
				if token.MatchString(installed) {
					kernels = append(kernels, installed)
				}
			}
		}
		sort.Strings(kernels)
		if len(kernels) > 128 {
			kernels = kernels[:128]
		}
		set("kernel_installed", "dpkg-query", "complete", "observed", map[string]any{"versions": kernels})
		set("packages_installed", "dpkg-query", "complete", "observed", map[string]any{"manager": "apt", "count": count})
	} else {
		absent("kernel_installed", "dpkg-query")
		absent("packages_installed", "dpkg-query")
	}
	b, e = r.Run(ctx, "/usr/bin/apt", "-o", "Dir::Cache::pkgcache=", "-o", "Dir::Cache::srcpkgcache=", "-o", "Debug::NoLocking=1", "list", "--upgradable")
	candidates := []map[string]string{}
	state, reason := "complete", "cached_only"
	if e == nil {
		for _, l := range strings.Split(string(b), "\n") {
			f := strings.Fields(l)
			if len(f) < 6 || !strings.Contains(f[0], "/") {
				continue
			}
			name := strings.SplitN(f[0], "/", 2)[0]
			old := strings.TrimSuffix(f[len(f)-1], "]")
			if !token.MatchString(name) || !token.MatchString(f[1]) || !token.MatchString(old) {
				state, reason = "partial", "invalid_output"
				continue
			}
			if len(candidates) >= 512 {
				state, reason = "partial", "truncated"
				break
			}
			candidates = append(candidates, map[string]string{"name": name, "installed": old, "candidate": f[1]})
		}
		set("packages_candidates", "apt-cache", state, reason, map[string]any{"manager": "apt", "candidates": candidates, "repository_contacted": false})
	} else {
		absent("packages_candidates", "apt-cache")
	}
	unitStates := []map[string]string{}
	state, reason = "complete", "observed"
	for _, unit := range units {
		b, e = r.Run(ctx, "/usr/bin/systemctl", "show", unit, "--property=LoadState,ActiveState,SubState", "--no-pager")
		v := map[string]string{"name": unit}
		if e != nil {
			v["load"] = "unknown"
			v["active"] = "unknown"
			v["sub"] = "unknown"
			state, reason = "partial", "collection_failed"
		} else {
			for _, l := range strings.Split(string(b), "\n") {
				kv := strings.SplitN(l, "=", 2)
				if len(kv) != 2 || !regexp.MustCompile(`^[a-z-]{1,32}$`).MatchString(kv[1]) {
					continue
				}
				switch kv[0] {
				case "LoadState":
					v["load"] = kv[1]
				case "ActiveState":
					v["active"] = kv[1]
				case "SubState":
					v["sub"] = kv[1]
				}
			}
			for _, k := range []string{"load", "active", "sub"} {
				if v[k] == "" {
					v[k] = "unknown"
					state, reason = "partial", "invalid_output"
				}
			}
		}
		unitStates = append(unitStates, v)
	}
	set("services", "systemd", state, reason, map[string]any{"units": unitStates})
	b, e = r.Run(ctx, "/usr/sbin/sshd", "-T")
	policy := map[string]any{"ports": []int{}, "fingerprints": []string{}, "password_authentication": "unknown", "permit_root_login": "unknown"}
	state, reason = "partial", "ssh_global_policy"
	if e != nil {
		state, reason = "unavailable", "collection_failed"
	} else {
		ports := []int{}
		for _, l := range strings.Split(string(b), "\n") {
			f := strings.Fields(l)
			if len(f) != 2 {
				continue
			}
			switch f[0] {
			case "port":
				n, e := strconv.Atoi(f[1])
				if e == nil && n > 0 && n <= 65535 {
					ports = append(ports, n)
				}
			case "passwordauthentication":
				if f[1] == "yes" || f[1] == "no" {
					policy["password_authentication"] = f[1]
				}
			case "permitrootlogin":
				for _, allowed := range []string{"yes", "no", "prohibit-password", "without-password", "forced-commands-only"} {
					if f[1] == allowed {
						policy["permit_root_login"] = f[1]
					}
				}
			}
		}
		policy["ports"] = ports
	}
	fps := []string{}
	for _, key := range []string{"rsa", "ecdsa", "ed25519"} {
		b, e = r.Run(ctx, "/usr/bin/ssh-keygen", "-E", "sha256", "-lf", "/etc/ssh/ssh_host_"+key+"_key.pub")
		f := strings.Fields(string(b))
		if e == nil && len(f) > 1 && regexp.MustCompile(`^SHA256:[A-Za-z0-9+/]{43}$`).MatchString(f[1]) {
			fps = append(fps, f[1])
		}
	}
	policy["fingerprints"] = fps
	if state == "unavailable" {
		set("ssh", "sshd-public-keys", state, reason, nil)
	} else {
		set("ssh", "sshd-public-keys", state, reason, policy)
	}
	b, e = r.Read("/proc/self/mountinfo")
	mounts := []map[string]any{}
	state, reason = "complete", "observed"
	if e == nil {
		for _, l := range strings.Split(string(b), "\n") {
			parts := strings.SplitN(l, " - ", 2)
			if len(parts) != 2 {
				continue
			}
			a, z := strings.Fields(parts[0]), strings.Fields(parts[1])
			if len(a) < 6 || len(z) < 2 || !strings.HasPrefix(z[1], "/dev/") {
				continue
			}
			typ := z[0]
			if !contains([]string{"ext2", "ext3", "ext4", "xfs", "btrfs", "vfat", "f2fs", "zfs"}, typ) {
				continue
			}
			mount := a[4]
			if !regexp.MustCompile(`^/[A-Za-z0-9_./-]*$`).MatchString(mount) || len(mount) > 256 || !token.MatchString(strings.TrimPrefix(z[1], "/")) {
				state, reason = "partial", "invalid_output"
				continue
			}
			if len(mounts) >= 64 {
				state, reason = "partial", "truncated"
				break
			}
			nums, e := r.FS(mount)
			if e != nil {
				state, reason = "partial", "collection_failed"
				continue
			}
			m := map[string]any{"device": z[1], "mount": mount, "type": typ}
			for k, v := range nums {
				m[k] = v
			}
			mounts = append(mounts, m)
		}
		set("filesystems", "mountinfo-statfs", state, reason, map[string]any{"mounts": mounts})
	} else {
		absent("filesystems", "mountinfo-statfs")
	}
	for _, spec := range []struct {
		k, source, path string
		args            []string
	}{
		{"docker", "docker-version", "/usr/bin/docker", []string{"version", "--format", "{{.Server.Version}}"}},
		{"tor", "tor-version", "/usr/bin/docker", []string{"exec", "impreza_tor", "tor", "--version"}},
		{"proxy", "caddy-version", "/usr/bin/docker", []string{"exec", "impreza_caddy", "caddy", "version"}},
		{"shield", "caddy-modules", "/usr/bin/docker", []string{"exec", "impreza_caddy", "caddy", "list-modules", "--versions"}},
	} {
		b, e = r.Run(ctx, spec.path, spec.args...)
		text := string(b)
		if spec.k == "shield" {
			text = ""
			for _, l := range strings.Split(string(b), "\n") {
				if strings.HasPrefix(l, "http.handlers.waf ") {
					text = l
					break
				}
			}
		}
		m := version.FindStringSubmatch(text)
		if e == nil && len(m) > 1 {
			set(spec.k, spec.source, "complete", "observed", map[string]any{"version": m[1]})
		} else {
			absent(spec.k, spec.source)
		}
	}
	return report
}
func contains(a []string, s string) bool {
	for _, v := range a {
		if s == v {
			return true
		}
	}
	return false
}
