package fixtures_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readDockerfile returns the repository-root Dockerfile contents.
func readDockerfile(t *testing.T) string {
	t.Helper()
	path := filepath.Join("..", "..", "..", "Dockerfile")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

// workerSSHMounts returns the container-side targets of the worker service's
// SSH file binds (those whose container path ends in id_ed25519/known_hosts).
func workerSSHMounts(t *testing.T) []string {
	t.Helper()
	worker := service(t, readCompose(t), "worker")
	volumes, ok := worker["volumes"].([]any)
	if !ok {
		t.Fatalf("worker volumes = %#v, want list", worker["volumes"])
	}
	var targets []string
	for _, raw := range volumes {
		vol, ok := raw.(string)
		if !ok {
			continue
		}
		// The host portion uses ${VAR:-default} whose own ":" defeats a naive
		// split, so identify the container target as the absolute-path segment
		// that names an SSH credential file.
		for _, part := range strings.Split(vol, ":") {
			if !strings.HasPrefix(part, "/") {
				continue
			}
			if strings.HasSuffix(part, "id_ed25519") || strings.HasSuffix(part, "known_hosts") {
				targets = append(targets, part)
			}
		}
	}
	return targets
}

// TestDockerfileRunsAsNonRoot guards SPEC-adjacent hardening (#365): the
// runtime stage must create an unprivileged user and switch to it via USER so
// a compromised agent command does not execute as root in the container.
func TestDockerfileRunsAsNonRoot(t *testing.T) {
	dockerfile := readDockerfile(t)

	if !strings.Contains(dockerfile, "useradd") {
		t.Error("Dockerfile does not create a dedicated user (no useradd); runtime stage would run as root")
	}

	var hasUserDirective bool
	for _, line := range strings.Split(dockerfile, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "USER" && fields[1] != "root" {
			hasUserDirective = true
			break
		}
	}
	if !hasUserDirective {
		t.Error("Dockerfile has no non-root USER directive; the container runs as root")
	}
}

// TestWorkerSSHMountsUnderNonRootHome guards that the deploy-key binds land in
// the aiops user's home, not /root/.ssh (#365). A /root/.ssh target would be
// unreadable by the non-root process and re-roots the credential under root.
func TestWorkerSSHMountsUnderNonRootHome(t *testing.T) {
	targets := workerSSHMounts(t)
	if len(targets) == 0 {
		t.Fatal("worker service declares no SSH file binds")
	}
	for _, target := range targets {
		if strings.HasPrefix(target, "/root/") {
			t.Errorf("worker SSH bind targets %q under /root; expected the unprivileged user's home", target)
		}
		if !strings.HasPrefix(target, "/home/aiops/.ssh/") {
			t.Errorf("worker SSH bind targets %q; expected /home/aiops/.ssh/...", target)
		}
	}
}

// TestWorkerComposeHardening guards the no-new-privileges + cap_drop hardening
// added alongside the non-root user (#365).
func TestWorkerComposeHardening(t *testing.T) {
	worker := service(t, readCompose(t), "worker")

	secOpt, ok := worker["security_opt"].([]any)
	if !ok {
		t.Fatalf("worker security_opt = %#v, want list", worker["security_opt"])
	}
	var hasNoNewPriv bool
	for _, raw := range secOpt {
		if s, ok := raw.(string); ok && strings.Contains(s, "no-new-privileges:true") {
			hasNoNewPriv = true
		}
	}
	if !hasNoNewPriv {
		t.Error("worker security_opt missing no-new-privileges:true")
	}

	capDrop, ok := worker["cap_drop"].([]any)
	if !ok {
		t.Fatalf("worker cap_drop = %#v, want list", worker["cap_drop"])
	}
	var dropsAll bool
	for _, raw := range capDrop {
		if s, ok := raw.(string); ok && strings.EqualFold(s, "ALL") {
			dropsAll = true
		}
	}
	if !dropsAll {
		t.Error("worker cap_drop does not drop ALL capabilities")
	}
}
