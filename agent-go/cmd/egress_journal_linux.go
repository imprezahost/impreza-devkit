//go:build linux

package cmd

import (
	"fmt"
	"log/syslog"
	"strings"
)

// cliNotify sends the egress host line of an operator command to the local
// syslog (journald on systemd hosts, identifier impreza-agent), next to the
// daemon's own lines. Counts and a fingerprint only, as in the daemon.
func cliNotify(msg string, args ...any) {
	w, err := syslog.New(syslog.LOG_INFO|syslog.LOG_DAEMON, "impreza-agent")
	if err != nil {
		return
	}
	defer w.Close()
	var b strings.Builder
	b.WriteString(msg + " (operator command)")
	for i := 0; i+1 < len(args); i += 2 {
		fmt.Fprintf(&b, " %v=%v", args[i], args[i+1])
	}
	_ = w.Info(b.String())
}
