# Agent 0.6.35 public source snapshot

This source snapshot includes the artifact-admission correction in the updater,
public HTTPS deployment advisories and the additive first-route observation.
Deployment installation and first-route observation are different: neither is a
promise of continuing health or of a compatible API/UI release.

Operator-only maintenance and rehearsal implementation paths present in the
signed executable are not included in this public snapshot. The public updater
preserves the previous public ingress unit definition while applying the same
validation-before-unit-mutation ordering. It does not add an unavailable reload
entry point. These source differences are deliberate, and the embedded updater
bytes differ from the signed executable's source. This snapshot therefore must
not be described as an independently reproduced source of the signed executable.

The release process must record the two architecture comparisons and the exact
source differences before accepting this public projection. No new executable
is published merely by making this source available; signed channel artifacts
remain a separate release. Do not disable signature, expiry, digest, version or
sequence checks to install a local build as an official release. Updates remain
an explicit choice for each server, preserving credentials, configuration and
application containers.
