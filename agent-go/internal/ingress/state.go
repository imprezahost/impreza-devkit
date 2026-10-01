package ingress

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Stable reason codes reported when a revision is not enforced.
const (
	ReasonUnavailable   = "iptables_unavailable"
	ReasonIPv6          = "ipv6_unavailable"
	ReasonApplyFailed   = "apply_failed"
	ReasonResetLocally  = "reset_locally"
	ReasonNotPublished  = "port_not_published"
	ReasonInvalidPolicy = "invalid_policy"
)

const stateFile = "ingress.json"
const stateLimit = 1 << 20

// Entry is one deployment's desired state and what is known to be live.
type Entry struct {
	Desired      Policy  `json:"desired"`
	Applied      *Policy `json:"applied,omitempty"`
	Enforced     bool    `json:"enforced"`
	Fingerprint  string  `json:"fingerprint,omitempty"`
	Reason       string  `json:"reason,omitempty"`
	ResetLocally bool    `json:"reset_locally,omitempty"`
	UpdatedAt    string  `json:"updated_at"`
}

type document struct {
	Version     int               `json:"version"`
	Deployments map[string]*Entry `json:"deployments"`
	// Live holds the per-family fingerprint of the chains as last applied,
	// so a reconcile notices a flush (Docker, ufw, firewalld) or drift.
	Live map[Family]string `json:"live,omitempty"`
	// Bridges are the Docker bridges exempted in the last apply, reused when
	// Docker cannot be asked (the boot restore runs before docker.service).
	Bridges []string `json:"bridges,omitempty"`
}

// Status is the per-deployment view reported to the control plane.
type Status struct {
	DeploymentID string
	Revision     uint32
	Enforced     bool
	Fingerprint  string
	Reason       string
}

// Manager owns ingress.json and the chains. All methods take the state lock.
type Manager struct {
	StateDir string
	Engine   Engine
	// HasGlobalIPv6 decides whether a host without ip6tables is a failure.
	HasGlobalIPv6 func() bool
	Now           func() time.Time
	// Bridges lists the Docker bridges whose traffic is the host's own
	// containers; PublicInterfaces the interfaces of the default routes.
	Bridges          func(context.Context) ([]string, error)
	PublicInterfaces func() []string
}

// NewManager returns a Manager bound to the host binaries.
func NewManager(stateDir string) *Manager {
	return &Manager{StateDir: stateDir, Engine: Engine{Run: RealRunner}, HasGlobalIPv6: hostHasGlobalIPv6, Now: time.Now,
		Bridges:          func(ctx context.Context) ([]string, error) { return DockerBridges(ctx, RealRunner) },
		PublicInterfaces: DefaultRouteInterfaces}
}

// bridgeTimeout bounds the question to Docker, apart from the kernel apply:
// a Docker that does not answer never costs the rules their deadline.
var bridgeTimeout = 5 * time.Second

// discoverBridges asks Docker for its bridges, within bridgeTimeout.
func (m *Manager) discoverBridges(ctx context.Context) ([]string, error) {
	if m.Bridges == nil {
		return nil, errors.New("no bridge discovery")
	}
	ctx, cancel := context.WithTimeout(ctx, bridgeTimeout)
	defer cancel()
	return m.Bridges(ctx)
}

// bridges asks Docker, or falls back to the list of the last apply. The boot
// restore never asks (discover false): it runs before docker.service, and
// with docker.socket listening the CLI would wait on a daemon that is
// ordered after it.
func (m *Manager) bridges(ctx context.Context, doc *document, discover bool) []string {
	if discover {
		if b, err := m.discoverBridges(ctx); err == nil {
			return b
		}
	}
	return doc.Bridges
}

// publicExempt names a default-route interface that is in the exemption list.
func (m *Manager) publicExempt(bridges []string) string {
	if m.PublicInterfaces == nil {
		return ""
	}
	for _, pub := range m.PublicInterfaces() {
		for _, b := range bridges {
			if pub == b {
				return pub
			}
		}
	}
	return ""
}

