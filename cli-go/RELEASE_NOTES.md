# CLI Go 0.2.2

Go SDK retries now respect context cancellation during backoff and safely bound
Retry-After values. This prevents an API timeout from being extended by a retry
wait. Existing authentication and deployment commands are unchanged. Python
packages are unchanged.
