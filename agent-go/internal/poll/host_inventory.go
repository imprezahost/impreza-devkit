package poll

import (
	"context"
	"errors"
	"github.com/imprezahost/impreza-devkit/agent-go/internal/hostinventory"
	sdk "github.com/imprezahost/impreza-devkit/sdk-go/client"
	"regexp"
	"time"
)

var inventoryID = regexp.MustCompile(`^[a-f0-9]{32}$`)

// A demand rides the heartbeat response; collection and retries run separately.
// Legacy servers do not emit demand and cause no new endpoint requests.
func (p *Poller) inventoryLoop(ctx context.Context) {
	last := ""
	for {
		var request sdk.InventoryRequest
		select {
		case <-ctx.Done():
			return
		case request = <-p.inventoryDemand:
		}
		if !request.Due || !inventoryID.MatchString(request.RequestID) || request.RequestID == last {
			continue
		}
		collectCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		report := hostinventory.Collect(collectCtx)
		cancel()
		for {
			c, cancel := context.WithTimeout(ctx, 15*time.Second)
			e := p.client.Post(c, "/v1/agent/host-inventory/report", map[string]any{"request_id": request.RequestID, "report": report}, nil)
			cancel()
			var conflict *sdk.Conflict
			var invalid *sdk.InvalidRequest
			if e == nil || errors.As(e, &conflict) || errors.As(e, &invalid) {
				last = request.RequestID
				break
			}
			if !sleepCtx(ctx, time.Minute) {
				return
			}
		}
	}
}
