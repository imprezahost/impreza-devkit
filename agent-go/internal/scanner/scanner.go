// Package scanner produces bounded, advisory-only findings from a source tree.
// It never executes project code or sends file contents to a remote service.
package scanner

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

type Finding struct {
	Kind     string `json:"kind"`
	Severity string `json:"severity"`
	File     string `json:"file"`
	Name     string `json:"name"`
	Version  string `json:"version,omitempty"`
	ID       string `json:"id,omitempty"`
	Fix      string `json:"fix"`
}
type Report struct {
	Findings         []Finding `json:"findings,omitempty"`
	SkippedFiles     []string  `json:"skipped_files,omitempty"`
	Truncated        bool      `json:"truncated,omitempty"`
	AdvisoryAge      string    `json:"advisory_age,omitempty"`
	DependencyStatus string    `json:"dependency_status"`
	Scanned          bool      `json:"scanned"`
}

const (
	MaxFileBytes   = 2 << 20
	MaxTotalByte   = 64 << 20
	MaxBaseBytes   = 16 << 20
	MaxEntries     = 50000
	MaxTreeEntries = 20000
	MaxFindings    = 100
	MaxSkipped     = 50
	TimeBudget     = 60 * time.Second
)

var depNameRe = regexp.MustCompile(`^[a-zA-Z0-9@/._-]{1,120}$`)
var versionRe = regexp.MustCompile(`^[a-zA-Z0-9.+_-]{1,100}$`)
var idRe = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,80}$`)

func safeDepName(s string) string {
	if len(s) > 120 {
		s = s[:120]
	}
	if !depNameRe.MatchString(s) {
		return "[filtered]"
	}
	return s
}
func safePath(s string) string {
	s = filepath.ToSlash(s)
	if len(s) > 240 || strings.HasPrefix(s, "/") || strings.Contains(s, "..") {
		return "[filtered-path]"
	}
	for _, r := range s {
		if r < 32 || r > 126 || strings.ContainsRune("<>\"`", r) {
			return "[filtered-path]"
		}
	}
	return s
}

type AdvisoryEntry struct {
	Ecosystem string     `json:"ecosystem"`
	Name      string     `json:"name"`
	Version   string     `json:"version,omitempty"`
	Versions  []string   `json:"versions,omitempty"`
	Ranges    []Interval `json:"ranges,omitempty"`
	Aliases   []string   `json:"aliases,omitempty"`
	Source    string     `json:"source,omitempty"`
	ID        string     `json:"id"`
	Sev       string     `json:"severity"`
	Summary   string     `json:"summary,omitempty"`
}
type AdvisoryBase struct {
	Schema      int             `json:"schema,omitempty"`
	Revision    uint64          `json:"revision,omitempty"`
	ExpiresAt   string          `json:"expires_at,omitempty"`
	Incomplete  bool            `json:"incomplete,omitempty"`
	Attribution []string        `json:"attribution,omitempty"`
	GeneratedAt string          `json:"generated_at"`
	Entries     []AdvisoryEntry `json:"entries"`
}
type scan struct {
	ctx     context.Context
	root    *os.Root
	report  *Report
	bytes   int64
	entries int
	index   map[string][]AdvisoryEntry
}

func (s *scan) add(f Finding) {
	if len(s.report.Findings) >= MaxFindings {
		s.report.Truncated = true
		return
	}
	f.File = safePath(f.File)
	s.report.Findings = append(s.report.Findings, f)
}
func (s *scan) skip(name string) {
	if len(s.report.SkippedFiles) >= MaxSkipped {
		s.report.Truncated = true
		return
	}
	s.report.SkippedFiles = append(s.report.SkippedFiles, safePath(name))
}
func (s *scan) expired() bool {
	if s.ctx.Err() != nil {
		s.report.Truncated = true
		return true
	}
	return false
}
func ScanDir(root string, base *AdvisoryBase) (*Report, error) {
	return ScanDirContext(context.Background(), root, base)
}
func ScanDirContext(parent context.Context, root string, base *AdvisoryBase) (*Report, error) {
	ctx, cancel := context.WithTimeout(parent, TimeBudget)
	defer cancel()
	rp := &Report{DependencyStatus: "unavailable"}
	r, err := os.OpenRoot(root)
	if err != nil {
		return rp, errors.New("source scan root unavailable")
	}
	defer r.Close()
	s := scan{ctx: ctx, root: r, report: rp, index: map[string][]AdvisoryEntry{}}
	if validBase(base) {
		rp.Truncated = base.Incomplete
		rp.AdvisoryAge = base.GeneratedAt
		rp.DependencyStatus = "available"
		stamp, _ := time.Parse(time.RFC3339, base.GeneratedAt)
		if time.Since(stamp) > 7*24*time.Hour {
			rp.DependencyStatus = "stale"
		}
		if expires, err := time.Parse(time.RFC3339, base.ExpiresAt); err == nil && !expires.After(time.Now()) {
			rp.DependencyStatus = "stale"
		}
		for _, e := range base.Entries {
			key := e.Ecosystem + "\x00" + e.Name
			s.index[key] = append(s.index[key], e)
		}
	}
	rp.Scanned = true
	s.walk(".")
	return rp, nil
}

