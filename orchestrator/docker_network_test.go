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

// dockerTestHost substitutes the external host services, not the startup script.
// Its Docker service reads the generated config on restart; a runner can observe
// that state only after setup has actually run. All filesystem writes are local
// to the fixture, including when these tests run on a host with Docker installed.
type dockerTestHost struct {
	bin, configDir, state string
	env                   []string
}

func newDockerTestHost(t *testing.T) dockerTestHost {
	t.Helper()
	root := t.TempDir()
	host := dockerTestHost{bin: filepath.Join(root, "bin"), configDir: filepath.Join(root, "config"), state: filepath.Join(root, "bridge-mtu")}
	for _, dir := range []string{host.bin, host.configDir} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	host.env = append(os.Environ(),
		"PATH="+host.bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GCRUNNER_DOCKER_CONFIG_DIR="+host.configDir,
		"TEST_DOCKER_STATE="+host.state,
		"TEST_HOST_MTU=1460", "TEST_ENGINE_VERSION=29.0.0",
		"TEST_NO_ROUTE=false", "TEST_VALIDATE_FAIL=false", "TEST_RESTART_FAIL=false", "TEST_INSPECT_FAIL=false",
	)
	for name, body := range map[string]string{
		"ip": `case "$*" in
  *route*) if "$TEST_NO_ROUTE"; then echo '[]'; else echo '[{"dev":"ens4"}]'; fi ;;
  *link*) jq -n --arg mtu "$TEST_HOST_MTU" '[{mtu: $mtu}]' ;;
  *) exit 1 ;;
esac`,
		"dockerd": `if [ "$1" = --version ]; then
  echo "Docker version $TEST_ENGINE_VERSION, build fixture"
else
  if "$TEST_VALIDATE_FAIL"; then exit 1; fi
  jq -e 'type == "object"' "${!#}" >/dev/null
fi`,
		"systemctl": `if "$TEST_RESTART_FAIL"; then exit 1; fi
jq -r .mtu "$GCRUNNER_DOCKER_CONFIG_DIR/daemon.json" > "$TEST_DOCKER_STATE"`,
		"docker": `if "$TEST_INSPECT_FAIL"; then echo 1500; else cat "$TEST_DOCKER_STATE"; fi`,
	} {
		if err := os.WriteFile(filepath.Join(host.bin, name), []byte("#!/bin/bash\nset -eu\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return host
}

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
		name, mtu, config, wantNetworkDefault string
		env                                   []string
		wantFail, configInstalled             bool
	}{
		{name: "fresh", mtu: "1460", wantNetworkDefault: "1460", configInstalled: true},
		{name: "merge", mtu: "1500", wantNetworkDefault: "1500", configInstalled: true, config: `{"log-driver":"local","default-network-opts":{"bridge":{"other":"keep"},"other-driver":{"key":"value"}}}`},
		{name: "legacy-engine", mtu: "1460", configInstalled: true, env: []string{"TEST_ENGINE_VERSION=26.1.4"}},
		{name: "jumbo", mtu: "8896", wantNetworkDefault: "8896", configInstalled: true},
		{name: "no-default-route", mtu: "1460", env: []string{"TEST_NO_ROUTE=true"}, wantFail: true},
		{name: "invalid-mtu", mtu: "oops", wantFail: true},
		{name: "small-mtu", mtu: "1200", wantFail: true},
		{name: "invalid-version", mtu: "1460", env: []string{"TEST_ENGINE_VERSION=unknown"}, wantFail: true},
		{name: "invalid-json", mtu: "1460", config: `{`, wantFail: true},
		{name: "non-object", mtu: "1460", config: `[]`, wantFail: true},
		{name: "invalid-config", mtu: "1460", config: `{"unknown":true}`, env: []string{"TEST_VALIDATE_FAIL=true"}, wantFail: true},
		{name: "restart-failure", mtu: "1460", wantNetworkDefault: "1460", configInstalled: true, env: []string{"TEST_RESTART_FAIL=true"}, wantFail: true},
		{name: "bridge-mismatch", mtu: "1460", wantNetworkDefault: "1460", configInstalled: true, env: []string{"TEST_INSPECT_FAIL=true"}, wantFail: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			host := newDockerTestHost(t)
			host.env = append(host.env, "TEST_HOST_MTU="+tt.mtu)
			host.env = append(host.env, tt.env...)
			configPath := filepath.Join(host.configDir, "daemon.json")
			if tt.config != "" {
				// Proxy credentials and other preexisting settings may be root-only.
				if err := os.WriteFile(configPath, []byte(tt.config), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			for attempt := 0; attempt < 2; attempt++ {
				cmd := exec.Command("bash", "-c", "set -e\n"+dockerNetworkScript+"\nprintf runner-started")
				cmd.Env = host.env
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
				state, err := os.ReadFile(host.state)
				if err != nil || strings.TrimSpace(string(state)) != tt.mtu {
					t.Fatalf("running bridge MTU = %q, want %s (error: %v)", state, tt.mtu, err)
				}
			}
			if !tt.configInstalled {
				if _, err := os.Stat(host.state); !os.IsNotExist(err) {
					t.Fatal("Docker restarted after invalid configuration")
				}
				raw, _ := os.ReadFile(configPath)
				if string(raw) != tt.config {
					t.Fatalf("invalid configuration replaced original: %s", raw)
				}
			} else {
				info, err := os.Stat(configPath)
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode().Perm()&0o077 != 0 {
					t.Fatalf("daemon settings exposed to group/other: %o", info.Mode().Perm())
				}
				raw, err := os.ReadFile(configPath)
				if err != nil {
					t.Fatal(err)
				}
				var config struct {
					MTU                int                          `json:"mtu"`
					LogDriver          string                       `json:"log-driver"`
					DefaultNetworkOpts map[string]map[string]string `json:"default-network-opts"`
				}
				if err := json.Unmarshal(raw, &config); err != nil {
					t.Fatal(err)
				}
				wantMTU, _ := strconv.Atoi(tt.mtu)
				if config.MTU != wantMTU {
					t.Fatalf("MTU = %v, want %d", config.MTU, wantMTU)
				}
				bridge := config.DefaultNetworkOpts["bridge"]
				if bridge["com.docker.network.driver.mtu"] != tt.wantNetworkDefault {
					t.Fatalf("new bridge default = %q, want %q", bridge["com.docker.network.driver.mtu"], tt.wantNetworkDefault)
				}
				if tt.config != "" && (config.LogDriver != "local" || bridge["other"] != "keep" || config.DefaultNetworkOpts["other-driver"]["key"] != "value") {
					t.Fatalf("existing settings lost: %s", raw)
				}
			}
			files, _ := filepath.Glob(filepath.Join(host.configDir, "daemon.json.*"))
			if len(files) != 0 {
				t.Fatalf("temporary files leaked: %v", files)
			}
		})
	}
}
