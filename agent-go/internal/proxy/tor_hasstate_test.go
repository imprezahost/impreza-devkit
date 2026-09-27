package proxy

// HasState must only report "no state" on a VERIFIED absence — a stat
// error is not absence, and a surviving impreza_tor container (even
// stopped) means the daemon may still hold services the disk no longer
// shows.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestHasStateVerifiesBeforeAbsence(t *testing.T) {
	t.Run("no state and no container", func(t *testing.T) {
		tor := newTestTor(t)
		tor.ContainerPresent = func(context.Context) (bool, error) { return false, nil }
		has, err := tor.HasState(context.Background())
		if err != nil || has {
			t.Fatalf("empty host must report no state: %v %v", has, err)
		}
	})
	t.Run("no state but container present", func(t *testing.T) {
		tor := newTestTor(t)
		tor.ContainerPresent = func(context.Context) (bool, error) { return true, nil }
		has, err := tor.HasState(context.Background())
		if err != nil || !has {
			t.Fatalf("a surviving impreza_tor container must count as state: %v %v", has, err)
		}
	})
	t.Run("probe failure is not absence", func(t *testing.T) {
		tor := newTestTor(t)
		tor.ContainerPresent = func(context.Context) (bool, error) { return false, errors.New("docker daemon unreachable") }
		has, err := tor.HasState(context.Background())
		if err == nil || has {
			t.Fatalf("an unverifiable container must not read as no state: %v %v", has, err)
		}
	})
	t.Run("stat error is not absence", func(t *testing.T) {
		tor := newTestTor(t)
		tor.StateDir += string(rune(0)) // Lstat fails with EINVAL, never ErrNotExist
		has, err := tor.HasState(context.Background())
		if err == nil || has {
			t.Fatalf("an unreadable state tree must not read as no state: %v %v", has, err)
		}
	})
	t.Run("on-disk state answers alone", func(t *testing.T) {
		tor := newTestTor(t)
		tor.ContainerPresent = func(context.Context) (bool, error) {
			return false, errors.New("probe must not run when state exists")
		}
		if err := os.MkdirAll(filepath.Join(tor.StateDir, "data"), 0o700); err != nil {
			t.Fatal(err)
		}
		has, err := tor.HasState(context.Background())
		if err != nil || !has {
			t.Fatalf("existing state must answer without the container probe: %v %v", has, err)
		}
	})
}
