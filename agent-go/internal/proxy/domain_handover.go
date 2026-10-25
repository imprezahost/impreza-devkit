package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var handoverDeployment = regexp.MustCompile(`^dpl_(?:[a-f0-9]{16}|[a-f0-9]{24})$`)
var handoverHostname = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+$`)

// Recovery is limited to this exact single-app transition. A traffic switch
// journal for a different operation must never be removed or interpreted here.
func (c *Caddy) domainHandoverJournal(id, after string) (*routingSwitchRecord, error) {
	if !handoverDeployment.MatchString(id) || !handoverHostname.MatchString(after) {
		return nil, ErrRoutingRecoveryRequired
	}
	info, err := os.Lstat(c.switchRecordPath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > 4*1024*1024 {
		return nil, ErrRoutingRecoveryRequired
	}
	raw, err := os.ReadFile(c.switchRecordPath())
	if err != nil {
		return nil, err
	}
	var r routingSwitchRecord
	if json.Unmarshal(raw, &r) != nil || r.Version != 1 || r.Source != id || r.Target != id || r.Hostname != after || len(r.SourceBefore) == 0 || r.TargetExisted || len(r.TargetBefore) != 0 {
		return nil, ErrRoutingRecoveryRequired
	}
	return &r, nil
}

func hostBlock(block, host string) bool {
	return host != "" && (strings.HasPrefix(block, host+" {") || strings.HasPrefix(block, "http://"+host+" {"))
}

// FinishDomainHandover is the commit after a verified probe. The live fragment still serves BOTH the
// previous and the new hostname; this drops the previous block, reloads, then completes the journal.
// A fragment that already serves only the new hostname is not rewritten. A pending journal still regenerates
// and reloads before it is removed: reload is idempotent, and completing without it leaves Caddy on the overlap.
func (c *Caddy) FinishDomainHandover(ctx context.Context, id, before, after string) error {
	if (before != "" && !handoverHostname.MatchString(before)) || before == after {
		return ErrRoutingRecoveryRequired
	}
	r, err := c.domainHandoverJournal(id, after)
	if err != nil {
		return err
	}
	if r == nil {
		return nil
	}
	if before != "" {
		path := filepath.Join(c.StateDir, "deployments", id+".caddy")
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Size() > 2*1024*1024 {
			return ErrRoutingRecoveryRequired
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		header, blocks, err := splitFragment(string(raw))
		if err != nil {
			return errors.Join(ErrRoutingRecoveryRequired, err)
		}
		var kept []string
		dropped, serving := 0, 0
		for _, block := range blocks {
			if hostBlock(block, before) {
				dropped++
				continue
			}
			if hostBlock(block, after) {
				serving++
			}
			kept = append(kept, block)
		}
		if serving != 1 || dropped > 1 {
			return ErrRoutingRecoveryRequired
		}
		if dropped == 1 {
			if err := writeSwitchFile(path, []byte(strings.Join(append(header, kept...), "\n")+"\n")); err != nil {
				return errors.Join(ErrRoutingRecoveryRequired, err)
			}
		}
		// dropped == 0 is the retry after a crash or a failed regenerate: the new fragment is already on disk
		// and Caddy may still be serving the overlap. Reload before the journal is removed.
		if err := c.regenerateCaddyfile(); err != nil {
			return errors.Join(ErrRoutingRecoveryRequired, err)
		}
		if err := c.reloadSwitch(ctx); err != nil {
			if dropped != 1 {
				return errors.Join(ErrRoutingRecoveryRequired, err)
			}
			// Keep disk equal to what Caddy still runs (the overlap), so a retried commit drops and reloads again.
			restoreErr := writeSwitchFile(path, raw)
			if restoreErr == nil {
				restoreErr = c.regenerateCaddyfile()
			}
			return errors.Join(ErrRoutingRecoveryRequired, err, restoreErr)
		}
	}
	return c.CompleteHostnameSwitch(after, id, id)
}

func (c *Caddy) RestoreDomainHandover(ctx context.Context, id, before, after string) error {
	if (before != "" && !handoverHostname.MatchString(before)) || before == after {
		return ErrRoutingRecoveryRequired
	}
	r, err := c.domainHandoverJournal(id, after)
	if err != nil {
		return err
	}
	path := filepath.Join(c.StateDir, "deployments", id+".caddy")
	var previous []byte
	if r != nil {
		previous = r.SourceBefore
	} else {
		info, err := os.Lstat(path)
		if os.IsNotExist(err) && before == "" {
			previous = []byte("# No previous clearnet route.\n")
		} else {
			if err != nil || !info.Mode().IsRegular() || info.Size() > 2*1024*1024 {
				return ErrRoutingRecoveryRequired
			}
			previous, err = os.ReadFile(path)
			if err != nil {
				return err
			}
		}
	}
	_, blocks, err := splitFragment(string(previous))
	if err != nil {
		return err
	}
	found := 0
	for _, block := range blocks {
		if before == "" && !strings.HasSuffix(strings.SplitN(block, "\n", 2)[0], ".onion {") {
			return ErrRoutingRecoveryRequired
		}
		if strings.HasPrefix(block, after+" {") || strings.HasPrefix(block, "http://"+after+" {") {
			return ErrRoutingRecoveryRequired
		}
		if strings.HasPrefix(block, before+" {") || strings.HasPrefix(block, "http://"+before+" {") {
			found++
		}
	}
	if (before != "" && found != 1) || (before == "" && found != 0) {
		return ErrRoutingRecoveryRequired
	}
	if err := writeSwitchFile(path, previous); err != nil {
		return err
	}
	if err := c.regenerateCaddyfile(); err != nil {
		return err
	}
	if err := c.reloadSwitch(ctx); err != nil {
		return err
	}
	// The previous fragment is live again: only the journal is left to complete (never a commit).
	if r == nil {
		return nil
	}
	return c.CompleteHostnameSwitch(after, id, id)
}

// BeginDomainHandover keeps the previous fragment in a private durable journal.
// The caller must probe the new route and finish the journal, or invoke undo.
// An interrupted operation blocks every unrelated proxy mutation on restart.
//
// it publishes an OVERLAP fragment, the reviewed previous block plus the new one, so the current domain
// keeps its certificate and keeps serving while the new hostname obtains its own. FinishDomainHandover drops
// the previous block only after the caller's probe passed; undo restores the original exactly, as before.
func (c *Caddy) BeginDomainHandover(ctx context.Context, id, before, after string, routes []Route) (func(context.Context) error, error) {
	if !handoverDeployment.MatchString(id) || (before != "" && !handoverHostname.MatchString(before)) || !handoverHostname.MatchString(after) ||
		len(before) > 253 || len(after) > 253 || before == after || strings.HasSuffix(before, ".onion") || strings.HasSuffix(after, ".onion") ||
		len(routes) != 1 || routes[0].Hostname != after {
		return nil, errors.New("domain handover identity cannot be verified")
	}
	if err := c.guardRoutingSwitch(); err != nil {
		return nil, err
	}
	if err := c.ensureDirs(); err != nil {
		return nil, err
	}
	path := filepath.Join(c.StateDir, "deployments", id+".caddy")
	info, err := os.Lstat(path)
	var original []byte
	if os.IsNotExist(err) && before == "" {
		original = []byte("# No previous clearnet route.\n")
	} else {
		if err != nil || !info.Mode().IsRegular() || info.Size() > 2*1024*1024 {
			return nil, errors.New("domain handover fragment cannot be verified")
		}
		original, err = os.ReadFile(path)
		if err != nil {
			return nil, err
		}
	}
	header, blocks, err := splitFragment(string(original))
	if err != nil {
		return nil, err
	}
	found := 0
	for _, block := range blocks {
		if hostBlock(block, before) {
			found++
		}
		if hostBlock(block, after) {
			return nil, errors.New("application already serves the new domain")
		}
	}
	if (before != "" && found != 1) || (before == "" && found != 0) {
		return nil, errors.New("application is not serving the reviewed previous domain")
	}
	if err := validateBasicAuth(routes[0].BasicAuth); err != nil {
		return nil, err
	}
	candidate := []byte(renderFragment(id, routes))
	_, next, err := splitFragment(string(candidate))
	if err != nil {
		return nil, err
	}
	// Other blocks, especially the onion endpoint, must retain their policy.
	other := func(all []string, host string) string {
		var kept []string
		for _, b := range all {
			if !strings.HasPrefix(b, host+" {") && !strings.HasPrefix(b, "http://"+host+" {") {
				kept = append(kept, strings.TrimSpace(b))
			}
		}
		return strings.Join(kept, "\n")
	}
	if other(blocks, before) != other(next, after) {
		return nil, errors.New("domain handover would change another endpoint")
	}
	// The live fragment while the new hostname is unproven: everything the app serves today, plus the new block.
	// A first clearnet domain (before == "") has nothing to keep serving, so it publishes the candidate directly.
	live := candidate
	if before != "" {
		var added []string
		for _, b := range next {
			if hostBlock(b, after) {
				added = append(added, b)
			}
		}
		if len(added) != 1 {
			return nil, errors.New("domain handover candidate does not have exactly one new block")
		}
		live = []byte(strings.Join(append(append(append([]string{}, header...), blocks...), added...), "\n") + "\n")
		if _, check, err := splitFragment(string(live)); err != nil || len(check) != len(blocks)+1 {
			return nil, errors.New("domain handover overlap fragment cannot be verified")
		}
	}
	if err := c.beginRoutingSwitch(routingSwitchRecord{Version: 1, Hostname: after, Source: id, Target: id, SourceBefore: original}); err != nil {
		return nil, errors.Join(ErrRoutingRecoveryRequired, err)
	}
	undo := func(_ context.Context) error {
		recovery, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := writeSwitchFile(path, original); err != nil {
			return errors.Join(ErrRoutingRecoveryRequired, err)
		}
		if err := c.regenerateCaddyfile(); err != nil {
			return errors.Join(ErrRoutingRecoveryRequired, err)
		}
		if err := c.reloadSwitch(recovery); err != nil {
			return errors.Join(ErrRoutingRecoveryRequired, err)
		}
		return c.CompleteHostnameSwitch(after, id, id)
	}
	for _, step := range []func() error{
		func() error { return writeSwitchFile(path, live) }, c.regenerateCaddyfile, func() error { return c.reloadSwitch(ctx) },
	} {
		if err := step(); err != nil {
			if rollbackErr := undo(ctx); rollbackErr != nil {
				return nil, errors.Join(ErrRoutingRecoveryRequired, err, rollbackErr)
			}
			return nil, fmt.Errorf("domain handover rolled back: %w", err)
		}
	}
	return undo, nil
}
