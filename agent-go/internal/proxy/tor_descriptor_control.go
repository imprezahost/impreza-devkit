package proxy

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	descriptorControlIO = 2 * time.Second
	descriptorPollTick  = 250 * time.Millisecond
	descriptorMaxLines  = 1024
	descriptorMaxBytes  = 256 * 1024
	safeCookieServerKey = "Tor safe cookie authentication server-to-controller hash"
	safeCookieClientKey = "Tor safe cookie authentication controller-to-server hash"
)

// ControlSocket is Tor's Unix equivalent of ControlPort. The existing private
// data mount makes it reachable by the Go agent without curl/nc/bash in either
// published image. No controller command/address/cookie is a process argument.
func (t *Tor) observeDescriptorUpload(ctx context.Context, onion string, owned func() bool) bool {
	dataDir := filepath.Join(t.StateDir, "data")
	for _, dir := range []string{t.StateDir, dataDir} {
		st, err := os.Lstat(dir)
		if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return false
		}
	}
	socket := filepath.Join(dataDir, "descriptor-control.sock")
	st, err := os.Lstat(socket)
	if err != nil || st.Mode()&os.ModeSocket == 0 || st.Mode().Perm()&0077 != 0 {
		return false // No retry/backoff for a missing/unsafe/old control endpoint.
	}
	cookiePath := filepath.Join(dataDir, "control_auth_cookie")
	cookie, cookieInfo, ok := descriptorCookie(cookiePath)
	if !ok {
		return false
	}
	defer clear(cookie)
	stillOwned := func() bool {
		ci, ce := os.Lstat(cookiePath)
		si, se := os.Lstat(socket)
		return ce == nil && se == nil && singleRegularLink(ci) && ci.Mode().Perm() == 0600 &&
			os.SameFile(ci, cookieInfo) && os.SameFile(si, st) && owned()
	}
	connectCtx, cancel := context.WithTimeout(ctx, descriptorControlIO)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(connectCtx, "unix", socket)
	if err != nil {
		return false
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	c := descriptorControl{conn: conn, reader: bufio.NewReaderSize(conn, 4096), address: strings.TrimSuffix(onion, ".onion")}
	if !c.authenticate(ctx, cookie) || !stillOwned() {
		return false
	}
	c.acceptEvents = true
	if !c.write(ctx, "SETEVENTS HS_DESC\r\n") {
		return false
	}
	if line, ok := c.reply(); !ok || line != "250 OK" {
		return false
	}
	// GETINFO's local descriptor cache proves creation, not upload. Do not turn
	// a cached descriptor or the mere hostname file into a publication claim.
	for ctx.Err() == nil && stillOwned() {
		if c.uploaded {
			return stillOwned()
		}
		_ = conn.SetReadDeadline(controlDeadline(ctx, descriptorPollTick))
		line, err := c.line()
		if err != nil {
			if timeout, ok := err.(net.Error); ok && timeout.Timeout() && line == "" {
				continue
			}
			return false
		}
		if !strings.HasPrefix(line, "650 ") && !strings.HasPrefix(line, "650-") {
			return false
		}
		c.event(line)
	}
	return false
}

