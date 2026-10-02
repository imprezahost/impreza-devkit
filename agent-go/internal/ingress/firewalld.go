package ingress

import (
	"bufio"
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"time"
)

// firewalld reload: `firewall-cmd --reload` rebuilds the firewall and, on
// hosts where Docker registers with firewalld, drops the ingress rules until
// the next reconcile. Docker itself listens for the same D-Bus signal; the
// agent does too, through `gdbus monitor` (glib2, which firewalld requires),
// so no D-Bus library is added. Each Reloaded signal triggers the reconcile
// at once and twice more shortly after (Docker re-creates its own chains in
// reaction to the same signal). Without gdbus or firewalld the watcher backs
// off and retries; the periodic reconcile stays the safety net.

// FirewalldReloadSignal is the member firewalld emits after a reload.
const FirewalldReloadSignal = "org.fedoraproject.FirewallD1.Reloaded"

// ReloadDelays are the reconcile times after each reload signal.
var ReloadDelays = []time.Duration{200 * time.Millisecond, time.Second, 3 * time.Second}

// monitorStarter starts the signal monitor and returns its output; tests
// replace it.
type monitorStarter func(ctx context.Context) (io.ReadCloser, func() error, error)

func gdbusMonitor(ctx context.Context) (io.ReadCloser, func() error, error) {
	if _, err := exec.LookPath("gdbus"); err != nil {
		return nil, nil, errors.New("gdbus not available")
	}
	cmd := exec.CommandContext(ctx, "gdbus", "monitor", "--system", "--dest", "org.fedoraproject.FirewallD1")
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	return out, cmd.Wait, nil
}

// WatchFirewalldReload calls onReload for every firewalld reload until ctx
// ends. It never returns an error: a missing monitor or bus is retried with a
// backoff up to maxBackoff.
func WatchFirewalldReload(ctx context.Context, onReload func()) {
	watchReload(ctx, gdbusMonitor, onReload, 5*time.Second, 5*time.Minute)
}

func watchReload(ctx context.Context, start monitorStarter, onReload func(), backoff, maxBackoff time.Duration) {
	wait := backoff
	for ctx.Err() == nil {
		out, done, err := start(ctx)
		if err == nil {
			sc := bufio.NewScanner(io.LimitReader(out, 1<<30))
			sc.Buffer(make([]byte, 4096), 64*1024)
			seen := false
			for sc.Scan() {
				seen = true
				if strings.Contains(sc.Text(), FirewalldReloadSignal) {
					onReload()
				}
			}
			out.Close()
			_ = done()
			if seen {
				wait = backoff // a monitor that ran resets the backoff
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if wait *= 2; wait > maxBackoff {
			wait = maxBackoff
		}
	}
}

// AfterReload runs fn at each of ReloadDelays, coalescing signals that
// arrive while a series is pending.
func AfterReload(ctx context.Context, fn func()) func() {
	pending := make(chan struct{}, 1)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-pending:
				start := time.Now()
				for _, d := range ReloadDelays {
					select {
					case <-ctx.Done():
						return
					case <-time.After(time.Until(start.Add(d))):
					}
					fn()
				}
			}
		}
	}()
	return func() {
		select {
		case pending <- struct{}{}:
		default: // a series is already queued
		}
	}
}
