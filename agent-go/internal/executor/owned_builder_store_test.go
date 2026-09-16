package executor

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func builderStoreFixture(t *testing.T) (string, *ownedBuilderRecord, *ownedBuilderIdentity) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("requires production Linux filesystem locks")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	b, w, _ := ownedBuilderFixture()
	r := &ownedBuilderRecord{Version: 1, Phase: "intent", Work: *w, DeploymentID: b.DeploymentID, BootID: "12345678-1234-1234-1234-123456789abc", DaemonID: "local-fixture-daemon", ImageID: b.ImageID}
	return dir, r, b
}

func TestOwnedBuilderJournalPersistsExclusiveIntentAndOrderedTransitions(t *testing.T) {
	dir, r, b := builderStoreFixture(t)
	if err := createOwnedBuilderIntent(dir, r); err != nil {
		t.Fatal(err)
	}
	if err := createOwnedBuilderIntent(dir, r); err == nil {
		t.Fatal("ambiguous launch could overwrite original intent")
	}
	for _, step := range []struct {
		from, to string
		identity *ownedBuilderIdentity
	}{{"intent", "bound", b}, {"bound", "stopping", nil}, {"stopping", "stopped", nil}} {
		if err := transitionOwnedBuilder(dir, &r.Work, r.DeploymentID, r.BootID, r.DaemonID, step.from, step.to, step.identity); err != nil {
			t.Fatal(err)
		}
		got, err := loadOwnedBuilderRecord(dir, &r.Work, r.DeploymentID, r.BootID, r.DaemonID)
		if err != nil || got.Phase != step.to || got.Identity.ContainerID != b.ContainerID {
			t.Fatal(got, err)
		}
		if err := transitionOwnedBuilder(dir, &r.Work, r.DeploymentID, r.BootID, r.DaemonID, step.from, step.to, step.identity); err == nil {
			t.Fatal("replayed transition accepted")
		}
	}
	info, err := os.Stat(filepath.Join(dir, "builder.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("journal not private", err)
	}
}

func TestOwnedBuilderJournalRejectsForeignIdentityWithoutChangingBytes(t *testing.T) {
	for _, kind := range []string{"work", "request", "deployment", "boot", "daemon", "image", "container", "skip", "rewrite"} {
		t.Run(kind, func(t *testing.T) {
			dir, r, b := builderStoreFixture(t)
			if err := createOwnedBuilderIntent(dir, r); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(filepath.Join(dir, "builder.json"))
			if err != nil {
				t.Fatal(err)
			}
			work := r.Work
			deployment, boot, daemon := r.DeploymentID, r.BootID, r.DaemonID
			from, to := "intent", "bound"
			switch kind {
			case "work":
				work.ID = strings.Repeat("a", 32)
			case "request":
				work.RequestSHA256 = strings.Repeat("b", 64)
			case "deployment":
				deployment = "dpl_foreign"
			case "boot":
				boot = "87654321-1234-1234-1234-123456789abc"
			case "daemon":
				daemon = "foreign"
			case "image":
				b.ImageID = "sha256:" + strings.Repeat("a", 64)
			case "container":
				b.ContainerID = "short"
			case "skip":
				to = "stopped"
			case "rewrite":
				from = "bound"
			}
			if err = transitionOwnedBuilder(dir, &work, deployment, boot, daemon, from, to, b); err == nil {
				t.Fatal("foreign transition accepted")
			}
			after, _ := os.ReadFile(filepath.Join(dir, "builder.json"))
			if string(before) != string(after) {
				t.Fatal("rejected transition modified journal")
			}
		})
	}
}

func TestOwnedBuilderJournalConcurrentBindingHasOneWinner(t *testing.T) {
	dir, r, b := builderStoreFixture(t)
	if err := createOwnedBuilderIntent(dir, r); err != nil {
		t.Fatal(err)
	}
	var wins atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if transitionOwnedBuilder(dir, &r.Work, r.DeploymentID, r.BootID, r.DaemonID, "intent", "bound", b) == nil {
				wins.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatal("expected exactly one binding", wins.Load())
	}
}

func TestOwnedBuilderJournalRefusesInvalidStorage(t *testing.T) {
	for _, kind := range []string{"symlink record", "symlink lock", "truncated", "trailing", "public record", "public directory", "record directory"} {
		t.Run(kind, func(t *testing.T) {
			dir, r, b := builderStoreFixture(t)
			if err := createOwnedBuilderIntent(dir, r); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "builder.json")
			outside := filepath.Join(t.TempDir(), "protected")
			if err := os.WriteFile(outside, []byte("untouched"), 0600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "symlink record", "record directory":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if kind == "symlink record" {
					if err := os.Symlink(outside, path); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := os.Mkdir(path, 0700); err != nil {
						t.Fatal(err)
					}
				}
			case "symlink lock":
				lock := filepath.Join(dir, "builder.lock")
				if err := os.Remove(lock); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, lock); err != nil {
					t.Fatal(err)
				}
			case "truncated":
				if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			case "trailing":
				f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				_, err = f.WriteString(" {}")
				f.Close()
				if err != nil {
					t.Fatal(err)
				}
			case "public record":
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
			case "public directory":
				if err := os.Chmod(dir, 0755); err != nil {
					t.Fatal(err)
				}
			}
			if err := transitionOwnedBuilder(dir, &r.Work, r.DeploymentID, r.BootID, r.DaemonID, "intent", "bound", b); err == nil {
				t.Fatal("invalid storage accepted")
			}
			data, _ := os.ReadFile(outside)
			if string(data) != "untouched" {
				t.Fatal("foreign file modified")
			}
		})
	}
}