func descriptorCookie(path string) ([]byte, os.FileInfo, bool) {
	st, err := os.Lstat(path)
	if err != nil || !singleRegularLink(st) || st.Mode().Perm() != 0600 || st.Size() != 32 {
		return nil, nil, false
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, false
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !singleRegularLink(opened) || opened.Mode().Perm() != 0600 || opened.Size() != 32 || !os.SameFile(st, opened) {
		return nil, nil, false
	}
	b, err := io.ReadAll(io.LimitReader(f, 33))
	return b, st, err == nil && len(b) == 32
}

type descriptorControl struct {
	conn                   net.Conn
	reader                 *bufio.Reader
	address                string
	lines, bytes           int
	acceptEvents, uploaded bool
}

func controlDeadline(ctx context.Context, window time.Duration) time.Time {
	d := time.Now().Add(window)
	if outer, ok := ctx.Deadline(); ok && outer.Before(d) {
		return outer
	}
	return d
}

func (c *descriptorControl) write(ctx context.Context, command string) bool {
	if ctx.Err() != nil {
		return false
	}
	if c.conn.SetDeadline(controlDeadline(ctx, descriptorControlIO)) != nil {
		return false
	}
	n, err := io.WriteString(c.conn, command)
	return err == nil && n == len(command)
}

func (c *descriptorControl) line() (string, error) {
	b, err := c.reader.ReadSlice('\n')
	// An idle read deadline is not a protocol line. Non-empty partial reads
	// still consume budget and fail closed below; never discard hostile bytes.
	if len(b) > 0 {
		c.lines++
	}
	c.bytes += len(b)
	if c.lines > descriptorMaxLines || c.bytes > descriptorMaxBytes {
		return "", io.ErrShortBuffer
	}
	if err != nil {
		return string(b), err
	}
	if len(b) < 2 || b[len(b)-2] != '\r' {
		return "", io.ErrUnexpectedEOF
	}
	return string(b[:len(b)-2]), nil
}

// Only the requested HS_DESC event is subscribed. Extra optional fields/lines
// are ignored, unrelated services and CREATED/UPLOAD/RECEIVED are not success.
// Tor v3 daemons emit UNKNOWN, not NO_AUTH. It is
// metadata, not authority: SAFECOOKIE + exact owned service + UPLOADED govern.
// Keep the explicit NO_AUTH compatibility value; other auth methods refuse.
func (c *descriptorControl) event(line string) {
	if !c.acceptEvents || (!strings.HasPrefix(line, "650 ") && !strings.HasPrefix(line, "650-")) {
		return
	}
	f := strings.Fields(line[4:])
	if len(f) >= 5 && f[0] == "HS_DESC" && f[1] == "UPLOADED" && f[2] == c.address && (f[3] == "UNKNOWN" || f[3] == "NO_AUTH") {
		c.uploaded = true
	}
}

func (c *descriptorControl) reply() (string, bool) {
	for {
		line, err := c.line()
		if err != nil {
			return "", false
		}
		if strings.HasPrefix(line, "650 ") || strings.HasPrefix(line, "650-") {
			c.event(line)
			continue
		}
		if strings.HasPrefix(line, "250-") {
			continue
		}
		return line, strings.HasPrefix(line, "250 ")
	}
}

func cookieMAC(key string, cookie, client, server []byte) []byte {
	h := hmac.New(sha256.New, []byte(key))
	_, _ = h.Write(cookie)
	_, _ = h.Write(client)
	_, _ = h.Write(server)
	return h.Sum(nil)
}

func (c *descriptorControl) authenticate(ctx context.Context, cookie []byte) bool {
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return false
	}
	if !c.write(ctx, "AUTHCHALLENGE SAFECOOKIE "+hex.EncodeToString(nonce)+"\r\n") {
		return false
	}
	line, ok := c.reply()
	if !ok {
		return false
	}
	f := strings.Fields(line)
	if len(f) != 4 || f[1] != "AUTHCHALLENGE" {
		return false
	}
	fields := make(map[string]string)
	for _, part := range f[2:] {
		pair := strings.SplitN(part, "=", 2)
		if len(pair) != 2 || fields[pair[0]] != "" {
			return false
		}
		fields[pair[0]] = pair[1]
	}
	server, e1 := hex.DecodeString(fields["SERVERNONCE"])
	proof, e2 := hex.DecodeString(fields["SERVERHASH"])
	if e1 != nil || e2 != nil || len(server) != 32 || len(proof) != 32 ||
		!hmac.Equal(proof, cookieMAC(safeCookieServerKey, cookie, nonce, server)) {
		return false
	}
	if !c.write(ctx, "AUTHENTICATE "+hex.EncodeToString(cookieMAC(safeCookieClientKey, cookie, nonce, server))+"\r\n") {
		return false
	}
	line, ok = c.reply()
	return ok && line == "250 OK"
}
