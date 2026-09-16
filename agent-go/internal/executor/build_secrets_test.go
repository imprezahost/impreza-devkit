package executor

import (
	"encoding/json"
	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestBuildSecretNames(t *testing.T) {
	for _, names := range [][]string{nil, {"../key"}, {"key", "key"}, {"key-name"}, {"_key"}} {
		if validateBuildSecretNames(names) == nil {
			t.Fatalf("accepted invalid names: %v", names)
		}
	}
	if err := validateBuildSecretNames([]string{"npmrc", "BuildToken1"}); err != nil {
		t.Fatal(err)
	}
}

func TestBuildSecretFilesAreBoundAndCleaned(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Unix private modes")
	}
	app := t.TempDir()
	values := map[string]string{"npmrc": "fixture-private-token", "BuildToken1": "second-token"}
	if err := writeBuildSecrets(app, []string{"BuildToken1", "npmrc"}, values); err != nil {
		t.Fatal(err)
	}
	for name, value := range values {
		p := filepath.Join(app, ".build-secrets", name)
		info, _ := os.Stat(p)
		data, _ := os.ReadFile(p)
		if info.Mode().Perm() != 0600 || string(data) != value {
			t.Fatal("private file mismatch")
		}
	}
	if err := clearBuildSecrets(app); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(app, ".build-secrets")); !os.IsNotExist(err) {
		t.Fatal("credential directory retained")
	}
	if err := writeBuildSecrets(app, []string{"npmrc"}, values); err == nil {
		t.Fatal("accepted unexpected credential")
	}
	outside := t.TempDir()
	sentinel := filepath.Join(outside, "npmrc")
	_ = os.WriteFile(sentinel, []byte("outside"), 0600)
	if err := os.Symlink(outside, filepath.Join(app, ".build-secrets")); err != nil {
		t.Fatal(err)
	}
	if clearBuildSecrets(app) == nil {
		t.Fatal("followed secret directory symlink")
	}
	if data, _ := os.ReadFile(sentinel); string(data) != "outside" {
		t.Fatal("outside file changed")
	}
}

func TestPrivatePreparationWorkerWithholdsOutputAndDeletesSecrets(t *testing.T) {
	d, r, _ := workFixture(t, "build", `cat .build-secrets/npmrc; exit 42`)
	// Replace the fixture's unused first worker with a secret-aware request.
	payload := sdkclient.DeployPayload{DeploymentID: r.DeploymentID, Manifest: sdkclient.AppManifest{Runtime: sdkclient.ManifestRuntime{Build: &sdkclient.BuildContext{SecretNames: []string{"npmrc"}, SecretProtocol: buildSecretsProtocol}}}}
	raw, _ := json.Marshal(payload)
	w, err := d.createPreparationWork(&sdkclient.PollCommand{ID: "cmd_test", Payload: raw}, r.DeploymentID, "build")
	if err != nil {
		t.Fatal(err)
	}
	if err = writeBuildSecrets(d.appDir(r.DeploymentID), []string{"npmrc"}, map[string]string{"npmrc": "must-not-leak-in-worker-receipt"}); err != nil {
		t.Fatal(err)
	}
	if err = RunPreparationWorker(d.StateDir, w.ID); err != nil {
		t.Fatal(err)
	}
	receipt, err := os.ReadFile(filepath.Join(d.StateDir, "operations", "preparation-"+w.ID, "result.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(receipt), "must-not-leak") || !strings.Contains(string(receipt), "withheld") {
		t.Fatal("private output was not withheld")
	}
	if _, err = os.Stat(filepath.Join(d.appDir(r.DeploymentID), ".build-secrets")); !os.IsNotExist(err) {
		t.Fatal("worker retained build credentials")
	}
}
