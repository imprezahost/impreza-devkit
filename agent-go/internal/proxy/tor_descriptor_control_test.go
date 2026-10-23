package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

type descriptorScenario struct {
	badServerProof bool
	beforeEvent    func(*Tor, string, string)
	events         string
	ready          chan struct{}
}

// Real Unix control transport and SAFECOOKIE exchange, no daemon/process seam.
// Short owned path avoids the Unix socket's pathname length limit.
func descriptorFixture(t *testing.T, scenario descriptorScenario) (*Tor, string, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Linux Unix control socket fixture")
	}
	root, err := os.MkdirTemp("", "tor-desc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	tor := &Tor{StateDir: root}
	id, onion := "dpl_descriptor", strings.Repeat("d", 56)+".onion"
	data := filepath.Join(root, "data")
	dir := filepath.Join(root, "services", id)
	for _, path := range []string{data, dir} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "hostname"), []byte(onion+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cookie := bytes.Repeat([]byte{0x42}, 32) // Synthetic fixture, not a credential.
	if err := os.WriteFile(filepath.Join(data, "control_auth_cookie"), cookie, 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(data, "descriptor-control.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	t.Cleanup(func() {
		_ = listener.Close()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(time.Second):
			t.Error("control fixture did not reach EOF")
		}
	})
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
		reader := bufio.NewReader(conn)
		fail := func() { done <- fmt.Errorf("SAFECOOKIE fixture protocol refused") }
		line, err := reader.ReadString('\n')
		f := strings.Fields(line)
		if err != nil || len(f) != 3 || f[0] != "AUTHCHALLENGE" || f[1] != "SAFECOOKIE" {
			fail()
			return
		}
		client, err := hex.DecodeString(f[2])
		if err != nil || len(client) != 32 {
			fail()
			return
		}
		server := bytes.Repeat([]byte{0x24}, 32)
		// Independently form the spec MAC input, not the product cookieMAC helper.
		input := append(append(append([]byte{}, cookie...), client...), server...)
		mac := hmac.New(sha256.New, []byte("Tor safe cookie authentication server-to-controller hash"))
		_, _ = mac.Write(input)
		proof := mac.Sum(nil)
		if scenario.badServerProof {
			proof[0] ^= 0xff
		}
		_, _ = fmt.Fprintf(conn, "250 AUTHCHALLENGE SERVERHASH=%x SERVERNONCE=%x\r\n", proof, server)
		line, err = reader.ReadString('\n')
		if scenario.badServerProof {
			if err == nil || line != "" {
				fail()
				return
			}
			done <- nil
			return
		}
		f = strings.Fields(line)
		mac = hmac.New(sha256.New, []byte("Tor safe cookie authentication controller-to-server hash"))
		_, _ = mac.Write(input)
		if err != nil || len(f) != 2 || f[0] != "AUTHENTICATE" || f[1] != hex.EncodeToString(mac.Sum(nil)) {
			fail()
			return
		}
		_, _ = io.WriteString(conn, "250 OK\r\n")
		line, err = reader.ReadString('\n')
		if err != nil || line != "SETEVENTS HS_DESC\r\n" {
			fail()
			return
		}
		_, _ = io.WriteString(conn, "250 OK\r\n")
		if scenario.ready != nil {
			close(scenario.ready)
		}
		if scenario.beforeEvent != nil {
			scenario.beforeEvent(tor, id, onion)
		}
		if scenario.events != "" {
			_, _ = io.WriteString(conn, strings.ReplaceAll(scenario.events, " OWN ", " "+strings.TrimSuffix(onion, ".onion")+" "))
		} else {
			_, _ = io.Copy(io.Discard, conn) // EOF when the owned context cancels.
		}
		done <- nil
	}()
	return tor, id, onion
}

func TestDescriptorUploadUsesActualSafeCookieTransport(t *testing.T) {
	tor, id, onion := descriptorFixture(t, descriptorScenario{events: "650 HS_DESC CREATED OWN UNKNOWN HSDIR_FIXTURE\r\n" +
		"650 HS_DESC UPLOADED " + strings.Repeat("e", 56) + " UNKNOWN HSDIR_FIXTURE\r\n" +
		"650 HS_DESC RECEIVED OWN UNKNOWN HSDIR_FIXTURE\r\n" +
		"650 HS_DESC UPLOADED OWN UNKNOWN HSDIR_FIXTURE REPLICA=0 EXTRA=ignored\r\n"})
	t.Setenv("PATH", t.TempDir()) // Empty of Docker/curl/nc/bash: no image client needed.
	if !tor.WaitForOnionPublication(context.Background(), id, onion) {
		t.Fatal("own descriptor upload not observed")
	}
}

func TestDescriptorCreatedIsNotUpload(t *testing.T) {
	tor, id, onion := descriptorFixture(t, descriptorScenario{events: "650 HS_DESC CREATED OWN UNKNOWN HSDIR_FIXTURE\r\n"})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if tor.WaitForOnionPublication(ctx, id, onion) {
		t.Fatal("created event confirmed upload")
	}
}

