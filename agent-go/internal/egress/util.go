package egress

// Shared plumbing for the v4 and v6 applies: resolver parsing for either
// family, the digest helper both fingerprints use, the clock used by both
// status writers, and the v6 status section of egress.json.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ReadResolversAny parses resolv.conf and returns every nameserver address
// (v4 and v6), deduplicated, in file order. The validation discipline of
// ReadResolvers (regular file, symlink resolves, size cap) applies.
func ReadResolversAny(path string) ([]netip.Addr, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, errors.New("resolver configuration unreadable")
	}
	if info.Mode()&os.ModeSymlink != 0 {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return nil, errors.New("resolver configuration symlink cannot be resolved")
		}
		target, err := os.Lstat(resolved)
		if err != nil || !target.Mode().IsRegular() {
			return nil, errors.New("resolver configuration does not resolve to a regular file")
		}
		path = resolved
		info = target
	}
	if !info.Mode().IsRegular() || info.Size() > resolvConfLimit {
		return nil, errors.New("resolver configuration is not a regular file")
	}
	raw, err := readBoundedRegularFile(path, resolvConfLimit)
	if err != nil || len(raw) > resolvConfLimit {
		return nil, errors.New("resolver configuration unreadable")
	}
	var resolvers []netip.Addr
	seen := map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		if i := strings.IndexAny(line, "#;"); i >= 0 {
			line = line[:i]
		}
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] != "nameserver" || len(resolvers) >= 8 {
			continue
		}
		addr, err := netip.ParseAddr(fields[1])
		if err != nil || addr.Zone() != "" || addr.Is4In6() || seen[addr.String()] {
			continue
		}
		seen[addr.String()] = true
		resolvers = append(resolvers, addr)
	}
	if len(resolvers) == 0 {
		return nil, errors.New("no resolver configured")
	}
	return resolvers, nil
}

// ReadResolvers is ReadResolversAny filtered to IPv4 (the original v4
// behavior, preserved).
func ReadResolvers(path string) ([]string, error) {
	base, err := ReadResolversAny(path)
	if err != nil {
		return nil, err
	}
	var out []string
	seen := map[string]bool{}
	for _, a := range base {
		if !a.Is4() || seen[a.String()] {
			continue
		}
		seen[a.String()] = true
		out = append(out, a.String())
	}
	if len(out) == 0 {
		return nil, errors.New("no IPv4 resolver configured")
	}
	return out, nil
}

func sha256Sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

// status6Path is the v6 section of the same egress.json document.
func status6Path(stateDir string) (string, error) {
	path, err := statusPath(stateDir)
	if err != nil {
		return "", err
	}
	return path, nil
}

type statusFile struct {
	V4     Status  `json:"v4"`
	V6     *Status `json:"v6,omitempty"`
	V4Host *Status `json:"v4_host,omitempty"`
	V6Host *Status `json:"v6_host,omitempty"`
}

func readStatus6(stateDir string) Status {
	var s Status
	path, err := status6Path(stateDir)
	if err != nil {
		return s
	}
	raw, err := readBoundedRegularFile(path, 8192)
	if err != nil {
		return s
	}
	var f statusFile
	if json.Unmarshal(raw, &f) != nil || f.V6 == nil {
		return Status{}
	}
	return *f.V6
}

func writeStatus6(stateDir string, status Status) error {
	return mutateStatus(stateDir, func(f *statusFile) { f.V6 = &status })
}

func readHostStatus4(stateDir string) Status {
	return readSection(stateDir, func(f *statusFile) *Status { return f.V4Host })
}

func writeHostStatus4(stateDir string, status Status) error {
	return mutateStatus(stateDir, func(f *statusFile) { f.V4Host = &status })
}

func readHostStatus6(stateDir string) Status {
	return readSection(stateDir, func(f *statusFile) *Status { return f.V6Host })
}

func writeHostStatus6(stateDir string, status Status) error {
	return mutateStatus(stateDir, func(f *statusFile) { f.V6Host = &status })
}

func readSection(stateDir string, get func(*statusFile) *Status) Status {
	path, err := status6Path(stateDir)
	if err != nil {
		return Status{}
	}
	raw, err := readBoundedRegularFile(path, 8192)
	if err != nil {
		return Status{}
	}
	var f statusFile
	if json.Unmarshal(raw, &f) != nil {
		return Status{}
	}
	if s := get(&f); s != nil {
		return *s
	}
	return Status{}
}

// mutateStatus rewrites egress.json atomically, preserving every section it
// does not touch.
func mutateStatus(stateDir string, mutate func(*statusFile)) error {
	path, err := status6Path(stateDir)
	if err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return errors.New("egress status path is not a regular file")
	}
	var f statusFile
	if raw, err := readBoundedRegularFile(path, 8192); err == nil {
		_ = json.Unmarshal(raw, &f)
	}
	mutate(&f)
	raw, err := json.Marshal(f)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(stateDir, ".egress-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Bound private state reads and reject symlinks/special files before parsing.
func readBoundedRegularFile(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errors.New("invalid regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(raw)) > limit {
		return nil, errors.New("invalid regular file")
	}
	return raw, nil
}
