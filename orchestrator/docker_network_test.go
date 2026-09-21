package function

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestDockerNetworkSetupWithoutDocker(t *testing.T) {
	// An empty PATH models a custom image without Docker. No external command
	// should run, and exiting the setup subshell must not exit VM startup.
	cmd := exec.Command("bash", "-c", "set -e\n"+dockerNetworkScript+"\nprintf runner-started")
	cmd.Env = append(os.Environ(), "PATH="+t.TempDir())
	out, err := cmd.CombinedOutput()
	if err != nil || string(out) != "runner-started" {
		t.Fatalf("startup without Docker: %v, %s", err, out)
	}
}

func TestDockerNetworkSetup(t *testing.T) {
	for _, tt := range []struct {
		name, version, mtu, config          string
		validateFail, inspectFail, wantFail bool
		noRoute, restartFail                bool
	}{
		{name: "fresh", version: "28.5.1", mtu: "1460"},
		{name: "merge", version: "27.5.1", mtu: "1500", config: `{"log-driver":"local","default-network-opts":{"bridge":{"other":"keep"},"other-driver":{"key":"value"}}}`},
		{name: "older-engine", version: "26.1.4", mtu: "1460"},
		{name: "jumbo", version: "28.5.1", mtu: "8896"},
		{name: "no-default-route", version: "28.5.1", mtu: "1460", noRoute: true, wantFail: true},
		{name: "invalid-mtu", version: "28.5.1", mtu: "oops", wantFail: true},
		{name: "small-mtu", version: "28.5.1", mtu: "1200", wantFail: true},
		{name: "invalid-version", version: "unknown", mtu: "1460", wantFail: true},
		{name: "invalid-json", version: "28.5.1", mtu: "1460", config: `{`, wantFail: true},
		{name: "non-object", version: "28.5.1", mtu: "1460", config: `[]`, wantFail: true},
		{name: "invalid-config", version: "28.5.1", mtu: "1460", config: `{"unknown":true}`, validateFail: true, wantFail: true},
		{name: "restart-failure", version: "28.5.1", mtu: "1460", restartFail: true, wantFail: true},
		{name: "bridge-mismatch", version: "28.5.1", mtu: "1460", inspectFail: true, wantFail: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			configDir := filepath.Join(dir, "config")
			if err := os.Mkdir(configDir, 0o755); err != nil {
				t.Fatal(err)
			}
			configPath := filepath.Join(configDir, "daemon.json")
			write := func(path, body string) {
				t.Helper()
				if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if tt.config != "" {
				write(configPath, tt.config)
			}
			write(filepath.Join(dir, "mtu"), tt.mtu)
			validate := "exit 0"
			if tt.validateFail {
				validate = "exit 1"
			}
			actual := tt.mtu
			if tt.inspectFail {
				actual = "1500"
			}
			route := `echo '[{"dev":"ens4"}]'`
			if tt.noRoute {
				route = `echo '[]'`
			}
			restart := "echo restart >> '" + filepath.Join(dir, "restarts") + "'"
			if tt.restartFail {
				restart += "\nexit 1"
			}
			for name, body := range map[string]string{
				"ip":        route,
				"dockerd":   "if [ \"$1\" = --version ]; then echo 'Docker version " + tt.version + ", build test'; else " + validate + "; fi",
				"systemctl": restart,
				"docker":    "echo '" + actual + "'",
			} {
				write(filepath.Join(dir, name), "#!/bin/bash\nset -eu\n"+body+"\n")
			}
			script := strings.ReplaceAll(dockerNetworkScript, "/etc/docker", configDir)
			script = strings.ReplaceAll(script, "/sys/class/net/${interface}/mtu", filepath.Join(dir, "mtu"))
			for attempt := 0; attempt < 2; attempt++ {
				cmd := exec.Command("bash", "-c", "set -e\n"+script+"\nprintf runner-started")
				cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
				out, err := cmd.CombinedOutput()
				if (err != nil) != tt.wantFail {
					t.Fatalf("error=%v, wantFail=%v\n%s", err, tt.wantFail, out)
				}
				if strings.Contains(string(out), "runner-started") == tt.wantFail {
					t.Fatalf("runner startup gate incorrect: %s", out)
				}
				if tt.wantFail {
					break
				}
			}
			if tt.wantFail && !tt.inspectFail && !tt.restartFail {
				if _, err := os.Stat(filepath.Join(dir, "restarts")); !os.IsNotExist(err) {
					t.Fatal("Docker restarted after invalid configuration")
				}
				raw, _ := os.ReadFile(configPath)
				if string(raw) != tt.config {
					t.Fatalf("invalid configuration replaced original: %s", raw)
				}
			} else {
				raw, err := os.ReadFile(configPath)
				if err != nil {
					t.Fatal(err)
				}
				var config map[string]any
				if err := json.Unmarshal(raw, &config); err != nil {
					t.Fatal(err)
				}
				wantMTU, _ := strconv.Atoi(tt.mtu)
				if config["mtu"] != float64(wantMTU) {
					t.Fatalf("MTU = %v, want %d", config["mtu"], wantMTU)
				}
				if strings.HasPrefix(tt.version, "26.") {
					if config["default-network-opts"] != nil {
						t.Fatal("unsupported option on old engine")
					}
				} else {
					opts := config["default-network-opts"].(map[string]any)
					bridge := opts["bridge"].(map[string]any)
					if bridge["com.docker.network.driver.mtu"] != tt.mtu {
						t.Fatalf("wrong bridge defaults: %v", bridge)
					}
					if tt.name == "merge" && (config["log-driver"] != "local" || bridge["other"] != "keep" || opts["other-driver"] == nil) {
						t.Fatalf("existing settings lost: %s", raw)
					}
				}
			}
			files, _ := filepath.Glob(filepath.Join(configDir, "daemon.json.*"))
			if len(files) != 0 {
				t.Fatalf("temporary files leaked: %v", files)
			}
		})
	}
}
