package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDatabaseAdminErrorOutputNeverReachesLogs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executes the Linux docker command boundary")
	}
	consumer, c := generationFixture()
	_ = consumer
	id := strings.Repeat("1", 64)
	container := bindingProviderContainer{ID: id}
	container.State.Running = true
	container.Config.Labels = map[string]string{"com.docker.compose.project": c.ProviderDeploymentID, "com.docker.compose.service": "postgres"}
	network := bindingProviderNetwork{ID: strings.Repeat("2", 64), Name: c.ProviderDeploymentID + "_default", Driver: "bridge", Labels: map[string]string{"com.docker.compose.project": c.ProviderDeploymentID, "com.docker.compose.network": "default"}, Containers: map[string]json.RawMessage{id: json.RawMessage(`{"IPv4Address":"172.18.0.2/16"}`)}}
	containers, _ := json.Marshal([]bindingProviderContainer{container})
	networks, _ := json.Marshal([]bindingProviderNetwork{network})
	dir := t.TempDir()
	script := "#!/bin/sh\nif [ \"$1\" = network ]; then\n cat <<'JSON'\n" + string(networks) + "\nJSON\n exit 0\nfi\nif [ \"$1\" = inspect ]; then\n if [ \"$2\" = --format ]; then printf 'MARIADB_ROOT_PASSWORD=root-secret\\n'; exit 0; fi\n cat <<'JSON'\n" + string(containers) + "\nJSON\n exit 0\nfi\ncat >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	var logs bytes.Buffer
	d := &Docker{StateDir: dir, Log: slog.New(slog.NewTextHandler(&logs, nil))}
	marker := "private-SQL-verifier-marker"
	if _, err := d.provisionPostgresBindingSQL(context.Background(), c, marker); err == nil || !strings.Contains(err.Error(), "provisioning failed") {
		t.Fatal("PostgreSQL error path not exercised")
	}
	if err := d.runMysqlAdminSQL(context.Background(), id, marker); err == nil || !strings.Contains(err.Error(), "administration failed") {
		t.Fatal("MariaDB error path not exercised")
	}
	if strings.Contains(logs.String(), marker) || strings.Contains(logs.String(), "root-secret") {
		t.Fatal("raw database stderr escaped to agent logs")
	}
}
