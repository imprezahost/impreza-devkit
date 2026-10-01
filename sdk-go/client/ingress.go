package client

// Ingress firewall: the control plane sends the complete desired
// allowlist of one deployment (never a delta); the agent enforces it in its
// own iptables/ip6tables chains and reports the applied revision with the
// live fingerprint. An agent without IngressProtocol never receives it.

// IngressProtocol is the capability an agent advertises when it can enforce
// per-deployment ingress allowlists.
const IngressProtocol = "ingress-firewall-v1"

// CommandIngressUpdate replaces one deployment's ingress allowlist. Rules
// lists only restricted ports; a port absent from Rules is open, and an
// empty Rules removes every restriction of the deployment.
const CommandIngressUpdate CommandKind = "ingress_update"

// IngressUpdatePayload is the full desired state of one deployment.
type IngressUpdatePayload struct {
	DeploymentID string        `json:"deployment_id"`
	Revision     uint32        `json:"revision"`
	Rules        []IngressRule `json:"rules"`
}

// IngressRule restricts one published port to the listed CIDR sources.
type IngressRule struct {
	Port     int      `json:"port"`
	Protocol string   `json:"protocol"` // tcp | udp
	Sources  []string `json:"sources"`
}

// IngressResult is the outcome of an ingress_update, and the per-deployment
// entry of the heartbeat. Enforced is true only when Revision is live in the
// kernel with Fingerprint; Reason is a stable code otherwise.
type IngressResult struct {
	DeploymentID string `json:"deployment_id"`
	Revision     uint32 `json:"revision"`
	Enforced     bool   `json:"enforced"`
	Fingerprint  string `json:"fingerprint,omitempty"`
	Reason       string `json:"reason,omitempty"`
}

// IngressReport is the heartbeat view of every deployment the agent holds an
// allowlist for. It carries revisions and fingerprints only, never sources.
type IngressReport struct {
	Protocol    string          `json:"protocol"`
	Deployments []IngressResult `json:"deployments"`
}
