// Package packaging embeds the reviewed customer updater in the agent binary.
// Managed updates execute these bytes, never a freshly downloaded shell script.
package packaging

import _ "embed"

//go:embed update.sh
var UpdateScript []byte
