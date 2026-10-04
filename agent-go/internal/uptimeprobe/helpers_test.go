package uptimeprobe

import (
	"net"
	"time"

	sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"
)

// Test-only helpers: the sdk client built for the fake control plane.

type sdkClient = sdkclient.Client

func sdkclientNewAgent(baseURL string) (*sdkclient.Client, error) {
	return sdkclient.NewAgent(sdkclient.AgentOptions{
		AgentID:     "agt_test0000000001",
		AgentSecret: "test-secret",
		BaseURL:     baseURL,
		Timeout:     5 * time.Second,
	})
}

// dialerFor wraps the guard's dialer for the mechanics tests.
func dialerFor(ip net.IP) *httpTransport {
	return hostDialer(ip, 5*time.Second)
}