func TestDescriptorRejectsForgedServerBeforeClientAuthentication(t *testing.T) {
	tor, id, onion := descriptorFixture(t, descriptorScenario{badServerProof: true})
	if tor.WaitForOnionPublication(context.Background(), id, onion) {
		t.Fatal("forged server authenticated")
	}
}

func TestDescriptorCancelledObservationClosesSocket(t *testing.T) {
	ready := make(chan struct{})
	tor, id, onion := descriptorFixture(t, descriptorScenario{ready: ready})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go func() {
		select {
		case <-ready:
			cancel()
		case <-ctx.Done():
		}
	}()
	if tor.WaitForOnionPublication(ctx, id, onion) {
		t.Fatal("cancellation confirmed upload")
	}
}

func TestDescriptorRejectsRotationAndSameAddressInodeReplacement(t *testing.T) {
	for _, sameAddress := range []bool{false, true} {
		mutation := make(chan error, 1)
		scenario := descriptorScenario{events: "650 HS_DESC UPLOADED OWN UNKNOWN HSDIR_FIXTURE\r\n"}
		scenario.beforeEvent = func(tor *Tor, id, onion string) {
			path := filepath.Join(tor.StateDir, "services", id, "hostname")
			address := strings.Repeat("z", 56) + ".onion"
			if sameAddress {
				address = onion
			}
			err := os.WriteFile(path+".next", []byte(address+"\n"), 0600)
			if err == nil {
				err = os.Rename(path+".next", path)
			}
			mutation <- err
		}
		tor, id, onion := descriptorFixture(t, scenario)
		if tor.WaitForOnionPublication(context.Background(), id, onion) {
			t.Fatal("rotated hostname confirmed")
		}
		select {
		case err := <-mutation:
			if err != nil {
				t.Fatal("fixture rotation failed")
			}
		case <-time.After(time.Second):
			t.Fatal("fixture did not rotate")
		}
	}
}

func TestDescriptorRejectsOversizedControlLine(t *testing.T) {
	tor, id, onion := descriptorFixture(t, descriptorScenario{events: "650 HS_DESC " + strings.Repeat("x", 5000) + "\r\n"})
	if tor.WaitForOnionPublication(context.Background(), id, onion) {
		t.Fatal("oversized control reply confirmed")
	}
}

func TestDescriptorRejectsCookieAndSocketReplacement(t *testing.T) {
	for _, target := range []string{"cookie", "socket"} {
		mutation := make(chan error, 1)
		scenario := descriptorScenario{events: "650 HS_DESC UPLOADED OWN UNKNOWN HSDIR_FIXTURE\r\n"}
		scenario.beforeEvent = func(tor *Tor, _, _ string) {
			data := filepath.Join(tor.StateDir, "data")
			var err error
			if target == "cookie" {
				path := filepath.Join(data, "control_auth_cookie")
				err = os.WriteFile(path+".next", bytes.Repeat([]byte{0x42}, 32), 0600)
				if err == nil {
					err = os.Rename(path+".next", path)
				}
			} else {
				path := filepath.Join(data, "descriptor-control.sock")
				err = os.Rename(path, path+".replaced")
			}
			mutation <- err
		}
		tor, id, onion := descriptorFixture(t, scenario)
		if tor.WaitForOnionPublication(context.Background(), id, onion) {
			t.Fatal("changed controller identity confirmed")
		}
		select {
		case err := <-mutation:
			if err != nil {
				t.Fatal("fixture endpoint change failed")
			}
		case <-time.After(time.Second):
			t.Fatal("fixture did not change endpoint")
		}
	}
}

func TestDescriptorProtocolTotalBudgets(t *testing.T) {
	for _, line := range []string{"650 EXTRA\r\n", "650 EXTRA " + strings.Repeat("x", 300) + "\r\n"} {
		c := descriptorControl{reader: bufio.NewReaderSize(strings.NewReader(strings.Repeat(line, descriptorMaxLines+1)), 4096)}
		refused := false
		for i := 0; i <= descriptorMaxLines; i++ {
			if _, err := c.line(); err != nil {
				refused = true
				break
			}
		}
		if !refused {
			t.Fatal("unbounded descriptor input accepted")
		}
	}
}

func TestDescriptorCookieRefusesUnsafeFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cookie")
	for _, size := range []int{0, 31, 33} {
		if err := os.WriteFile(path, make([]byte, size), 0600); err != nil {
			t.Fatal(err)
		}
		if _, _, ok := descriptorCookie(path); ok {
			t.Fatal("wrong cookie size accepted")
		}
	}
	if err := os.WriteFile(path, make([]byte, 32), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := descriptorCookie(path); ok {
		t.Fatal("readable cookie accepted")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(path, path+".alias"); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := descriptorCookie(path); ok {
		t.Fatal("hardlinked cookie accepted")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path+".alias", path); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := descriptorCookie(path); ok {
		t.Fatal("symlink cookie accepted")
	}
}
