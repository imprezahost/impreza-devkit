package client

// DeploymentReadiness is an additive, finite observation of this deploy command.
// It is neither authorization nor an ongoing health/availability guarantee.
// No hostname, log, HTTP response, identifier or customer value belongs here.
type DeploymentReadiness struct {
	Protocol       string `json:"protocol"`
	Installation   string `json:"installation"`
	PublicEndpoint string `json:"public_endpoint"`
	Reason         string `json:"reason"`
	Scope          string `json:"scope"`
	Diagnostics    string `json:"diagnostics"`
}

// UnknownDeploymentReadiness is the safe projection for absent/legacy data.
func UnknownDeploymentReadiness() DeploymentReadiness {
	return DeploymentReadiness{"F60-READINESS-1", "UNKNOWN", "UNKNOWN", "NO_OBSERVATION", "FIRST_CLEARNET_ROUTE", "NOT_CHECKED"}
}

// SafeDeploymentReadiness accepts only complete, coherent protocol-1 tuples.
// It never copies arbitrary strings from a wire payload into the projection.
func SafeDeploymentReadiness(r *DeploymentReadiness) DeploymentReadiness {
	unknown := UnknownDeploymentReadiness()
	if r == nil || r.Protocol != unknown.Protocol || r.Scope != unknown.Scope || r.Diagnostics != "NOT_CHECKED" {
		return unknown
	}
	switch r.Installation {
	case "COMPLETED", "NOT_COMPLETED", "UNKNOWN":
	default:
		return unknown
	}
	valid := false
	switch r.PublicEndpoint {
	case "CONFIRMED":
		valid = r.Reason == "PROBE_PASSED" && r.Installation == "COMPLETED"
	case "UNVERIFIED":
		valid = (r.Reason == "PROBE_NOT_CONFIRMED" || r.Reason == "CANCELLED") && r.Installation == "COMPLETED"
	case "REFUSED":
		valid = r.Reason == "ROUTE_APPLY_FAILED" && r.Installation == "COMPLETED"
	case "NOT_APPLICABLE":
		valid = r.Reason == "ONION_ONLY" && r.Installation == "COMPLETED"
	case "NOT_CHECKED":
		valid = r.Reason == "NO_PROBE"
	case "UNKNOWN":
		valid = r.Reason == "NO_OBSERVATION"
	}
	if !valid {
		return unknown
	}
	return *r
}
