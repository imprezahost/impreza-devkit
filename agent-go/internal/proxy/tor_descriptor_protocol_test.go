package proxy

// Descriptor protocol cases use the owned Unix SAFECOOKIE fixture and synthetic events.
import (
	"bufio"
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestDescriptorV3ShapeThroughCaller(t *testing.T) {
	line := "650 HS_DESC UPLOADED OWN UNKNOWN HSDIR_FIXTURE"
	tor, id, onion := descriptorFixture(t, descriptorScenario{events: line + "\r\n"})
	if !tor.WaitForOnionPublication(context.Background(), id, onion) {
		t.Fatal("real v3 UNKNOWN upload lost")
	}
}

func TestDescriptorProtocolRefusesOtherActionAddressAuthAndPrefix(t *testing.T) {
	addr := strings.Repeat("d", 56)
	for _, line := range []string{
		"650 HS_DESC CREATED " + addr + " UNKNOWN HSDIR_FIXTURE",
		"650 HS_DESC UPLOAD " + addr + " UNKNOWN HSDIR_FIXTURE",
		"650 HS_DESC RECEIVED " + addr + " UNKNOWN HSDIR_FIXTURE",
		"650 HS_DESC UPLOADED " + strings.Repeat("e", 56) + " UNKNOWN HSDIR_FIXTURE",
		"650 HS_DESC UPLOADED " + addr + " BASIC_AUTH HSDIR_FIXTURE",
		"650 HS_DESC UPLOADED " + addr + " STEALTH_AUTH HSDIR_FIXTURE",
		"650 HS_DESC UPLOADED " + addr + " FUTURE HSDIR_FIXTURE",
		"650 HS_DESC UPLOADED " + addr + " UNKNOWN", // missing HSDir field
		"250 HS_DESC UPLOADED " + addr + " UNKNOWN HSDIR_FIXTURE",
	} {
		c := descriptorControl{address: addr, acceptEvents: true}
		c.event(line)
		if c.uploaded {
			t.Fatal("unbound event accepted")
		}
	}
	// Preserve explicit old NO_AUTH wire compatibility without accepting all auth.
	c := descriptorControl{address: addr, acceptEvents: true}
	c.event("650 HS_DESC UPLOADED " + addr + " NO_AUTH HSDIR_FIXTURE")
	if !c.uploaded {
		t.Fatal("legacy explicit auth lost")
	}
}

type descriptorIdleReader struct {
	ticks     int
	remaining string
}

func (r *descriptorIdleReader) Read(b []byte) (int, error) {
	if r.ticks > 0 {
		r.ticks--
		return 0, &net.DNSError{IsTimeout: true}
	}
	if r.remaining == "" {
		return 0, io.EOF
	}
	n := copy(b, r.remaining)
	r.remaining = r.remaining[n:]
	return n, nil
}
func TestDescriptorProtocolEmptyTimeoutDoesNotExhaustLineBudget(t *testing.T) {
	source := &descriptorIdleReader{ticks: descriptorMaxLines + 17, remaining: "650 HS_DESC UPLOADED own UNKNOWN HSDIR_FIXTURE\r\n"}
	c := descriptorControl{reader: bufio.NewReaderSize(source, 4096)}
	for i := 0; i < descriptorMaxLines+17; i++ {
		line, e := c.line()
		timeout, ok := e.(net.Error)
		if line != "" || !ok || !timeout.Timeout() || c.lines != 0 || c.bytes != 0 {
			t.Fatal("idle timeout consumed protocol budget")
		}
	}
	line, e := c.line()
	if e != nil || c.lines != 1 || !strings.HasPrefix(line, "650 HS_DESC UPLOADED") {
		t.Fatal("real line lost after idle", e)
	}
}
func TestDescriptorProtocolPartialTimeoutStillConsumesAndRefuses(t *testing.T) {
	source := io.MultiReader(strings.NewReader("650 HS_DESC"), &descriptorIdleReader{ticks: 1})
	c := descriptorControl{reader: bufio.NewReaderSize(source, 4096)}
	line, e := c.line()
	if e == nil || line == "" || c.lines != 1 || c.bytes != len(line) {
		t.Fatal("partial bytes discarded")
	}
}
func TestDescriptorProtocolLateUploadDoesNotTurnTimeoutIntoSuccess(t *testing.T) {
	release := make(chan struct{})
	tor, id, onion := descriptorFixture(t, descriptorScenario{
		beforeEvent: func(*Tor, string, string) { <-release },
		events:      "650 HS_DESC UPLOADED OWN UNKNOWN HSDIR_FIXTURE\r\n",
	})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	confirmed := tor.WaitForOnionPublication(ctx, id, onion)
	close(release)
	if confirmed || time.Since(started) > time.Second {
		t.Fatal("late upload extended wait or invented success")
	}
	// Public caller hard cap remains30s; no goroutine/history observation added.
	if onionPublicationBudget != 30*time.Second {
		t.Fatal("deploy budget changed")
	}
}

func TestDescriptorProtocolRejectedEventsThroughOwnedTransport(t *testing.T) {
	for _, event := range []string{
		"650 HS_DESC CREATED OWN UNKNOWN HSDIR_FIXTURE\r\n",
		"650 HS_DESC UPLOAD OWN UNKNOWN HSDIR_FIXTURE\r\n",
		"650 HS_DESC UPLOADED " + strings.Repeat("e", 56) + " UNKNOWN HSDIR_FIXTURE\r\n",
		"650 HS_DESC UPLOADED OWN BASIC_AUTH HSDIR_FIXTURE\r\n",
		"650 HS_DESC UPLOADED OWN UNKNOWN\r\n",
	} {
		tor, id, onion := descriptorFixture(t, descriptorScenario{events: event})
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		confirmed := tor.WaitForOnionPublication(ctx, id, onion)
		cancel()
		if confirmed {
			t.Fatal("rejected event confirmed publication through real caller")
		}
	}
}
