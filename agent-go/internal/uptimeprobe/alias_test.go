package uptimeprobe

import (
	"net/http"
)

// httpTransport and hostDialer are thin aliases so tests can build a
// guard dialer without importing probeguard directly everywhere.
type httpTransport = http.Transport
