package packaging

import _ "embed"

// The published installer executes these bytes; the release tests and the
// agent-public sync treat this file as the single source.
//
//go:embed install.sh
var InstallScript []byte
