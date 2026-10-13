//go:build !windows

package proxy

import (
	"os"
	"reflect"
	"testing"
)

// Only the Windows tests swap torRename (tor_rename_windows_test.go); every
// other build, release included, replaces Tor state with os.Rename itself.
func TestTorRenameIsOsRename(t *testing.T) {
	if reflect.ValueOf(torRename).Pointer() != reflect.ValueOf(os.Rename).Pointer() {
		t.Fatal("torRename is not os.Rename outside the Windows tests")
	}
}
