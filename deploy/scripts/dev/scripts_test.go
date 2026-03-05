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

	run := func(script string, sandbox bool, pubsub string) {
		t.Helper()
		cmd := exec.Command(filepath.Join(root, "deploy/scripts/dev", script))
		cmd.Env = append(os.Environ(), "DOCKER_BIN="+mockDocker)
		if sandbox {
			cmd.Env = append(cmd.Env, "KRILL_ENABLE_DOCKER_SANDBOX=1")
		}
		if pubsub != "" {
			cmd.Env = append(cmd.Env, "KRILL_PUBSUB_PROFILE="+pubsub)
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s failed: %v\n%s", script, err, string(out))
		}
	}

	run("up.sh", true, "nats")
	run("down.sh", true, "nats")
	run("reset.sh", true, "redis")

	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, must := range []string{
		"compose -f " + filepath.Join(root, "deploy/compose/docker-compose.yml"),
		"docker-compose.sandbox.yml --profile docker-sandbox",
		"docker-compose.pubsub.yml --profile pubsub-nats",
		"docker-compose.pubsub.yml --profile pubsub-redis",
		"up -d --build",
		"down --remove-orphans",
		"down -v --remove-orphans",
		" ps",
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
