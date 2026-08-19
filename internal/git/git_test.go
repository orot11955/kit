package git

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

type stubRunner struct {
	output []byte
	err    error
	dir    string
}

func (s *stubRunner) Run(_ context.Context, dir string, _ ...string) ([]byte, error) {
	s.dir = dir
	return s.output, s.err
}

func (s *stubRunner) RunInput(context.Context, string, []byte, ...string) ([]byte, error) {
	return nil, errors.New("unexpected RunInput")
}

func TestValidateDependency(t *testing.T) {
	tests := []struct {
		name    string
		output  string
		runErr  error
		wantErr string
	}{
		{name: "minimum", output: "git version 2.34.0\n"},
		{name: "apple suffix", output: "git version 2.39.3 (Apple Git-146)\n"},
		{name: "distribution suffix", output: "git version 2.43.0.windows.1\n"},
		{name: "missing", runErr: errors.New("executable file not found"), wantErr: "not installed"},
		{name: "too old", output: "git version 2.33.9\n", wantErr: "unsupported"},
		{name: "malformed", output: "unknown\n", wantErr: "could not determine"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := &stubRunner{output: []byte(tt.output), err: tt.runErr}
			err := (Service{Runner: runner, Dir: "/path/that/need/not/exist"}).ValidateDependency(context.Background())
			if tt.wantErr == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantErr != "" && (err == nil || !bytes.Contains([]byte(err.Error()), []byte(tt.wantErr))) {
				t.Fatalf("error %q does not contain %q", err, tt.wantErr)
			}
			if runner.dir != "" {
				t.Fatalf("dependency check inherited repository dir %q", runner.dir)
			}
		})
	}
}

func TestGitInstallHintIsActionable(t *testing.T) {
	if hint := gitInstallHint("darwin"); !bytes.Contains([]byte(hint), []byte("xcode-select")) {
		t.Fatalf("macOS hint is not actionable: %s", hint)
	}
	if hint := gitInstallHint("linux"); !bytes.Contains([]byte(hint), []byte("apt install git")) {
		t.Fatalf("Ubuntu hint is not actionable: %s", hint)
	}
}

func TestGitEnvironmentRemovesKitProviderTokens(t *testing.T) {
	got := gitEnvironment([]string{
		"PATH=/usr/bin",
		"KIT_GITLAB_TOKEN=gitlab-secret",
		"KIT_FORGEJO_TOKEN=forgejo-secret",
		"KIT_GITLAB_HOST=gitlab.example",
		"UNRELATED_TOKEN=kept",
	})
	want := []string{"PATH=/usr/bin", "KIT_GITLAB_HOST=gitlab.example", "UNRELATED_TOKEN=kept"}
	if len(got) != len(want) {
		t.Fatalf("unexpected environment: %#v", got)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("environment[%d] = %q, want %q", index, got[index], want[index])
		}
	}
}

func TestRedactGitErrorRemovesURLCredentials(t *testing.T) {
	message := redactGitError("fatal: https://user:password@example.test/repo?access_token=query-secret token=other")
	for _, secret := range []string{"user:password", "query-secret", "other"} {
		if bytes.Contains([]byte(message), []byte(secret)) {
			t.Fatalf("credential %q leaked in %q", secret, message)
		}
	}
}

func TestCandidatesAndAppliedByStablePatchID(t *testing.T) {
	dir := initRepository(t)
	gitRun(t, dir, "switch", "-c", "work")
	writeAndCommit(t, dir, "a.txt", "a\n", "add a")
	first := gitOutput(t, dir, "rev-parse", "HEAD")
	writeAndCommit(t, dir, "b.txt", "b\n", "add b")
	second := gitOutput(t, dir, "rev-parse", "HEAD")

	gitRun(t, dir, "switch", "develop")
	gitRun(t, dir, "cherry-pick", "--no-commit", first)
	gitRun(t, dir, "commit", "-m", "same patch with a different commit") // omit -x; patch-id must detect it

	service := Service{Dir: dir}
	commits, err := service.Candidates(context.Background(), "develop", "work", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(commits) != 2 || commits[0].Hash != first || commits[1].Hash != second {
		t.Fatalf("unexpected oldest-first candidates: %#v", commits)
	}
	commits, err = service.Applied(context.Background(), "develop", commits)
	if err != nil {
		t.Fatal(err)
	}
	if !commits[0].Applied || commits[1].Applied {
		t.Fatalf("unexpected applied status: %#v", commits)
	}
}

func TestAppliedByCherryPickRecord(t *testing.T) {
	dir := initRepository(t)
	gitRun(t, dir, "switch", "-c", "work")
	writeAndCommit(t, dir, "a.txt", "a\n", "add a")
	original := gitOutput(t, dir, "rev-parse", "HEAD")
	gitRun(t, dir, "switch", "develop")
	gitRun(t, dir, "cherry-pick", "-x", original)

	service := Service{Dir: dir}
	commits, err := service.Candidates(context.Background(), "develop", "work", true)
	if err != nil {
		t.Fatal(err)
	}
	commits, err = service.Applied(context.Background(), "develop", commits)
	if err != nil {
		t.Fatal(err)
	}
	if len(commits) != 1 || !commits[0].Applied {
		t.Fatalf("cherry-pick record was not detected: %#v", commits)
	}
}

func initRepository(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitRun(t, dir, "init", "-b", "develop")
	gitRun(t, dir, "config", "user.name", "Kit Test")
	gitRun(t, dir, "config", "user.email", "kit@example.invalid")
	writeAndCommit(t, dir, "root.txt", "root\n", "root")
	return dir
}

func writeAndCommit(t *testing.T, dir, name, content, message string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", name)
	gitRun(t, dir, "commit", "-m", message)
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return string(out[:len(out)-1])
}