// Directory batches bound allocation before inspecting an attacker-controlled tree.
// os.Root confines every open, including symlink resolution, to the source tree.
func (s *scan) walk(dir string) {
	if s.expired() {
		return
	}
	f, err := openNoFollow(s.root, dir)
	if err != nil {
		s.skip(dir)
		return
	}
	defer f.Close()
	for {
		batch, err := f.ReadDir(128)
		if err != nil && err != io.EOF {
			s.skip(dir)
			return
		}
		for _, entry := range batch {
			s.entries++
			if s.entries > MaxTreeEntries || s.expired() {
				s.report.Truncated = true
				return
			}
			name := filepath.Join(dir, entry.Name())
			if entry.Type()&os.ModeSymlink != 0 {
				s.skip(name)
				continue
			}
			if entry.IsDir() {
				if entry.Name() == ".git" || entry.Name() == "node_modules" || entry.Name() == "vendor" {
					continue
				}
				s.walk(name)
				if s.report.Truncated && (s.entries > MaxTreeEntries || s.bytes > MaxTotalByte || s.expired()) {
					return
				}
				continue
			}
			info, e := entry.Info()
			if e != nil || !info.Mode().IsRegular() {
				s.skip(name)
				continue
			}
			if info.Size() > MaxFileBytes {
				s.skip(name)
				continue
			}
			file, e := openNoFollow(s.root, name)
			if e != nil {
				s.skip(name)
				continue
			}
			stat, e := file.Stat()
			if e != nil || !stat.Mode().IsRegular() {
				file.Close()
				s.skip(name)
				continue
			}
			remaining := int64(MaxTotalByte) - s.bytes
			if remaining <= 0 {
				file.Close()
				s.report.Truncated = true
				return
			}
			limit := int64(MaxFileBytes)
			if remaining < limit {
				limit = remaining
			}
			raw, e := io.ReadAll(io.LimitReader(file, limit+1))
			file.Close()
			s.bytes += int64(len(raw))
			if e != nil || int64(len(raw)) > limit {
				s.skip(name)
				if s.bytes > MaxTotalByte {
					s.report.Truncated = true
					return
				}
				continue
			}
			if s.expired() {
				return
			}
			s.secrets(name, raw)
			if len(s.index) > 0 {
				s.dependencies(name, raw)
			}
		}
		if err == io.EOF {
			return
		}
	}
}
func validBase(b *AdvisoryBase) bool {
	if b == nil || b.Entries == nil || len(b.Entries) > MaxEntries {
		return false
	}
	ts, err := time.Parse(time.RFC3339, b.GeneratedAt)
	if err != nil || ts.After(time.Now().Add(5*time.Minute)) {
		return false
	}
	for _, e := range b.Entries {
		if e.Ecosystem != "npm" && e.Ecosystem != "Packagist" && e.Ecosystem != "Go" {
			return false
		}
		if !depNameRe.MatchString(e.Name) || !idRe.MatchString(e.ID) || len(e.Summary) > 2000 ||
			(e.Version == "" && len(e.Versions) == 0 && len(e.Ranges) == 0) || len(e.Versions) > 5000 || len(e.Ranges) > 128 || len(e.Aliases) > 32 {
			return false
		}
		if e.Version != "" && !versionRe.MatchString(e.Version) {
			return false
		}
		for _, v := range e.Versions {
			if !versionRe.MatchString(v) {
				return false
			}
		}
		for _, a := range e.Aliases {
			if !idRe.MatchString(a) {
				return false
			}
		}
		for _, r := range e.Ranges {
			if !validInterval(e.Ecosystem, r) {
				return false
			}
		}
		if e.Sev != "low" && e.Sev != "moderate" && e.Sev != "high" && e.Sev != "critical" && e.Sev != "unknown" {
			return false
		}
	}
	return true
}
func readBaseFile(stateDir, name string, limit int64) ([]byte, error) {
	r, err := os.OpenRoot(stateDir)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	info, err := r.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errors.New("invalid advisory file")
	}
	f, err := openNoFollow(r, name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil || !stat.Mode().IsRegular() {
		return nil, errors.New("invalid advisory file")
	}
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(raw)) > limit {
		return nil, errors.New("advisory read failed")
	}
	return raw, nil
}
func (s *scan) match(file, ecosystem, name, version string) {
	if !depNameRe.MatchString(name) || !versionRe.MatchString(version) {
		return
	}
	seen := map[string]bool{}
	for _, e := range s.index[ecosystem+"\x00"+name] {
		if s.expired() {
			return
		}
		matches, understood := affected(e, version)
		if !understood {
			s.report.Truncated = true
		}
		if !matches {
			continue
		}
		duplicate := seen[e.ID]
		for _, a := range e.Aliases {
			duplicate = duplicate || seen[a]
		}
		seen[e.ID] = true
		for _, a := range e.Aliases {
			seen[a] = true
		}
		if duplicate {
			continue
		}
		s.add(Finding{Kind: "vulnerability", Severity: e.Sev, File: file, Name: name, Version: version, ID: e.ID,
			Fix: "Review the advisory and choose a fixed version compatible with your project."})
	}
}
func (s *scan) dependencies(file string, raw []byte) {
	switch filepath.Base(file) {
	case "package-lock.json":
		var lock struct {
			Packages map[string]struct {
				Version string `json:"version"`
			} `json:"packages"`
			Dependencies map[string]struct {
				Version string `json:"version"`
			} `json:"dependencies"`
		}
		if json.Unmarshal(raw, &lock) != nil {
			s.skip(file)
			return
		}
		keys := make([]string, 0, len(lock.Packages))
		for k := range lock.Packages {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, p := range keys {
			if s.expired() {
				return
			}
			i := strings.LastIndex(p, "node_modules/")
			if i < 0 {
				continue
			}
			s.match(file, "npm", p[i+len("node_modules/"):], lock.Packages[p].Version)
		}
		if len(lock.Packages) == 0 {
			for name, p := range lock.Dependencies {
				if s.expired() {
					return
				}
				s.match(file, "npm", name, p.Version)
			}
		}
	case "composer.lock":
		type dep struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		}
		var lock struct {
			Packages []dep `json:"packages"`
			Dev      []dep `json:"packages-dev"`
		}
		if json.Unmarshal(raw, &lock) != nil {
			s.skip(file)
			return
		}
		for _, p := range append(lock.Packages, lock.Dev...) {
			if s.expired() {
				return
			}
			s.match(file, "Packagist", p.Name, p.Version)
		}
	case "go.sum":
		seen := map[string]bool{}
		for _, line := range strings.Split(string(raw), "\n") {
			if s.expired() {
				return
			}
			fields := strings.Fields(line)
			if len(fields) != 3 {
				continue
			}
			version := strings.TrimSuffix(fields[1], "/go.mod")
			key := fields[0] + "\x00" + version
			if seen[key] {
				continue
			}
			seen[key] = true
			s.match(file, "Go", fields[0], version)
		}
	}
}

