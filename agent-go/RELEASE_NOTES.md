# Agent 0.6.11

- Support named build credentials fetched for the authenticated deployment operation, with private temporary files and cleanup.
- Retain Compose runtime source files by archive identity and preserve them through rollback.
- Reconcile interrupted preparation after a verified host reboot when the operation, local Docker endpoint and retained configuration satisfy the recovery checks. Uncertain outcomes still require reconciliation.
- Build with Go 1.26.6 and updated network dependencies.

Update explicitly after deployment operations finish. The updater verifies the artifact and embedded version, preserves configuration and applications, and restores the prior executable if startup fails. Immediate build interruption and automatic fleet updates are not included.
