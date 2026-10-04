package probeguard

import (
	"fmt"
	"net"
	"time"

	"golang.org/x/net/proxy"
)

// socksDialer resolves the SOCKS5 dialer behind SocksDialer.
func socksDialer(socksAddr string, timeout time.Duration) (proxy.ContextDialer, error) {
	d, err := proxy.SOCKS5("tcp", socksAddr, nil, &net.Dialer{Timeout: timeout, KeepAlive: 0})
	if err != nil {
		return nil, fmt.Errorf("probeguard: socks dialer %s: %w", socksAddr, err)
	}
	cd, ok := d.(proxy.ContextDialer)
	if !ok {
		return nil, fmt.Errorf("probeguard: socks dialer %s lacks DialContext", socksAddr)
	}
	return cd, nil
}
