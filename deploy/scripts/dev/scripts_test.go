package devscripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestUpDownReset_SmokeWithMockDocker(t *testing.T) {
	root := projectRoot(t)
	logPath := filepath.Join(t.TempDir(), "docker.log")
	mockDocker := filepath.Join(t.TempDir(), "docker")
	mock := "#!/usr/bin/env bash\nset -euo pipefail\necho \"$*\" >> \"" + logPath + "\"\nexit 0\n"
	if err := os.WriteFile(mockDocker, []byte(mock), 0o755); err != nil {
		t.Fatal(err)
	}

	run := func(script string, sandbox bool) {
		t.Helper()
		cmd := exec.Command(filepath.Join(root, "deploy/scripts/dev", script))
		cmd.Env = append(os.Environ(), "DOCKER_BIN="+mockDocker)
		if sandbox {
			cmd.Env = append(cmd.Env, "KRILL_ENABLE_DOCKER_SANDBOX=1")
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s failed: %v\n%s", script, err, string(out))
		}
	}

	run("up.sh", true)
	run("down.sh", true)
	run("reset.sh", true)

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, must := range []string{
		"compose -f " + filepath.Join(root, "deploy/compose/docker-compose.yml"),
		"docker-compose.sandbox.yml --profile docker-sandbox up -d --build",
		"docker-compose.sandbox.yml --profile docker-sandbox down --remove-orphans",
		"docker-compose.sandbox.yml --profile docker-sandbox down -v --remove-orphans",
		"docker-compose.sandbox.yml --profile docker-sandbox ps",
	} {
		if !strings.Contains(got, must) {
			t.Fatalf("expected %q in docker calls, got:\n%s", must, got)
		}
	}
}

func projectRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Clean(filepath.Join(wd, "../../.."))
}
