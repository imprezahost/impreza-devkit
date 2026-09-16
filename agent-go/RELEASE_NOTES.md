# Agent 0.6.12

Adds optional controlled builds on Ubuntu 24.04 amd64: explicit administrative
preparation, checksum-pinned executor delivery, verified build cancellation and
recovery after worker loss or host reboot without replay. Invalid or missing
identity evidence still requires support. Legacy builds retain checkpoint
cancellation; replacement, data rollback and automatic fleet upgrades are unchanged.

After active deployments finish, update explicitly and follow the
[controlled build guide](https://docs.imprezahost.com/deployment-cancellation.html#controlled-builds).
The update preserves identity, configuration and applications. Activation is a
separate administrator action. Trusted project code is required; this is not an
egress sandbox or a fixed-time termination guarantee.
