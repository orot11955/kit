package git

import (
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRemoteBranchHashDoesNotCreateRemoteTrackingRef(t *testing.T) {
	remote := filepath.Join(t.TempDir(), "remote.git")
	gitRun(t, "", "init", "--bare", remote)

	seed := initRepository(t)
	gitRun(t, seed, "remote", "add", "origin", remote)
	gitRun(t, seed, "push", "origin", "develop")
	want := gitOutput(t, seed, "rev-parse", "develop")

	probe := initRepository(t)
	gitRun(t, probe, "remote", "add", "origin", remote)
	if commandHasRef(probe, "refs/remotes/origin/develop") {
		t.Fatal("probe unexpectedly started with a remote-tracking ref")
	}

	hash, exists, err := (Service{Dir: probe}).RemoteBranchHash(context.Background(), "origin", "develop")
	if err != nil {
		t.Fatal(err)
	}
	if !exists || hash != want {
		t.Fatalf("remote branch result: exists=%v hash=%s want=%s", exists, hash, want)
	}
	if commandHasRef(probe, "refs/remotes/origin/develop") {
		t.Fatal("ls-remote diagnostic mutated remote-tracking refs")
	}
}

func TestTraceRunnerRedactsCredentialsAndDoesNotPrintInput(t *testing.T) {
	base := &stubRunner{output: []byte("ok")}
	var output bytes.Buffer
	runner := TraceRunner{Base: base, Writer: &output}
	secretURL := "https://user:password@example.test/repo?access_token=query-secret"
	if _, err := runner.Run(context.Background(), ".", "fetch", secretURL); err != nil {
		t.Fatal(err)
	}
	text := output.String()
	for _, secret := range []string{"user:password", "query-secret"} {
		if strings.Contains(text, secret) {
			t.Fatalf("verbose command leaked %q: %s", secret, text)
		}
	}

	output.Reset()
	input := []byte("private patch body")
	if _, err := runner.RunInput(context.Background(), ".", input, "patch-id", "--stable"); err == nil {
		// stubRunner intentionally rejects RunInput; tracing still must not leak stdin.
	}
	if strings.Contains(output.String(), string(input)) || !strings.Contains(output.String(), "stdin:18 bytes") {
		t.Fatalf("unexpected input trace: %q", output.String())
	}
}

func commandHasRef(dir, ref string) bool {
	cmd := exec.Command("git", "show-ref", "--verify", "--quiet", ref)
	cmd.Dir = dir
	return cmd.Run() == nil
}
