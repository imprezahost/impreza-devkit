package executor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func rebootWorkFixture(t *testing.T) (*Docker, *PreparationRecovery, string) {
	d, r, bin := workFixture(t, "build", `if [ "$1" = context ]; then echo unix:///var/run/docker.sock; else echo '{"Current":true,"Name":"default","Driver":"docker","Nodes":[{"Name":"default","Endpoint":"default","Status":"running"}]}'; fi`)
	dir, request, err := d.loadPreparationWork(r.Work, r.DeploymentID)
	if err != nil {
		t.Fatal(err)
	}
	request.BootID = "11111111-1111-1111-1111-111111111111"
	if request.BootID == currentBootID() {
		t.Fatal("fixture boot collision")
	}
	if err := writeWorkJSON(dir, "request.json", request); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(request)
	r.Work.RequestSHA256 = workHash(raw)
	if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte("#!/bin/sh\nprintf 'ActiveState=inactive\\nMainPID=0\\n'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return d, r, bin
}
func TestPreparationRebootNeedsBoundLocalInactiveWorker(t *testing.T) {
	d, r, bin := rebootWorkFixture(t)
	next, err := d.CompletedPreparationWork(r, "cmd_test")
	if err != nil || next.Phase != "aborted" || !next.Recoverable() || r.Phase != "busy" {
		t.Fatalf("reboot: %#v %v", next, err)
	}
	if err := RunPreparationWorker(d.StateDir, r.Work.ID); err == nil {
		t.Fatal("previous boot work replayed")
	}
	os.WriteFile(filepath.Join(bin, "systemctl"), []byte("#!/bin/sh\nprintf 'ActiveState=active\\nMainPID=123\\n'\n"), 0700)
	if _, err := d.preparationAfterReboot(r); err == nil {
		t.Fatal("active worker authorized")
	}
}
func TestPreparationRebootRejectsAmbiguousOrChangedState(t *testing.T) {
	for _, kind := range []string{"owned", "same_boot", "legacy", "unproven_daemon", "remote", "builder", "drift", "receipt", "corrupt_receipt", "identity"} {
		t.Run(kind, func(t *testing.T) {
			d, r, bin := rebootWorkFixture(t)
			dir, request, err := d.loadPreparationWork(r.Work, r.DeploymentID)
			if err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "owned":
				request.OwnedBuilder = true
			case "same_boot":
				request.BootID = currentBootID()
			case "unproven_daemon":
				request.LocalBootRecovery = false
			case "legacy":
				request.BootID = ""
			case "remote":
				request.Env = append(request.Env, "DOCKER_HOST=tcp://remote:2376")
			case "builder":
				os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\nif [ \"$1\" = context ]; then echo unix:///var/run/docker.sock; else echo 'docker-container remote'; fi\n"), 0700)
			case "drift":
				os.WriteFile(filepath.Join(d.appDir(r.DeploymentID), "compose.yaml"), []byte("changed"), 0600)
			case "receipt":
				writeWorkJSON(dir, "result.json", preparationWorkResult{Version: 1, ID: r.Work.ID, RequestSHA256: r.Work.RequestSHA256, Completed: true})
			case "corrupt_receipt":
				os.WriteFile(filepath.Join(dir, "result.json"), []byte("{"), 0600)
			case "identity":
				request.CommandID = "other"
			}
			writeWorkJSON(dir, "request.json", request)
			raw, _ := json.Marshal(request)
			r.Work.RequestSHA256 = workHash(raw)
			if _, err := d.preparationAfterReboot(r); err == nil {
				t.Fatal("unsafe reboot recovery", kind)
			}
		})
	}
}
func TestAbortedPreparationRequiresBoundWorker(t *testing.T) {
	r := recoveryFixture()
	r.Phase = "aborted"
	if r.Validate() == nil {
		t.Fatal("unbound aborted checkpoint accepted")
	}
	r.Work = &PreparationWork{ID: strings.Repeat("a", 32), Step: "build", CommandID: "cmd", RequestSHA256: strings.Repeat("b", 64)}
	if r.Validate() != nil || !r.Recoverable() {
		t.Fatal("valid aborted checkpoint refused")
	}
}

func TestPreparationRebootSupportsVerifiedAbsentBuildxOnly(t *testing.T) {
	d, r, bin := rebootWorkFixture(t)
	script := `#!/bin/sh
case "$1" in
context) echo unix:///var/run/docker.sock;;
buildx) exit 1;;
info) echo '[{"Name":"compose"}]';;
esac
`
	os.WriteFile(filepath.Join(bin, "docker"), []byte(script), 0700)
	if _, err := d.preparationAfterReboot(r); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(bin, "docker"), []byte(strings.ReplaceAll(script, `"compose"`, `"buildx"`)), 0700)
	if _, err := d.preparationAfterReboot(r); err == nil {
		t.Fatal("broken installed Buildx accepted as absent")
	}
}
