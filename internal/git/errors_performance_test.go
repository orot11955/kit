package git

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
)

func TestCommandFailurePreservesExitCodeAndRedactsCommand(t *testing.T) {
	cause := exec.Command("sh", "-c", "exit 7").Run()
	err := commandFailure(
		[]string{"ls-remote", "https://user:password@example.test/repo?token=query-secret"},
		"authorization failed for query-secret",
		cause,
	)
	if !IsExitCode(err, 7) {
		t.Fatalf("exit code was not preserved: %v", err)
	}
	message := err.Error()
	for _, secret := range []string{"user:password", "query-secret"} {
		if strings.Contains(message, secret) {
			t.Fatalf("secret %q leaked in %q", secret, message)
		}
	}
}

func TestConfigGetDistinguishesMissingFromGitFailure(t *testing.T) {
	dir := initRepository(t)
	service := Service{Dir: dir}
	if _, err := service.ConfigGet(context.Background(), "git.base"); !errors.Is(err, ErrConfigNotSet) {
		t.Fatalf("missing config was not classified: %v", err)
	}

	failure := &CommandError{Args: []string{"config"}, Message: "permission denied", ExitCode: 2}
	service = Service{Dir: dir, Runner: &stubRunner{err: failure}}
	if _, err := service.ConfigGet(context.Background(), "git.base"); err == nil || errors.Is(err, ErrConfigNotSet) || !IsExitCode(err, 2) {
		t.Fatalf("real Git failure was hidden as missing config: %v", err)
	}
}

func TestUpstreamReturnsEmptyOnlyForActualMissingUpstream(t *testing.T) {
	dir := initRepository(t)
	service := Service{Dir: dir}
	upstream, err := service.Upstream(context.Background())
	if err != nil || upstream != "" {
		t.Fatalf("unexpected upstream result: %q %v", upstream, err)
	}
}

type countingPipelineRunner struct {
	base          ExecRunner
	pipelines     int
	patchShows    int
	patchIDInputs int
}

func (r *countingPipelineRunner) Run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	if len(args) > 0 && args[0] == "show" && containsArg(args, "--patch") {
		r.patchShows++
	}
	return r.base.Run(ctx, dir, args...)
}

func (r *countingPipelineRunner) RunInput(ctx context.Context, dir string, input []byte, args ...string) ([]byte, error) {
	if len(args) > 0 && args[0] == "patch-id" {
		r.patchIDInputs++
	}
	return r.base.RunInput(ctx, dir, input, args...)
}

func (r *countingPipelineRunner) RunPipeline(ctx context.Context, dir string, leftArgs, rightArgs []string) ([]byte, error) {
	r.pipelines++
	return r.base.RunPipeline(ctx, dir, leftArgs, rightArgs)
}

func TestAppliedStreamsBaseAndBatchesCandidatePatchIDs(t *testing.T) {
	dir := initRepository(t)
	gitRun(t, dir, "switch", "-c", "work")
	writeAndCommit(t, dir, "a.txt", "a\n", "add a")
	first := gitOutput(t, dir, "rev-parse", "HEAD")
	writeAndCommit(t, dir, "b.txt", "b\n", "add b")
	gitRun(t, dir, "switch", "develop")
	gitRun(t, dir, "cherry-pick", "--no-commit", first)
	gitRun(t, dir, "commit", "-m", "same patch different commit")

	runner := &countingPipelineRunner{}
	service := Service{Dir: dir, Runner: runner}
	commits, err := service.Candidates(context.Background(), "develop", "work", true)
	if err != nil {
		t.Fatal(err)
	}
	commits, err = service.Applied(context.Background(), "develop", commits)
	if err != nil {
		t.Fatal(err)
	}
	if len(commits) != 2 || !commits[0].Applied || commits[1].Applied {
		t.Fatalf("unexpected applied result: %#v", commits)
	}
	if runner.pipelines != 1 {
		t.Fatalf("base patch history was not streamed exactly once: %d", runner.pipelines)
	}
	if runner.patchShows != 1 {
		t.Fatalf("candidate patches were not batched into one show: %d", runner.patchShows)
	}
	if runner.patchIDInputs != 1 {
		t.Fatalf("candidate patch-id input calls=%d want 1", runner.patchIDInputs)
	}
}

func TestAppliedSkipsPatchScanWhenCherryPickRecordIsEnough(t *testing.T) {
	dir := initRepository(t)
	gitRun(t, dir, "switch", "-c", "work")
	writeAndCommit(t, dir, "a.txt", "a\n", "add a")
	original := gitOutput(t, dir, "rev-parse", "HEAD")
	gitRun(t, dir, "switch", "develop")
	gitRun(t, dir, "cherry-pick", "-x", original)

	runner := &countingPipelineRunner{}
	service := Service{Dir: dir, Runner: runner}
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
	if runner.pipelines != 0 || runner.patchShows != 0 || runner.patchIDInputs != 0 {
		t.Fatalf("patch scan ran despite complete -x evidence: pipeline=%d show=%d input=%d", runner.pipelines, runner.patchShows, runner.patchIDInputs)
	}
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}
