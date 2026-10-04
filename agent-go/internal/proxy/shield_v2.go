package proxy

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// Do not advertise new rendering while the installed proxy is the s1 image.
// The release pins the s2 image; test builds use the same image.
func ShieldV2Available() bool {
	return shieldImageCapability("org.impreza.shield.protocol", "shield-v2")
}

func ShieldControlsAvailable() bool {
	return ShieldV2Available() && shieldImageCapability("org.impreza.shield.controls", "shield-v2-controls")
}

func shieldImageCapability(label, expected string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "image", "inspect", "--format", `{{index .Config.Labels "`+label+`"}}`, Image).Output()
	return err == nil && strings.TrimSpace(string(out)) == expected
}
