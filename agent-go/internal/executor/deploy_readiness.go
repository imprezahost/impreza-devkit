package executor

import sdkclient "github.com/imprezahost/impreza-devkit/sdk-go/client"

// Local to ONE finishReplacement call: no shared mutable observation or replay.
// Installation is the existing result status; only the actual probe supplies HTTPS.
type deployReadinessObservation struct {
	routeFailed bool
	checked     bool
	confirmed   bool
	cancelled   bool
	onionOnly   bool
}

func (o deployReadinessObservation) result(status string) *sdkclient.DeploymentReadiness {
	r := sdkclient.UnknownDeploymentReadiness()
	switch status {
	case "success":
		r.Installation = "COMPLETED"
	case "failed", "timeout", "partial":
		r.Installation = "NOT_COMPLETED"
	default:
		return &r
	}
	r.PublicEndpoint, r.Reason = "NOT_CHECKED", "NO_PROBE"
	if r.Installation == "COMPLETED" {
		switch {
		case o.cancelled:
			r.PublicEndpoint, r.Reason = "UNVERIFIED", "CANCELLED"
		case o.routeFailed:
			r.PublicEndpoint, r.Reason = "REFUSED", "ROUTE_APPLY_FAILED"
		case o.checked && o.confirmed:
			r.PublicEndpoint, r.Reason = "CONFIRMED", "PROBE_PASSED"
		case o.checked:
			r.PublicEndpoint, r.Reason = "UNVERIFIED", "PROBE_NOT_CONFIRMED"
		case o.onionOnly:
			r.PublicEndpoint, r.Reason = "NOT_APPLICABLE", "ONION_ONLY"
		}
	}
	safe := sdkclient.SafeDeploymentReadiness(&r)
	return &safe
}
