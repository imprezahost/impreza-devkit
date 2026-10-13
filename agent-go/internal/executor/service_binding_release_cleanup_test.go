package executor

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/imprezahost/impreza-devkit/agent-go/internal/dockertest"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

type releaseCleanupCase struct {
	d                         *Docker
	p                         sdkclient.DeployPayload
	old, current              string
	oldFile, compatibleFile   string
	oldBytes, compatibleBytes []byte
}

func releaseCleanupSeed(t *testing.T, engine, transition, image string) releaseCleanupCase {
	t.Helper()
	consumer, c, intent := rotationFixture()
	if transition == "abandon" {
		intent.Mode = "abandon"
		c.ServiceBindingRef = intent.Candidate
		_, _, c.Username = bindingGenerationNames(intent.Candidate)
	}
	if engine == "postgres-legacy" {
		c.Username = "imp_" + strings.TrimPrefix(c.BindingID, "bnd_")
	}
	oldSecret, nextSecret := strings.Repeat("a", 64), strings.Repeat("f", 64)
	url := func(login, password string) string {
		if engine == "mysql" {
			return "mysql://" + login + ":" + password + "@mariadb_" + c.ProviderDeploymentID + ":3306/" + c.Database
		}
		return "postgresql://" + login + ":" + password + "@pg_" + c.ProviderDeploymentID + ":5432/" + c.Database + "?sslmode=disable"
	}
	_, _, nextLogin := bindingGenerationNames(intent.Candidate)
	if transition == "abandon" {
		_, _, nextLogin = bindingGenerationNames(intent.Previous)
	}
	old, current := url(c.Username, oldSecret), url(nextLogin, nextSecret)
	p := rotationPayload(consumer, intent)
	if engine == "mysql" {
		p.Manifest.Runtime.ServiceBindingProtocol = sdkclient.MysqlServiceBindingGenerationProtocol
		p.Manifest.Runtime.ServiceBindingRotationProtocol = sdkclient.MysqlServiceBindingRotationProtocol
	}
	if transition == "removal" {
		current = ""
		protocol := sdkclient.ServiceBindingGenerationRetirementProtocol
		if engine == "mysql" {
			protocol = sdkclient.MysqlServiceBindingGenerationRetirementProtocol
		}
		if engine == "postgres-legacy" {
			protocol = sdkclient.ServiceBindingRetirementProtocol
		}
		p = sdkclient.DeployPayload{DeploymentID: consumer, Manifest: sdkclient.AppManifest{Runtime: sdkclient.ManifestRuntime{Type: "docker-compose", Startup: &sdkclient.ManifestStartup{RequireHealthy: true, TimeoutSeconds: 60}, ServiceBindingRetirementProtocol: protocol, ServiceBindingRetirements: []sdkclient.ServiceBindingRef{c.ServiceBindingRef}}}}
	}
	p.ServiceBindingRetirementAuthorizations = []sdkclient.ServiceBindingRetirement{{ServiceBindingRef: c.ServiceBindingRef, Username: c.Username, Database: c.Database, AdminUser: c.AdminUser}}
	p.Vars = map[string]any{"KEEP": "ordinary"}
	if current != "" {
		p.Vars["DATABASE_URL"] = current
	}
	d := &Docker{StateDir: t.TempDir(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	dir := d.appDir(consumer)
	if err := os.MkdirAll(filepath.Join(dir, "releases"), 0700); err != nil {
		t.Fatal(err)
	}
	save := func(id, value string) (string, []byte) {
		env := []byte("KEEP=ordinary\n")
		if value != "" {
			env = append(env, []byte("DATABASE_URL='"+value+"'\n")...)
		}
		r := runtimeRelease{Compose: releaseCleanupModel(consumer, image, value), Env: env, EnvExists: true, Metadata: sdkclient.DeploymentRelease{ID: id, ImageIDs: map[string]string{"app": image}}}
		raw, _ := json.Marshal(r)
		path := filepath.Join(dir, "releases", id+".json")
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		return path, raw
	}
	oldFile, oldBytes := save("rel_cleanup_retired", old)
	compatibleFile, compatibleBytes := save("rel_cleanup_compatible", current)
	os.WriteFile(filepath.Join(dir, "compose.yaml"), releaseCleanupModel(consumer, image, current), 0600)
	env, _ := renderEnv(p.Vars)
	os.WriteFile(filepath.Join(dir, ".env"), []byte(env), 0600)
	return releaseCleanupCase{d, p, old, current, oldFile, compatibleFile, oldBytes, compatibleBytes}
}
func releaseCleanupModel(id, image, value string) []byte {
	env := map[string]string{"KEEP": "ordinary"}
	if value != "" {
		env["DATABASE_URL"] = value
	}
	raw, _ := json.Marshal(map[string]any{"name": id, "services": map[string]any{"app": map[string]any{"image": image, "pull_policy": "never", "entrypoint": []string{"/bin/sh", "-c"}, "command": []string{"exec sleep 3600"}, "healthcheck": map[string]any{"test": []string{"CMD", "/bin/sh", "-c", "exit 0"}, "interval": "1s", "timeout": "1s", "retries": 2}, "environment": env}}})
	return raw
}
func releaseCleanupAssert(t *testing.T, f releaseCleanupCase) {
	t.Helper()
	raw, err := os.ReadFile(f.oldFile)
	var r runtimeRelease
	if err != nil || json.Unmarshal(raw, &r) != nil || strings.Contains(string(raw), strings.Repeat("a", 64)) || strings.Contains(string(r.Env), strings.Repeat("a", 64)) {
		t.Fatal("retired credential remains in retained snapshot")
	}
	saved, _ := os.ReadFile(f.compatibleFile)
	if string(saved) != string(f.compatibleBytes) {
		t.Fatal("compatible release was changed")
	}
	temps, _ := filepath.Glob(filepath.Join(filepath.Dir(f.oldFile), ".impreza-tmp-*"))
	if len(temps) != 0 {
		t.Fatal("snapshot temporary survived")
	}
	payload, _ := json.Marshal(sdkclient.RollbackPayload{DeploymentID: f.p.DeploymentID, TargetVersion: "rel_cleanup_retired"})
	out := f.d.Execute(context.Background(), &sdkclient.PollCommand{ID: "cmd_release_cleanup", Kind: sdkclient.CommandRollback, Payload: payload})
	if out.Status != "failed" || out.Error != "release changes a managed database connection; review the connection before redeploying; containers were not changed" {
		t.Fatal("retired rollback refusal changed")
	}
}
func TestReleaseCleanupRetirementEntry(t *testing.T) {
	for _, engine := range []string{"postgres", "mysql", "postgres-legacy"} {
		for _, transition := range []string{"rotation", "removal", "abandon"} {
			if engine == "postgres-legacy" && transition != "removal" {
				continue
			}
			t.Run(engine+"/"+transition, func(t *testing.T) {
				f := releaseCleanupSeed(t, engine, transition, "sha256:"+strings.Repeat("1", 64))
				c := dockertest.ComposeContainer(f.p.DeploymentID, "app", "running", 0)
				c.Image = "sha256:" + strings.Repeat("1", 64)
				c.State.Health = &dockertest.Health{Status: "healthy"}
				dockertest.Install(t, dockertest.State{Containers: []dockertest.Container{c}, ComposeConfig: map[string]string{f.d.appDir(f.p.DeploymentID): string(releaseCleanupModel(f.p.DeploymentID, c.Image, f.current))}})
				// These are the actual completion methods used by finishReplacement; the
				// provider is intentionally absent, so SQL retirement remains pending.
				if transition != "removal" {
					f.d.finishServiceBindingRotation(context.Background(), f.p, true)
				} else {
					f.d.finishServiceBindingRetirements(context.Background(), f.p)
				}
				// SQL retirement is pending: replaying this completion must preserve
				// the sanitized archive and every compatible rollback target.
				sanitized, err := os.ReadFile(f.oldFile)
				if err != nil {
					t.Fatal("retry measurement failed")
				}
				if transition != "removal" {
					f.d.finishServiceBindingRotation(context.Background(), f.p, true)
				} else {
					f.d.finishServiceBindingRetirements(context.Background(), f.p)
				}
				again, err := os.ReadFile(f.oldFile)
				if err != nil || string(again) != string(sanitized) {
					t.Fatal("retirement retry altered sanitized archive")
				}
				releaseCleanupAssert(t, f)
				payload, _ := json.Marshal(sdkclient.RollbackPayload{DeploymentID: f.p.DeploymentID, TargetVersion: "rel_cleanup_compatible"})
				out := f.d.Execute(context.Background(), &sdkclient.PollCommand{ID: "cmd_release_cleanup_ok", Kind: sdkclient.CommandRollback, Payload: payload})
				if out.Status != "success" {
					t.Fatalf("compatible rollback failed: %s", out.Error)
				}
			})
		}
	}
}
func TestReleaseCleanupUnhealthyAndInvalidIntentPreserve(t *testing.T) {
	for _, mode := range []string{"unhealthy", "invalid"} {
		t.Run(mode, func(t *testing.T) {
			f := releaseCleanupSeed(t, "postgres", "rotation", "sha256:"+strings.Repeat("1", 64))
			if mode == "invalid" {
				f.p.ServiceBindingRetirementAuthorizations[0].Username = "postgres"
			}
			f.d.finishServiceBindingRotation(context.Background(), f.p, mode != "unhealthy")
			raw, _ := os.ReadFile(f.oldFile)
			if string(raw) != string(f.oldBytes) {
				t.Fatal("unverified retirement altered archive")
			}
		})
	}
}
func TestReleaseCleanupAtomicWriteFailure(t *testing.T) {
	if os.Getenv("IMPREZA_RELEASE_CLEANUP_FAIL_RENAME") != "1" {
		t.Skip("rename-failure overlay required")
	}
	f := releaseCleanupSeed(t, "postgres", "rotation", "sha256:"+strings.Repeat("1", 64))
	double := dockertest.Install(t, dockertest.State{ComposeConfig: map[string]string{f.d.appDir(f.p.DeploymentID): string(releaseCleanupModel(f.p.DeploymentID, "sha256:"+strings.Repeat("1", 64), f.current))}})
	out := f.d.finishServiceBindingRotation(context.Background(), f.p, true)
	for _, call := range double.State().Calls {
		if len(call.Args) > 1 && call.Args[0] == "inspect" && call.Args[1] == "pg_"+f.p.ServiceBindingRetirementAuthorizations[0].ProviderDeploymentID {
			t.Fatal("failed cleanup started SQL retirement")
		}
	}
	raw, _ := os.ReadFile(f.oldFile)
	if out.Status != "cleanup_pending" || string(raw) != string(f.oldBytes) {
		t.Fatal("failed atomic write corrupted archive or claimed completion")
	}
	compatible, _ := os.ReadFile(f.compatibleFile)
	if string(compatible) != string(f.compatibleBytes) {
		t.Fatal("failed cleanup altered compatible release")
	}
	temps, _ := filepath.Glob(filepath.Join(filepath.Dir(f.oldFile), ".impreza-tmp-*"))
	if len(temps) != 0 {
		t.Fatal("failed rename left snapshot temporary")
	}
}

func TestReleaseCleanupUnsafeArchivePreserve(t *testing.T) {
	for _, mode := range []string{"current-still-retired", "snapshot-link", "archive-link", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			f := releaseCleanupSeed(t, "postgres", "rotation", "sha256:"+strings.Repeat("1", 64))
			dir := f.d.appDir(f.p.DeploymentID)
			current := f.current
			switch mode {
			case "current-still-retired":
				current = f.old
			case "malformed":
				f.oldBytes = []byte("invalid-json")
				os.WriteFile(f.oldFile, f.oldBytes, 0600)
			case "snapshot-link":
				outside := filepath.Join(f.d.StateDir, "outside.json")
				os.WriteFile(outside, f.oldBytes, 0600)
				os.Remove(f.oldFile)
				if os.Symlink(outside, f.oldFile) != nil {
					t.Skip("symlinks unavailable")
				}
			case "archive-link":
				archive := filepath.Dir(f.oldFile)
				outside := filepath.Join(f.d.StateDir, "outside")
				if os.Rename(archive, outside) != nil {
					t.Fatal("setup rename failed")
				}
				if os.Symlink(outside, archive) != nil {
					t.Skip("symlinks unavailable")
				}
			}
			dockertest.Install(t, dockertest.State{ComposeConfig: map[string]string{dir: string(releaseCleanupModel(f.p.DeploymentID, "sha256:"+strings.Repeat("1", 64), current))}})
			out := f.d.finishServiceBindingRotation(context.Background(), f.p, true)
			saved, _ := os.ReadFile(f.oldFile)
			if out.Status != "cleanup_pending" || string(saved) != string(f.oldBytes) {
				t.Fatal("unsafe archive was followed, overwritten or certified")
			}
		})
	}
}
func TestReleaseCleanupDeploymentIsolation(t *testing.T) {
	f := releaseCleanupSeed(t, "postgres", "rotation", "sha256:"+strings.Repeat("1", 64))
	other := filepath.Join(f.d.StateDir, "apps", "dep_other", "releases")
	if err := os.MkdirAll(other, 0700); err != nil {
		t.Fatal(err)
	}
	untouched := filepath.Join(other, "rel_other.json")
	if err := os.WriteFile(untouched, f.oldBytes, 0600); err != nil {
		t.Fatal(err)
	}
	unrelated := filepath.Join(filepath.Dir(f.oldFile), "rel_unrelated.json")
	var release runtimeRelease
	json.Unmarshal(f.oldBytes, &release)
	release.Metadata.ID = "rel_unrelated"
	release.Compose = []byte(strings.ReplaceAll(string(release.Compose), f.p.ServiceBindingRetirementAuthorizations[0].Username, "imp_other"))
	release.Env = []byte(strings.ReplaceAll(string(release.Env), f.p.ServiceBindingRetirementAuthorizations[0].Username, "imp_other"))
	before, _ := json.Marshal(release)
	os.WriteFile(unrelated, before, 0600)
	dockertest.Install(t, dockertest.State{ComposeConfig: map[string]string{f.d.appDir(f.p.DeploymentID): string(releaseCleanupModel(f.p.DeploymentID, "sha256:"+strings.Repeat("1", 64), f.current))}})
	f.d.finishServiceBindingRotation(context.Background(), f.p, true)
	for path, want := range map[string][]byte{untouched: f.oldBytes, unrelated: before, f.compatibleFile: f.compatibleBytes} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != string(want) {
			t.Fatal("cleanup crossed deployment or binding boundary")
		}
	}
	var cleaned runtimeRelease
	raw, _ := os.ReadFile(f.oldFile)
	json.Unmarshal(raw, &cleaned)
	if strings.Contains(string(cleaned.Env), strings.Repeat("a", 64)) {
		t.Fatal("matching retirement was not sanitized")
	}
}
