package executor

// When the supervised pull worker never writes a receipt, the
// deploy may only report the defined, retryable failure AFTER the worker
// unit is confirmed stopped — otherwise a wedged worker (e.g. a compose
// plugin holding the pipes) races the report and may still be pulling.

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

// workFixtureSystemd is workFixture with a stateful fake systemctl: every
// invocation is logged, and "show" reports active until a "stop" line for
// the unit exists in the log.
func workFixtureSystemd(t *testing.T, step string) (*Docker, *PreparationRecovery, string) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("Linux private filesystem and command execution")
	}
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	systemctl := "#!/bin/sh\n" +
		"log=\"" + bin + "/systemctl.log\"\n" +
		"echo \"$*\" >> \"$log\"\n" +
		"if [ \"$1\" = \"show\" ]; then\n" +
		"  if grep -q '^stop ' \"$log\" 2>/dev/null; then echo inactive; else echo active; fi\n" +
		"fi\n"
	for name, body := range map[string]string{"docker": "exit 0", "systemd-run": "exit 0", "systemctl": systemctl} {
		script := []byte(body)
		if !strings.HasPrefix(body, "#!") {
			script = []byte("#!/bin/sh\n" + body + "\n")
		}
		if err := os.WriteFile(filepath.Join(bin, name), script, 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	d := &Docker{StateDir: root}
	for _, dir := range []string{filepath.Join(root, "operations"), d.appDir("dpl_test")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(d.appDir("dpl_test"), "compose.yaml"), []byte("new compose"), 0600); err != nil {
		t.Fatal(err)
	}
	r := recoveryFixture()
	r.Phase = "busy"
	w, err := d.createPreparationWork(&sdkclient.PollCommand{ID: "cmd_test"}, r.DeploymentID, step)
	if err != nil {
		t.Fatal(err)
	}
	r.Work = w
	return d, r, bin
}

// A pull worker that never writes a receipt: the defined failure may only
// be reported after the unit was stopped and confirmed inactive.
func TestPullWorkerNoReceiptStopsUnitBeforeDefinedFailure(t *testing.T) {
	defer withPullBudgetKnobs(300*time.Millisecond, 400*time.Millisecond, 600*time.Millisecond, 50*time.Millisecond)()
	d, r, bin := workFixtureSystemd(t, "pull")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := d.waitPreparationWork(ctx, r.Work, r.DeploymentID)
	if err == nil || !strings.Contains(err.Error(), "can be retried") {
		t.Fatalf("missing defined retryable failure: %v", err)
	}
	log, _ := os.ReadFile(filepath.Join(bin, "systemctl.log"))
	if !strings.Contains(string(log), "stop impreza-preparation-"+r.Work.ID+".service") {
		t.Fatalf("failure reported without stopping the worker unit; systemctl log: %q", log)
	}
	if !strings.Contains(string(log), "show impreza-preparation-"+r.Work.ID+".service") {
		t.Fatalf("unit stop never confirmed inactive; systemctl log: %q", log)
	}
}
