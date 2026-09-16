package executor

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

func TestDockerPrivatePreparationWorker(t *testing.T) {
	if os.Getenv("IMPREZA_DOCKER_TEST") != "1" || runtime.GOOS != "linux" {
		t.Skip("requires disposable Linux Docker host")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	d := &Docker{StateDir: t.TempDir(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := os.Mkdir(filepath.Join(d.StateDir, "operations"), 0700); err != nil {
		t.Fatal(err)
	}
	app := d.appDir("dpl_private_worker")
	if err := os.MkdirAll(filepath.Join(app, "build-ctx"), 0700); err != nil {
		t.Fatal(err)
	}
	image := "impreza-private-worker-fixture:latest"
	t.Cleanup(func() { _, _ = d.dockerCmd(context.Background(), "image", "rm", "-f", image).CombinedOutput() })
	compose := "name: dpl_private_worker\nservices:\n  app:\n    image: " + image + "\n    build:\n      context: ./build-ctx\n      secrets: [npmrc]\nsecrets:\n  npmrc:\n    file: ./.build-secrets/npmrc\n"
	if err := os.WriteFile(filepath.Join(app, "compose.yaml"), []byte(compose), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app, "build-ctx", "Dockerfile"), []byte("FROM busybox:1.37.0\nRUN --mount=type=secret,id=npmrc,required=true cat /run/secrets/npmrc\nRUN test ! -e /run/secrets/npmrc\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeBuildSecrets(app, []string{"npmrc"}, map[string]string{"npmrc": "worker-output-must-never-contain-this-value"}); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(sdkclient.DeployPayload{DeploymentID: "dpl_private_worker", Manifest: sdkclient.AppManifest{Runtime: sdkclient.ManifestRuntime{Build: &sdkclient.BuildContext{SecretNames: []string{"npmrc"}, SecretProtocol: buildSecretsProtocol}}}})
	w, err := d.createPreparationWork(&sdkclient.PollCommand{ID: "cmd_private_worker", Payload: payload}, "dpl_private_worker", "build")
	if err != nil {
		t.Fatal(err)
	}
	if err = RunPreparationWorker(d.StateDir, w.ID); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(d.StateDir, "operations", "preparation-"+w.ID, "result.json"))
	if err != nil {
		t.Fatal(err)
	}
	var result preparationWorkResult
	if err = json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if !result.Completed || !result.Success || strings.Contains(string(raw), "worker-output-must-never") || !strings.Contains(result.Output, "withheld") {
		t.Fatalf("private worker did not produce a successful redacted receipt: %+v", result)
	}
	if _, err = os.Stat(filepath.Join(app, ".build-secrets")); !os.IsNotExist(err) {
		t.Fatal("private worker retained credentials")
	}
	if out, err := d.dockerCmd(ctx, "run", "--rm", image, "sh", "-c", "test ! -e /run/secrets/npmrc").CombinedOutput(); err != nil {
		t.Fatalf("credential leaked into runtime image: %v %s", err, out)
	}
}
