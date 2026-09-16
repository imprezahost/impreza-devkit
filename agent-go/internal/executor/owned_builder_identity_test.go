package executor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func ownedBuilderFixture() (*ownedBuilderIdentity, *PreparationWork, map[string]any) {
	w := &PreparationWork{ID: strings.Repeat("1", 32), CommandID: "cmd_builder", Step: "build", RequestSHA256: strings.Repeat("2", 64)}
	b := &ownedBuilderIdentity{Version: 1, WorkID: w.ID, CommandID: w.CommandID, DeploymentID: "dpl_builder", RequestSHA256: w.RequestSHA256, ContainerID: strings.Repeat("3", 64), ImageID: "sha256:" + strings.Repeat("4", 64), StateVolume: strings.Repeat("5", 64)}
	row := map[string]any{"Id": b.ContainerID, "Name": "/impreza-builder-" + b.WorkID, "Image": b.ImageID,
		"Config":     map[string]any{"User": "1000:1000", "Labels": map[string]string{"impreza.builder.work": b.WorkID, "impreza.builder.command": b.CommandID, "impreza.builder.deployment": b.DeploymentID, "impreza.builder.request": b.RequestSHA256}},
		"HostConfig": map[string]any{"PidsLimit": 512, "Memory": 768 * 1024 * 1024, "NanoCpus": 1000000000, "SecurityOpt": []string{"seccomp=unconfined", "apparmor=impreza-builder-" + b.WorkID}, "NetworkMode": "bridge", "IpcMode": "private", "RestartPolicy": map[string]string{"Name": "no"}},
		"Mounts":     []map[string]string{{"Type": "volume", "Name": b.StateVolume, "Destination": "/home/user/.local/share/buildkit"}}}
	return b, w, row
}

func TestOwnedBuilderRejectsForeignOrChangedBoundary(t *testing.T) {
	cases := map[string]func(map[string]any){
		"root user":        func(r map[string]any) { r["Config"].(map[string]any)["User"] = "0" },
		"privileged":       func(r map[string]any) { r["HostConfig"].(map[string]any)["Privileged"] = true },
		"added capability": func(r map[string]any) { r["HostConfig"].(map[string]any)["CapAdd"] = []string{"SYS_ADMIN"} },
		"host device": func(r map[string]any) {
			r["HostConfig"].(map[string]any)["Devices"] = []map[string]string{{"PathOnHost": "/dev/sda"}}
		},
		"device request": func(r map[string]any) {
			r["HostConfig"].(map[string]any)["DeviceRequests"] = []map[string]string{{"Driver": "nvidia"}}
		},
		"unbounded pids":   func(r map[string]any) { r["HostConfig"].(map[string]any)["PidsLimit"] = -1 },
		"unbounded memory": func(r map[string]any) { r["HostConfig"].(map[string]any)["Memory"] = 0 },
		"unbounded cpu":    func(r map[string]any) { r["HostConfig"].(map[string]any)["NanoCpus"] = 0 },
		"unscoped apparmor": func(r map[string]any) {
			r["HostConfig"].(map[string]any)["SecurityOpt"] = []string{"apparmor=unconfined", "seccomp=unconfined"}
		},
		"missing security options": func(r map[string]any) { delete(r["HostConfig"].(map[string]any), "SecurityOpt") },
		"foreign id":               func(r map[string]any) { r["Id"] = strings.Repeat("6", 64) },
		"different name":           func(r map[string]any) { r["Name"] = "/other" },
		"changed image":            func(r map[string]any) { r["Image"] = "sha256:" + strings.Repeat("7", 64) },
		"missing config":           func(r map[string]any) { delete(r, "Config") },
		"wrong owner": func(r map[string]any) {
			r["Config"].(map[string]any)["Labels"].(map[string]string)["impreza.builder.command"] = "cmd_foreign"
		},
		"host mount": func(r map[string]any) { r["HostConfig"].(map[string]any)["Binds"] = []string{"/:/host"} },
		"extra mount": func(r map[string]any) {
			r["HostConfig"].(map[string]any)["Mounts"] = []map[string]string{{"Type": "bind"}}
		},
		"host network":   func(r map[string]any) { r["HostConfig"].(map[string]any)["NetworkMode"] = "host" },
		"host pid":       func(r map[string]any) { r["HostConfig"].(map[string]any)["PidMode"] = "host" },
		"host ipc":       func(r map[string]any) { r["HostConfig"].(map[string]any)["IpcMode"] = "host" },
		"published port": func(r map[string]any) { r["HostConfig"].(map[string]any)["PublishAllPorts"] = true },
		"restart": func(r map[string]any) {
			r["HostConfig"].(map[string]any)["RestartPolicy"] = map[string]string{"Name": "always"}
		},
		"foreign volume": func(r map[string]any) { r["Mounts"].([]map[string]string)[0]["Name"] = strings.Repeat("8", 64) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			b, w, row := ownedBuilderFixture()
			mutate(row)
			raw, _ := json.Marshal([]any{row})
			calls := 0
			err := removeOwnedBuilder(context.Background(), b, w, b.DeploymentID, func(_ context.Context, args ...string) ([]byte, error) {
				calls++
				if calls != 1 {
					t.Fatal("mutation reached after failed identity check")
				}
				return raw, nil
			})
			if err == nil || calls != 1 {
				t.Fatal("changed boundary accepted", err, calls)
			}
		})
	}
}

func TestOwnedBuilderRemovalRequiresVerifiedAbsence(t *testing.T) {
	for _, scenario := range []string{"success", "inspect error", "remove error", "inventory error", "still present", "malformed inventory", "foreign operation"} {
		t.Run(scenario, func(t *testing.T) {
			b, w, row := ownedBuilderFixture()
			raw, _ := json.Marshal([]any{row})
			calls := 0
			if scenario == "foreign operation" {
				w.CommandID = "cmd_other"
			}
			err := removeOwnedBuilder(context.Background(), b, w, b.DeploymentID, func(_ context.Context, args ...string) ([]byte, error) {
				calls++
				switch calls {
				case 1:
					if scenario == "inspect error" {
						return nil, errors.New("transport")
					}
					return raw, nil
				case 2:
					if strings.Join(args, " ") != "container rm --force --volumes "+b.ContainerID {
						t.Fatal(args)
					}
					if scenario == "remove error" {
						return nil, errors.New("lost response")
					}
					return nil, nil
				case 3:
					if scenario == "inventory error" {
						return nil, errors.New("transport")
					}
					if scenario == "still present" {
						return []byte(b.ContainerID), nil
					}
					if scenario == "malformed inventory" {
						return []byte("permission denied"), nil
					}
					return []byte(strings.Repeat("9", 64)), nil
				}
				t.Fatal("unexpected call")
				return nil, nil
			})
			if (err == nil) != (scenario == "success") {
				t.Fatal(scenario, err)
			}
			if scenario == "foreign operation" && calls != 0 {
				t.Fatal("foreign operation reached Docker")
			}
		})
	}
}
