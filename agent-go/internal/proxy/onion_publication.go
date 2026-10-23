package proxy

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const onionPublicationBudget = 30 * time.Second

// WaitForOnionPublication observes a descriptor-upload event from the owning
// daemon, not client reachability. Unsupported images/control fail immediately.
// False is "not confirmed in this bounded observation", NOT publication failure.
// Upload may happen after the observation budget; deploy stays advisory and returns.
// No background observer, event persistence, retry or new worker is started here.
func (t *Tor) WaitForOnionPublication(ctx context.Context, deploymentID, onion string) bool {
	ctx, cancel := context.WithTimeout(ctx, onionPublicationBudget)
	defer cancel()
	if ctx.Err() != nil || !t.ownsPublicationTarget(deploymentID, onion) {
		return false
	}
	dir, err := t.serviceDir(deploymentID)
	if err != nil {
		return false
	}
	target := filepath.Join(dir, "hostname")
	original, err := os.Lstat(target)
	if err != nil {
		return false
	}
	owned := func() bool {
		current, err := os.Lstat(target)
		return err == nil && os.SameFile(original, current) && t.ownsPublicationTarget(deploymentID, onion)
	}
	return t.observeDescriptorUpload(ctx, onion, owned)
}

func (t *Tor) ownsPublicationTarget(deploymentID, onion string) bool {
	if !onionAddressRe.MatchString(onion) {
		return false
	}
	dir, err := t.serviceDir(deploymentID)
	if err != nil {
		return false
	}
	path := filepath.Join(dir, "hostname")
	info, err := os.Lstat(path)
	if err != nil || !singleRegularLink(info) || info.Size() > 128 {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !singleRegularLink(opened) || !os.SameFile(info, opened) {
		return false
	}
	data, err := io.ReadAll(io.LimitReader(f, 129))
	return err == nil && len(data) <= 128 && strings.TrimSpace(string(data)) == onion
}
