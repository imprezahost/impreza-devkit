package executor

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// Hermetic by the repo's own rule: docker is replaced by a fake runner.
func TestOnionGitHost(t *testing.T) {
	onion := "https://" + strings.Repeat("a", 56) + ".onion/team/app.git"
	if !onionGitHost(onion) {
		t.Fatal("v3 onion URL not detected")
	}
	for _, url := range []string{
		"https://github.com/team/app.git",
		"https://example.com/" + strings.Repeat("a", 56) + ".onion/app.git",
		"https://" + strings.Repeat("a", 55) + ".onion/app.git",
		"https://" + strings.Repeat("a", 56) + ".notonion/app.git",
		"::bad url::",
	} {
		if onionGitHost(url) {
			t.Fatalf("non-onion URL detected as onion: %s", url)
		}
	}
}

func TestStartOnionCloneProxyLifecycle(t *testing.T) {
	restore := onionProxyCmd
	defer func() { onionProxyCmd = restore }()

	var calls [][]string
	var removed []string
	onionProxyCmd = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		joined := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(joined, "run -d"):
			if !strings.Contains(joined, "127.0.0.1::9050") {
				t.Fatal("tor client published off-loopback")
			}
			if !strings.Contains(joined, "cap-drop") {
				t.Fatal("tor client keeps capabilities")
			}
			return []byte("container-started\n"), nil
		case strings.HasPrefix(joined, "port "):
			return []byte("127.0.0.1:49153\n"), nil
		case strings.HasPrefix(joined, "exec ") && strings.Contains(joined, "tor-resolve"):
			return []byte("93.184.216.34\n"), nil
		case strings.HasPrefix(joined, "rm -f "):
			removed = append(removed, strings.TrimPrefix(joined, "rm -f "))
			return nil, nil
		}
		return nil, fmt.Errorf("unexpected command: %s", joined)
	}

	proxy, err := startOnionCloneProxy(context.Background(), "https://"+strings.Repeat("a", 56)+".onion/team/app.git")
	if err != nil {
		t.Fatal(err)
	}
	if proxy == nil || proxy.port != "49153" {
		t.Fatalf("proxy not started correctly: %+v", proxy)
	}
	env := proxy.envEntries()
	for _, want := range []string{"https_proxy=socks5h://127.0.0.1:49153", "HTTPS_PROXY=", "all_proxy=", "ALL_PROXY="} {
		found := false
		for _, e := range env {
			if strings.HasPrefix(e, want) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing %s in %v", want, env)
		}
	}
	proxy.stop()
	if len(removed) != 1 || !strings.HasPrefix(removed[0], "impreza_gittor_") {
		t.Fatalf("client not destroyed: %v", removed)
	}
}

func TestStartOnionCloneProxySkippedForClearnet(t *testing.T) {
	restore := onionProxyCmd
	defer func() { onionProxyCmd = restore }()
	ran := false
	onionProxyCmd = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		ran = true
		return nil, nil
	}
	proxy, err := startOnionCloneProxy(context.Background(), "https://github.com/team/app.git")
	if err != nil || proxy != nil || ran {
		t.Fatalf("clearnet clone must not start a tor client: %v %v %v", proxy, err, ran)
	}
	// stop on a nil proxy must be safe.
	proxy.stop()
}

func TestOnionCloneRejectsUnsafeDestinationsWithoutStartingDocker(t *testing.T) {
	old := onionProxyCmd
	defer func() { onionProxyCmd = old }()
	onionProxyCmd = func(context.Context, string, ...string) ([]byte, error) {
		t.Fatal("unsafe URL started Docker")
		return nil, nil
	}
	onion := strings.Repeat("a", 56) + ".onion"
	for _, raw := range []string{"https://bad.onion/repo", "https://" + onion + "./repo", "ssh://git@" + onion + "/repo", "git@" + onion + ":repo", "https://secret@" + onion + "/repo"} {
		if _, err := startOnionCloneProxy(context.Background(), raw); err == nil {
			t.Fatalf("unsafe onion URL accepted: %s", raw)
		}
	}
	if p, err := startOnionCloneProxy(context.Background(), "git@github.com:team/app.git"); err != nil || p != nil {
		t.Fatal("ordinary SSH clone regressed", err)
	}
}

func TestOnionCloneRejectsNonLoopbackPortAndCleansUncertainStart(t *testing.T) {
	old := onionProxyCmd
	defer func() { onionProxyCmd = old }()
	for _, tc := range []string{"0.0.0.0:9050", "127.0.0.1:70000", "run-error"} {
		removed := false
		onionProxyCmd = func(ctx context.Context, name string, args ...string) ([]byte, error) {
			switch args[0] {
			case "run":
				if tc == "run-error" {
					return nil, fmt.Errorf("uncertain Docker response")
				}
				return []byte("started"), nil
			case "port":
				return []byte(tc), nil
			case "rm":
				removed = true
				return nil, nil
			default:
				t.Fatal("unexpected readiness call")
				return nil, nil
			}
		}
		if _, err := startOnionCloneProxy(context.Background(), "http://"+strings.Repeat("a", 56)+".onion/repo"); err == nil || !removed {
			t.Fatal(tc, "failed to reject and clean")
		}
	}
}

func TestStartOnionCloneProxyRefusesToHangOnBootstrap(t *testing.T) {
	restore := onionProxyCmd
	defer func() { onionProxyCmd = restore }()
	var removed []string
	onionProxyCmd = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.HasPrefix(joined, "run -d"):
			return []byte("container-started\n"), nil
		case strings.HasPrefix(joined, "port "):
			return []byte("127.0.0.1:49153\n"), nil
		case strings.HasPrefix(joined, "exec "):
			return nil, fmt.Errorf("not bootstrapped yet")
		case strings.HasPrefix(joined, "rm -f "):
			removed = append(removed, joined)
			return nil, nil
		}
		return nil, nil
	}
	// A context that expires before the bootstrap deadline.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := startOnionCloneProxy(ctx, "https://"+strings.Repeat("a", 56)+".onion/team/app.git"); err == nil {
		t.Fatal("expired context accepted a tor client")
	}
	if len(removed) != 1 {
		t.Fatalf("failed start must destroy the client, got %v", removed)
	}
}