// PolicyFingerprint is the digest the control plane recomputes for the
// revision it sent: the agent reports it only once the rules are live, so a
// green status proves this exact allowlist, not merely "something applied".
func PolicyFingerprint(p Policy) string {
	var b strings.Builder
	b.WriteString("impreza-ingress-v1\n")
	b.WriteString(p.DeploymentID + "\n")
	b.WriteString(fmt.Sprint(p.Revision) + "\n")
	for _, r := range p.Rules {
		b.WriteString(fmt.Sprintf("%s/%d %s\n", r.Protocol, r.Port, strings.Join(r.Sources, ",")))
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

func (m *Manager) path() string { return filepath.Join(m.StateDir, stateFile) }

func (m *Manager) load() (*document, error) {
	doc := &document{Version: 1, Deployments: map[string]*Entry{}}
	info, err := os.Lstat(m.path())
	if errors.Is(err, os.ErrNotExist) {
		return doc, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > stateLimit {
		return nil, errors.New("ingress state is not a regular file")
	}
	f, err := os.Open(m.path())
	if err != nil {
		return nil, errors.New("ingress state unreadable")
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, stateLimit+1))
	if err != nil || len(raw) > stateLimit {
		return nil, errors.New("ingress state unreadable")
	}
	if err := json.Unmarshal(raw, doc); err != nil || doc.Version != 1 {
		return nil, errors.New("ingress state is corrupt")
	}
	if doc.Deployments == nil {
		doc.Deployments = map[string]*Entry{}
	}
	// What is on disk is validated like what comes from the server.
	for id, e := range doc.Deployments {
		n, err := Normalize(e.Desired)
		if err != nil || n.DeploymentID != id {
			return nil, errors.New("ingress state holds an invalid policy")
		}
		e.Desired = n
	}
	return doc, nil
}

func (m *Manager) save(doc *document) error {
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(m.StateDir, ".ingress-*.json")
	if err != nil {
		return errors.New("ingress state could not be written")
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return errors.New("ingress state could not be written")
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return errors.New("ingress state could not be written")
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return errors.New("ingress state could not be written")
	}
	if err := tmp.Close(); err != nil {
		return errors.New("ingress state could not be written")
	}
	var renameErr error
	for attempt := 0; attempt < 5; attempt++ {
		// A scanner can hold the target for an instant off Linux (tests).
		if renameErr = os.Rename(name, m.path()); renameErr == nil {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return errors.New("ingress state could not be written")
}

func (m *Manager) stamp() string { return m.Now().UTC().Format(time.RFC3339) }

// active are the policies rendered into the kernel: every deployment with at
// least one restricted rule, except those reset locally.
func active(doc *document) []Policy {
	ids := make([]string, 0, len(doc.Deployments))
	for id := range doc.Deployments {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var out []Policy
	for _, id := range ids {
		e := doc.Deployments[id]
		if e.ResetLocally || len(e.Desired.Rules) == 0 {
			continue
		}
		out = append(out, e.Desired)
	}
	return out
}

// applyAll renders the whole host set. With nothing restricted, the chains
// and jumps are removed (no orphan chain). It records the outcome on every
// entry: enforced only when both families hold the new set.
func (m *Manager) applyAll(ctx context.Context, doc *document, discover bool) error {
	policies := active(doc)
	bridges := m.bridges(ctx, doc, discover)
	var reason string
	var applyErr error
	live := map[Family]string{}
	if len(policies) == 0 {
		for _, f := range []Family{V4, V6} {
			if err := m.Engine.removeFamily(ctx, f); err != nil && applyErr == nil {
				applyErr, reason = err, ReasonApplyFailed
			}
		}
	} else if pub := m.publicExempt(bridges); pub != "" {
		// Exempting the host's containers would exempt the internet: apply
		// nothing (the previous set stays) and say why.
		applyErr, reason = errors.New("the default-route interface "+pub+" is a Docker bridge"), ReasonPublicBridge
	} else {
		for _, f := range []Family{V4, V6} {
			l, err := m.Engine.applyFamily(ctx, f, policies, bridges)
			switch {
			case err == nil:
				live[f] = l.Fingerprint
			case errors.Is(err, errUnavailable) && f == V6 && !m.HasGlobalIPv6():
				// No routable IPv6 on this host: nothing to restrict there.
			case errors.Is(err, errUnavailable) && f == V6:
				applyErr, reason = errors.New("ip6tables unavailable on a host with IPv6"), ReasonIPv6
			case errors.Is(err, errUnavailable):
				applyErr, reason = errors.New("iptables unavailable"), ReasonUnavailable
			default:
				applyErr, reason = err, ReasonApplyFailed
			}
			if applyErr != nil {
				break
			}
		}
	}
	now := m.stamp()
	for _, e := range doc.Deployments {
		if e.ResetLocally {
			e.Enforced, e.Fingerprint, e.Reason = false, "", ReasonResetLocally
			continue
		}
		if applyErr != nil {
			// The previous set may still be live, but this revision is not:
			// never green on a failure.
			e.Enforced, e.Fingerprint, e.Reason, e.UpdatedAt = false, "", reason, now
			continue
		}
		applied := e.Desired
		e.Applied = &applied
		e.Enforced, e.Fingerprint, e.Reason, e.UpdatedAt = true, PolicyFingerprint(e.Desired), "", now
	}
	if applyErr == nil {
		doc.Live = live
		doc.Bridges = bridges
	}
	return applyErr
}

// Update stores and enforces the complete desired state of one deployment.
// A revision older than the stored one is ignored (the latest command wins;
// a replay can never roll the allowlist back). The same revision again is
// an idempotent re-apply.
func (m *Manager) Update(ctx context.Context, p Policy) (Status, error) {
	n, err := Normalize(p)
	if err != nil {
		return Status{DeploymentID: p.DeploymentID, Revision: p.Revision, Reason: ReasonInvalidPolicy}, err
	}
	unlock, err := lockState(m.StateDir)
	if err != nil {
		return Status{}, err
	}
	defer unlock()
	doc, err := m.load()
	if err != nil {
		return Status{}, err
	}
	if cur := doc.Deployments[n.DeploymentID]; cur != nil && cur.Desired.Revision > n.Revision {
		return statusOf(cur), fmt.Errorf("revision %d is older than the stored revision %d", n.Revision, cur.Desired.Revision)
	}
	doc.Deployments[n.DeploymentID] = &Entry{Desired: n, Applied: appliedOf(doc.Deployments[n.DeploymentID]), UpdatedAt: m.stamp()}
	applyErr := m.applyAll(ctx, doc, true)
	if err := m.save(doc); err != nil {
		return Status{}, err
	}
	return statusOf(doc.Deployments[n.DeploymentID]), applyErr
}

func appliedOf(e *Entry) *Policy {
	if e == nil {
		return nil
	}
	return e.Applied
}

func statusOf(e *Entry) Status {
	return Status{DeploymentID: e.Desired.DeploymentID, Revision: e.Desired.Revision, Enforced: e.Enforced,
		Fingerprint: e.Fingerprint, Reason: e.Reason}
}

// Remove forgets a deployment (uninstall) and re-renders the host set; the
// last deployment removes the chains entirely.
func (m *Manager) Remove(ctx context.Context, deploymentID string) error {
	if _, err := os.Lstat(m.path()); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	unlock, err := lockState(m.StateDir)
	if err != nil {
		return err
	}
	defer unlock()
	doc, err := m.load()
	if err != nil {
		return err
	}
	if _, ok := doc.Deployments[deploymentID]; !ok {
		return nil
	}
	delete(doc.Deployments, deploymentID)
	applyErr := m.applyAll(ctx, doc, true)
	if err := m.save(doc); err != nil {
		return err
	}
	return applyErr
}

// Reconcile re-applies when the kernel no longer matches the last apply (a
// Docker restart, ufw reload or firewalld reload moved or flushed the
// chains) or when the last attempt failed. It is cheap when nothing drifted.
func (m *Manager) Reconcile(ctx context.Context) error {
	unlock, err := lockState(m.StateDir)
	if err != nil {
		return err
	}
	defer unlock()
	doc, err := m.load()
	if err != nil {
		return err
	}
	policies := active(doc)
	if len(policies) == 0 && len(doc.Deployments) == 0 && len(doc.Live) == 0 {
		return nil
	}
	if !m.drifted(ctx, doc, policies) {
		return nil
	}
	applyErr := m.applyAll(ctx, doc, true)
	if err := m.save(doc); err != nil {
		return err
	}
	return applyErr
}

func (m *Manager) drifted(ctx context.Context, doc *document, policies []Policy) bool {
	for _, e := range doc.Deployments {
		if !e.ResetLocally && !e.Enforced {
			return true
		}
	}
	if len(policies) == 0 {
		return len(doc.Live) != 0
	}
	// A new or removed Docker network changes who is exempted, and a default
	// route that moved onto an exempted bridge must stop being green.
	if m.publicExempt(doc.Bridges) != "" {
		return true
	}
	if m.Bridges != nil {
		if b, err := m.discoverBridges(ctx); err == nil && !sameList(b, doc.Bridges) {
			return true
		}
	}
	for _, f := range []Family{V4, V6} {
		want, ok := doc.Live[f]
		if !ok {
			continue
		}
		fwd, host := Rules(f, policies, doc.Bridges)
		l, err := m.Engine.inspect(ctx, f, len(fwd), len(host))
		if err != nil || l.Fingerprint != want {
			return true
		}
	}
	return len(doc.Live) == 0
}

// Restore renders the stored state without contacting anyone: the boot unit
// runs it before docker.service so no published port is reachable before its
// allowlist is live.
func (m *Manager) Restore(ctx context.Context) error {
	unlock, err := lockState(m.StateDir)
	if err != nil {
		return err
	}
	defer unlock()
	doc, err := m.load()
	if err != nil {
		return err
	}
	if len(active(doc)) == 0 {
		return nil
	}
	applyErr := m.applyAll(ctx, doc, false)
	if err := m.save(doc); err != nil {
		return err
	}
	return applyErr
}

// Reset is the local recovery path: it removes both chains and jumps now and
// keeps every deployment unenforced ("reset_locally") until the control
// plane sends a new revision. The heartbeat reports it, so the dashboard
// never shows a reset allowlist as enforced.
func (m *Manager) Reset(ctx context.Context) error {
	unlock, err := lockState(m.StateDir)
	if err != nil {
		return err
	}
	defer unlock()
	doc, err := m.load()
	if err != nil {
		// A corrupt state must not block the recovery path.
		doc = &document{Version: 1, Deployments: map[string]*Entry{}}
	}
	var firstErr error
	for _, f := range []Family{V4, V6} {
		if err := m.Engine.removeFamily(ctx, f); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	for _, e := range doc.Deployments {
		if len(e.Desired.Rules) == 0 {
			continue
		}
		e.ResetLocally, e.Enforced, e.Fingerprint, e.Reason, e.UpdatedAt = true, false, "", ReasonResetLocally, m.stamp()
	}
	doc.Live = nil
	if err := m.save(doc); err != nil {
		return err
	}
	return firstErr
}

// Report lists every stored deployment, sorted, without any source.
func (m *Manager) Report() ([]Status, error) {
	doc, err := m.load()
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(doc.Deployments))
	for id := range doc.Deployments {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]Status, 0, len(ids))
	for _, id := range ids {
		out = append(out, statusOf(doc.Deployments[id]))
	}
	return out, nil
}
