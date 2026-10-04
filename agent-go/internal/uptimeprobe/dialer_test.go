package uptimeprobe

import (
	"net"
	"time"

	"github.com/imprezahost/impreza-devkit/agent-go/internal/probeguard"
)

// hostDialer delegates to the guard's dialer.
func hostDialer(ip net.IP, timeout time.Duration) *httpTransport {
	return probeguard.HostDialer(ip, timeout)
}
