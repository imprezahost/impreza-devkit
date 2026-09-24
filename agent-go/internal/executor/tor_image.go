package executor

// TorDataOwnerProtocol announces an agent whose pinned Tor image restores the
// data directory owner after a tor package upgrade (image 1.0.1 and later).
// Earlier agents cannot start Tor once an upgraded package has reset that
// owner, so the control plane can require this before provisioning onion
// services.
const TorDataOwnerProtocol = "tor-data-owner-v1"
