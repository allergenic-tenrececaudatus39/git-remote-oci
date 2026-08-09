package test

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// TestAuthenticatedRegistryE2E drives a full push and clone against a registry
// that requires credentials.
//
// Every other end-to-end test runs against an open registry, so the entire
// authentication path - the challenge, the token exchange, and the credential
// resolution order - was only ever exercised by unit tests against a fake. That
// is the path anyone using a hosted registry depends on.
func TestAuthenticatedRegistryE2E(t *testing.T) {
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("Skipping authenticated registry E2E test: Docker is not running")
	}

	const (
		user = "e2e-user"
		pass = "e2e-password"
	)

	binDir := t.TempDir()
	binaryPath := filepath.Join(binDir, "git-remote-oci")
	if out, err := exec.Command("go", "build", "-o", binaryPath, "github.com/mrueg/git-remote-oci").CombinedOutput(); err != nil {
		t.Fatalf("Failed to build git-remote-oci: %v\n%s", err, out)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("OCI_INSECURE", "1")

	// registry:2 reads an htpasswd file and accepts only bcrypt entries.
	hash, err := bcrypt.GenerateFromPassword([]byte(pass), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	authDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(authDir, "htpasswd"), []byte(user+":"+string(hash)+"\n"), 0644); err != nil {
		t.Fatalf("write htpasswd: %v", err)
	}
	// The container runs as a different user, so the file has to be readable
	// from outside the test's own account.
	if err := os.Chmod(authDir, 0755); err != nil {
		t.Fatalf("chmod auth dir: %v", err)
	}

	containerName := fmt.Sprintf("git-remote-oci-auth-e2e-%d", time.Now().UnixNano())
	runOut, err := exec.Command("docker", "run", "-d", "--name", containerName,
		"-p", "0:5000",
		"-v", authDir+":/auth",
		"-e", "REGISTRY_AUTH=htpasswd",
		"-e", "REGISTRY_AUTH_HTPASSWD_REALM=Registry Realm",
		"-e", "REGISTRY_AUTH_HTPASSWD_PATH=/auth/htpasswd",
		"-e", "REGISTRY_STORAGE_DELETE_ENABLED=true",
		"registry:2").CombinedOutput()
	if err != nil {
		t.Fatalf("Failed to start authenticated registry: %v\n%s", err, runOut)
	}
	lines := strings.Split(strings.TrimSpace(string(runOut)), "\n")
	containerID := strings.TrimSpace(lines[len(lines)-1])
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", containerID).Run() })

	portOut, err := exec.Command("docker", "port", containerID, "5000").CombinedOutput()
	if err != nil {
		t.Fatalf("Failed to get container port: %v\n%s", err, portOut)
	}
	_, portStr, err := net.SplitHostPort(strings.TrimSpace(strings.Split(string(portOut), "\n")[0]))
	if err != nil {
		t.Fatalf("Failed to parse port from %q: %v", portOut, err)
	}

	// An authenticated registry answers /v2/ with 401 until credentials are
	// supplied, so readiness is "responding", not "responding 200".
	ready := false
	for range 100 {
		resp, err := http.Get(fmt.Sprintf("http://localhost:%s/v2/", portStr))
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusOK {
				ready = true
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !ready {
		t.Fatalf("Authenticated registry at localhost:%s did not become ready", portStr)
	}

	remoteURL := fmt.Sprintf("oci://localhost:%s/test-org/private-repo", portStr)

	runGit := func(dir string, env []string, args ...string) (string, error) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), env...)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	srcDir := t.TempDir()
	base := []string{
		"GIT_AUTHOR_NAME=E2E", "GIT_AUTHOR_EMAIL=e2e@example.com",
		"GIT_COMMITTER_NAME=E2E", "GIT_COMMITTER_EMAIL=e2e@example.com",
	}
	for _, args := range [][]string{
		{"init", "-q", "-b", "main", "."},
		{"remote", "add", "origin", remoteURL},
	} {
		if out, err := runGit(srcDir, base, args...); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(srcDir, "SECRET.md"), []byte("private content\n"), 0644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	for _, args := range [][]string{{"add", "SECRET.md"}, {"commit", "-q", "-m", "private commit"}} {
		if out, err := runGit(srcDir, base, args...); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	// 1. Without credentials the push must fail, and say what to do about it.
	noCreds := append(append([]string{}, base...), "OCI_USERNAME=", "OCI_PASSWORD=", "DOCKER_CONFIG="+t.TempDir())
	out, err := runGit(srcDir, noCreds, "push", "origin", "main")
	if err == nil {
		t.Fatal("push to an authenticated registry succeeded without credentials")
	}
	if !strings.Contains(out, "anonymously") {
		t.Errorf("unauthenticated push should say the request was anonymous, got:\n%s", out)
	}

	// 2. With the wrong password it must fail naming the source.
	badCreds := append(append([]string{}, base...), "OCI_USERNAME="+user, "OCI_PASSWORD=wrong", "DOCKER_CONFIG="+t.TempDir())
	out, err = runGit(srcDir, badCreds, "push", "origin", "main")
	if err == nil {
		t.Fatal("push succeeded with the wrong password")
	}
	if !strings.Contains(out, "OCI_USERNAME") {
		t.Errorf("failed push should name the credential source, got:\n%s", out)
	}

	// 3. With the right credentials, push and clone must both work.
	goodCreds := append(append([]string{}, base...), "OCI_USERNAME="+user, "OCI_PASSWORD="+pass, "DOCKER_CONFIG="+t.TempDir())
	if out, err := runGit(srcDir, goodCreds, "push", "origin", "main"); err != nil {
		t.Fatalf("authenticated push failed: %v\n%s", err, out)
	}

	cloneParent := t.TempDir()
	if out, err := runGit(cloneParent, goodCreds, "clone", remoteURL, "cloned"); err != nil {
		t.Fatalf("authenticated clone failed: %v\n%s", err, out)
	}
	clonedDir := filepath.Join(cloneParent, "cloned")
	if out, err := runGit(clonedDir, goodCreds, "fsck", "--connectivity-only", "--no-dangling"); err != nil {
		t.Fatalf("cloned repository is not connected: %v\n%s", err, out)
	}
	content, err := os.ReadFile(filepath.Join(clonedDir, "SECRET.md"))
	if err != nil || !strings.Contains(string(content), "private content") {
		t.Errorf("cloned SECRET.md is wrong: %v, %q", err, content)
	}
}