type secretPattern struct {
	kind string
	re   *regexp.Regexp
	fix  string
}

var secretPatterns = []secretPattern{
	{"private_key", regexp.MustCompile(`-----BEGIN (?:RSA |EC |DSA |OPENSSH |ENCRYPTED )?PRIVATE KEY-----`), "Remove the private key from source and rotate it."},
	{"aws_access_key", regexp.MustCompile(`AKIA[0-9A-Z]{16}`), "Remove and rotate the AWS access key."},
	{"gcp_service_account", regexp.MustCompile(`"type"\s*:\s*"service_account"`), "Remove and rotate the service account key."},
	{"db_url_with_password", regexp.MustCompile(`(?:postgres(?:ql)?|mysql|mongodb(?:\+srv)?|redis)://[^\s:@]+:[^\s@]+@[^\s]+`), "Remove embedded credentials and use secret references."},
}

func (s *scan) secrets(file string, raw []byte) {
	name := filepath.Base(file)
	if name == ".env" || strings.HasPrefix(name, ".env.") {
		s.add(Finding{Kind: "secret", Severity: "high", File: file, Name: ".env_file", Fix: "Review this environment file for credentials; exclude secrets from source and rotate exposed values."})
	}
	for _, p := range secretPatterns {
		if s.expired() {
			return
		}
		if p.re.Match(raw) {
			s.add(Finding{Kind: "secret", Severity: "high", File: file, Name: p.kind, Fix: p.fix})
		}
	}
}
