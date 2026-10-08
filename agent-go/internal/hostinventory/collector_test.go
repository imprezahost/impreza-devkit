package hostinventory

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

const secret = "HOST_INVENTORY_PRIVATE_CANARY_DO_NOT_COLLECT"

type fixtureReader struct {
	t     *testing.T
	fail  string
	reads []string
	calls []string
}

func (r *fixtureReader) Read(p string) ([]byte, error) {
	r.reads = append(r.reads, p)
	switch p {
	case "/etc/os-release":
		return []byte("ID=ubuntu\nVERSION_ID=24.04\nPRIVATE=" + secret + "\n"), nil
	case "/proc/sys/kernel/osrelease":
		return []byte("6.8.0-86-generic\n"), nil
	case "/proc/sys/kernel/random/boot_id":
		return []byte("d2c15f20-cbea-4d6a-9b11-84a62eb95471\n"), nil
	case "/proc/self/mountinfo":
		return []byte("21 1 8:1 / / rw - ext4 /dev/vda1 rw\n22 1 0:4 / /proc rw - proc proc rw\n"), nil
	default:
		r.t.Errorf("forbidden read: %s", p)
		return []byte(secret), nil
	}
}
func (r *fixtureReader) Run(_ context.Context, p string, args ...string) ([]byte, error) {
	call := p + " " + strings.Join(args, " ")
	r.calls = append(r.calls, call)
	if strings.Contains(call, r.fail) && r.fail != "" {
		return nil, errors.New(secret)
	}
	switch p {
	case "/usr/bin/dpkg-query":
		if strings.Join(args, " ") != "-W -f=${db:Status-Abbrev} ${binary:Package} ${Version}\n" {
			r.t.Errorf("unreviewed dpkg arguments %q", args)
		}
		return []byte("ii apt 2.7.14\nii linux-image-6.8.0-86-generic 6.8.0-86.86\n"), nil
	case "/usr/bin/apt":
		if strings.Join(args, " ") != "-o Dir::Cache::pkgcache= -o Dir::Cache::srcpkgcache= -o Debug::NoLocking=1 list --upgradable" {
			r.t.Errorf("package writing or network command %q", args)
		}
		return []byte("Listing...\napt/noble-updates 2.7.14build2 amd64 [upgradable from: 2.7.14]\n"), nil
	case "/usr/bin/systemctl":
		if len(args) != 4 || args[0] != "show" || !contains([]string{"impreza-agent.service", "docker.service", "tor.service", "impreza-agent-ingress.service"}, args[1]) || args[2] != "--property=LoadState,ActiveState,SubState" || args[3] != "--no-pager" {
			r.t.Errorf("arbitrary service arguments %q", args)
		}
		return []byte("LoadState=loaded\nActiveState=active\nSubState=running\nEnvironment=" + secret + "\n"), nil
	case "/usr/sbin/sshd":
		if len(args) != 1 || args[0] != "-T" {
			r.t.Errorf("SSH mutation %q", args)
		}
		return []byte("port 22\npasswordauthentication no\npermitrootlogin prohibit-password\nauthorizedkeysfile " + secret + "\n"), nil
	case "/usr/bin/ssh-keygen":
		if len(args) != 4 || args[0] != "-E" || args[1] != "sha256" || args[2] != "-lf" || !strings.HasSuffix(args[3], "_key.pub") {
			r.t.Errorf("private key read %q", args)
		}
		return []byte("256 SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA " + secret + " (ED25519)\n"), nil
	case "/usr/bin/docker":
		if strings.Join(args, " ") == "version --format {{.Server.Version}}" {
			return []byte("29.1.3"), nil
		}
		if strings.Join(args, " ") == "exec impreza_caddy caddy version" {
			return []byte("v2.11.4 " + secret), nil
		}
		if strings.Join(args, " ") == "exec impreza_tor tor --version" {
			return []byte("Tor version 0.4.9.12."), nil
		}
		if strings.Join(args, " ") == "exec impreza_caddy caddy list-modules --versions" {
			return []byte("http.handlers.waf v3.4.0\nsecret.foo " + secret), nil
		}
		r.t.Errorf("unreviewed Docker read %q", args)
	default:
		r.t.Errorf("unreviewed command %s", call)
	}
	return nil, errors.New(secret)
}
func (r *fixtureReader) FS(p string) (map[string]uint64, error) {
	if p != "/" {
		r.t.Errorf("private mount %s", p)
	}
	return map[string]uint64{"bytes_total": 40000, "bytes_available": 30000, "inodes_total": 1000, "inodes_available": 999}, nil
}
func TestAllowlistedFactsAndPrivacy(t *testing.T) {
	r := &fixtureReader{t: t}
	at := time.Date(2026, 10, 2, 18, 0, 0, 0, time.UTC)
	report := CollectWith(context.Background(), r, at, "amd64")
	b, e := json.Marshal(report)
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(string(b), secret) {
		t.Fatalf("private data in report %s", b)
	}
	if report.Facts["os"].State != "complete" || report.Facts["kernel_loaded"].State != "complete" || report.Facts["boot"].State != "complete" {
		t.Fatal("positive host facts overblocked")
	}
	if len(report.Facts) != 13 {
		t.Fatalf("facts %d", len(report.Facts))
	}
	if report.Facts["packages_candidates"].State != "complete" {
		t.Fatal("cached candidates missing")
	}
	if report.Facts["ssh"].State != "partial" || report.Facts["ssh"].Reason != "ssh_global_policy" {
		t.Fatal("SSH Match limits hidden")
	}
	if len(r.reads) != 4 {
		t.Fatal("private file read")
	}
	for _, f := range report.Facts {
		if f.ObservedAt != at || !f.ValidUntil.Equal(at.Add(6*time.Hour)) {
			t.Fatal("unbounded observation validity")
		}
	}
}
func TestPartialCommandFailureNeverLeaksError(t *testing.T) {
	r := &fixtureReader{t: t, fail: "docker.service"}
	report := CollectWith(context.Background(), r, time.Now().UTC(), "amd64")
	if report.Facts["services"].State != "partial" {
		t.Fatal("failed unit became compatible")
	}
	b, _ := json.Marshal(report)
	if strings.Contains(string(b), secret) {
		t.Fatal("command error secret escaped")
	}
}
func TestUnavailablePackageCommands(t *testing.T) {
	r := &fixtureReader{t: t, fail: "/usr/bin/dpkg-query"}
	report := CollectWith(context.Background(), r, time.Now().UTC(), "amd64")
	if report.Facts["packages_installed"].State != "unavailable" || report.Facts["kernel_installed"].Value != nil {
		t.Fatal("failed package query invented facts")
	}
}
func TestBoundedOutput(t *testing.T) {
	var b bounded
	if _, e := b.Write(make([]byte, Limit+1)); e == nil {
		t.Fatal("output was unbounded")
	}
}
