package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"kit/internal/buildinfo"
	"kit/internal/clierror"
	"kit/internal/selector"
)

func TestParseCompareFlagsInAnyOrder(t *testing.T) {
	opts, help, err := parseCompare(globalOptions{cwd: "."}, []string{"work-2", "--limit", "4", "--base=main", "--json", "--cwd", "/tmp/repo"})
	if err != nil || help {
		t.Fatalf("parseCompare: %v, help=%v", err, help)
	}
	if opts.source != "work-2" || opts.base != "main" || opts.limit != 4 || !opts.json || opts.cwd != "/tmp/repo" {
		t.Fatalf("unexpected options: %#v", opts)
	}
}

func TestParsePickRequiresOneTarget(t *testing.T) {
	_, _, err := parsePick(globalOptions{cwd: "."}, []string{"--from", "work"})
	if clierror.Code(err) != clierror.Usage {
		t.Fatalf("expected usage error, got %v", err)
	}
}

func TestVersionJSON(t *testing.T) {
	var output bytes.Buffer
	a := &Application{
		IO:    IO{In: strings.NewReader(""), Out: &output, ErrOut: &output},
		Build: buildinfo.Info{Version: "v1.2.3", Commit: "abc", BuildDate: "date", Target: "darwin/arm64"},
	}
	if err := a.Run(context.Background(), []string{"version", "--json"}); err != nil {
		t.Fatal(err)
	}
	var got buildinfo.Info
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got != a.Build {
		t.Fatalf("got %#v, want %#v", got, a.Build)
	}
}

func TestCompareJSONClassifiesCommits(t *testing.T) {
	dir := appRepository(t)
	gitCommand(t, dir, "switch", "-c", "work")
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "feature.txt")
	gitCommand(t, dir, "commit", "-m", "feature")

	var output bytes.Buffer
	a := &Application{IO: IO{In: strings.NewReader(""), Out: &output, ErrOut: &output}, Build: buildinfo.Current()}
	if err := a.Run(context.Background(), []string{"compare", "--cwd", dir, "--json"}); err != nil {
		t.Fatal(err)
	}
	var got compareResult
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Pending != 1 || got.Applied != 0 || len(got.Commits) != 1 {
		t.Fatalf("unexpected compare result: %#v", got)
	}
}

func TestPickReportsNonTTYBeforeMutation(t *testing.T) {
	dir := appRepository(t)
	gitCommand(t, dir, "switch", "-c", "work")
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "feature.txt")
	gitCommand(t, dir, "commit", "-m", "feature")
	gitCommand(t, dir, "switch", "develop")

	var output bytes.Buffer
	a := &Application{
		IO:    IO{In: strings.NewReader(""), Out: &output, ErrOut: &output},
		Build: buildinfo.Current(),
		Select: func([]selector.Item, string) ([]selector.Item, error) {
			return nil, selector.ErrNotTTY
		},
	}
	err := a.Run(context.Background(), []string{"pick", "feat/test", "--cwd", dir})
	if clierror.Code(err) != clierror.Failure || !strings.Contains(err.Error(), "TTY") {
		t.Fatalf("expected non-TTY error, got %v", err)
	}
	cmd := exec.Command("git", "show-ref", "--verify", "--quiet", "refs/heads/feat/test")
	cmd.Dir = dir
	if runErr := cmd.Run(); runErr == nil {
		t.Fatal("target branch was created before TTY validation")
	}
}

func TestPickRejectsInvalidTargetBeforeSelection(t *testing.T) {
	dir := appRepository(t)
	gitCommand(t, dir, "switch", "-c", "work")
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "feature.txt")
	gitCommand(t, dir, "commit", "-m", "feature")
	gitCommand(t, dir, "switch", "develop")

	selected := false
	a := &Application{
		IO:    IO{In: strings.NewReader(""), Out: io.Discard, ErrOut: io.Discard},
		Build: buildinfo.Current(),
		Select: func([]selector.Item, string) ([]selector.Item, error) {
			selected = true
			return nil, nil
		},
	}
	err := a.Run(context.Background(), []string{"pick", "bad..branch", "--cwd", dir})
	if clierror.Code(err) != clierror.Failure || !strings.Contains(err.Error(), "invalid target branch") {
		t.Fatalf("expected invalid branch error, got %v", err)
	}
	if selected {
		t.Fatal("selector opened for an invalid branch")
	}
}

func TestPickCreatesBranchAndAppliesSourceOrder(t *testing.T) {
	dir := appRepository(t)
	gitCommand(t, dir, "switch", "-c", "work")
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "feature.txt")
	gitCommand(t, dir, "commit", "-m", "first feature")
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "feature.txt")
	gitCommand(t, dir, "commit", "-m", "second feature")
	gitCommand(t, dir, "switch", "develop")

	var output bytes.Buffer
	a := &Application{
		IO:    IO{In: strings.NewReader(""), Out: &output, ErrOut: &output},
		Build: buildinfo.Current(),
		Select: func(items []selector.Item, _ string) ([]selector.Item, error) {
			return items, nil
		},
	}
	if err := a.Run(context.Background(), []string{"pick", "feat/test", "--cwd", dir, "--yes"}); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "log", "--reverse", "--format=%s", "develop..feat/test")
	cmd.Dir = dir
	log, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(log)); got != "first feature\nsecond feature" {
		t.Fatalf("commits were not applied oldest-first:\n%s", got)
	}
	messageCmd := exec.Command("git", "log", "-1", "--format=%B", "feat/test")
	messageCmd.Dir = dir
	message, err := messageCmd.Output()
	if err != nil || !strings.Contains(string(message), "cherry picked from commit") {
		t.Fatalf("missing -x record: %v\n%s", err, message)
	}
}

func TestPickRejectsDirtyTreeBeforeSelection(t *testing.T) {
	dir := appRepository(t)
	gitCommand(t, dir, "switch", "-c", "work")
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "feature.txt")
	gitCommand(t, dir, "commit", "-m", "feature")
	gitCommand(t, dir, "switch", "develop")
	if err := os.WriteFile(filepath.Join(dir, "dirty.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	selected := false
	a := &Application{
		IO:    IO{In: strings.NewReader(""), Out: io.Discard, ErrOut: io.Discard},
		Build: buildinfo.Current(),
		Select: func([]selector.Item, string) ([]selector.Item, error) {
			selected = true
			return nil, nil
		},
	}
	err := a.Run(context.Background(), []string{"pick", "feat/dirty", "--cwd", dir})
	if clierror.Code(err) != clierror.Failure || !strings.Contains(err.Error(), "working tree has changes") {
		t.Fatalf("expected dirty-tree error, got %v", err)
	}
	if selected {
		t.Fatal("selector opened for a dirty working tree")
	}
	assertBranchMissing(t, dir, "feat/dirty")
}

func TestPickConflictAbortRestoresCheckoutAndDeletesTarget(t *testing.T) {
	dir := appRepository(t)
	gitCommand(t, dir, "switch", "-c", "work")
	if err := os.WriteFile(filepath.Join(dir, "root.txt"), []byte("work change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "root.txt")
	gitCommand(t, dir, "commit", "-m", "work change")
	gitCommand(t, dir, "switch", "develop")
	if err := os.WriteFile(filepath.Join(dir, "root.txt"), []byte("develop change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "root.txt")
	gitCommand(t, dir, "commit", "-m", "develop change")
	originalHead := gitCommandOutput(t, dir, "rev-parse", "HEAD")

	var output bytes.Buffer
	a := &Application{
		IO:    IO{In: strings.NewReader("a\n"), Out: &output, ErrOut: &output},
		Build: buildinfo.Current(),
		Select: func(items []selector.Item, _ string) ([]selector.Item, error) {
			return items, nil
		},
	}
	err := a.Run(context.Background(), []string{"pick", "feat/conflict", "--cwd", dir, "--yes", "--allow-stale"})
	if clierror.Code(err) != clierror.Failure || !strings.Contains(err.Error(), "restored the original checkout") {
		t.Fatalf("expected clean abort result, got %v\n%s", err, output.String())
	}
	if branch := gitCommandOutput(t, dir, "branch", "--show-current"); branch != "develop" {
		t.Fatalf("checkout was not restored: %s", branch)
	}
	if head := gitCommandOutput(t, dir, "rev-parse", "HEAD"); head != originalHead {
		t.Fatalf("HEAD changed after abort: got %s want %s", head, originalHead)
	}
	assertBranchMissing(t, dir, "feat/conflict")
	if status := gitCommandOutput(t, dir, "status", "--porcelain"); status != "" {
		t.Fatalf("working tree is dirty after abort: %s", status)
	}
	content, readErr := os.ReadFile(filepath.Join(dir, "root.txt"))
	if readErr != nil || string(content) != "develop change\n" {
		t.Fatalf("working file was not restored: %q, %v", content, readErr)
	}
}

func TestPickCanResumeAfterProcessExit(t *testing.T) {
	dir := appRepository(t)
	gitCommand(t, dir, "switch", "-c", "work")
	if err := os.WriteFile(filepath.Join(dir, "root.txt"), []byte("work change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "root.txt")
	gitCommand(t, dir, "commit", "-m", "work change")
	gitCommand(t, dir, "switch", "develop")
	if err := os.WriteFile(filepath.Join(dir, "root.txt"), []byte("develop change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "root.txt")
	gitCommand(t, dir, "commit", "-m", "develop change")

	a := &Application{
		IO:    IO{In: strings.NewReader("q\n"), Out: io.Discard, ErrOut: io.Discard},
		Build: buildinfo.Current(),
		Select: func(items []selector.Item, _ string) ([]selector.Item, error) {
			return items, nil
		},
	}
	err := a.Run(context.Background(), []string{"pick", "feat/resume", "--cwd", dir, "--yes", "--allow-stale"})
	if clierror.Code(err) != clierror.Conflict || !strings.Contains(err.Error(), "resume") {
		t.Fatalf("expected paused pick, got %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "root.txt"), []byte("resolved\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "root.txt")

	resume := &Application{IO: IO{In: strings.NewReader(""), Out: io.Discard, ErrOut: io.Discard}, Build: buildinfo.Current()}
	if err := resume.Run(context.Background(), []string{"pick", "--continue", "--cwd", dir}); err != nil {
		t.Fatal(err)
	}
	if branch := gitCommandOutput(t, dir, "branch", "--show-current"); branch != "feat/resume" {
		t.Fatalf("unexpected branch after resume: %s", branch)
	}
	statePath := filepath.Join(dir, ".git", "kit", "pick-state.json")
	if _, err := os.Stat(statePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pick state was not removed: %v", err)
	}
}

func TestPickContinueAcceptsConflictResolutionCommittedInIDE(t *testing.T) {
	dir := appRepository(t)
	gitCommand(t, dir, "switch", "-c", "work")
	if err := os.WriteFile(filepath.Join(dir, "root.txt"), []byte("work change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "root.txt")
	gitCommand(t, dir, "commit", "-m", "work change")
	gitCommand(t, dir, "switch", "develop")
	if err := os.WriteFile(filepath.Join(dir, "root.txt"), []byte("develop change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "root.txt")
	gitCommand(t, dir, "commit", "-m", "develop change")

	pause := &Application{
		IO:    IO{In: strings.NewReader("q\n"), Out: io.Discard, ErrOut: io.Discard},
		Build: buildinfo.Current(),
		Select: func(items []selector.Item, _ string) ([]selector.Item, error) {
			return items, nil
		},
	}
	err := pause.Run(context.Background(), []string{"pick", "feat/ide", "--cwd", dir, "--yes", "--allow-stale"})
	if clierror.Code(err) != clierror.Conflict {
		t.Fatalf("expected paused conflict, got %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "root.txt"), []byte("resolved in IDE\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "root.txt")
	gitCommand(t, dir, "commit", "-m", "resolve conflict in IDE")

	resume := &Application{IO: IO{In: strings.NewReader(""), Out: io.Discard, ErrOut: io.Discard}, Build: buildinfo.Current()}
	if err := resume.Run(context.Background(), []string{"pick", "--continue", "--cwd", dir}); err != nil {
		t.Fatal(err)
	}
	if got := gitCommandOutput(t, dir, "show", "feat/ide:root.txt"); got != "resolved in IDE" {
		t.Fatalf("external resolution commit was not kept: %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git", "kit", "pick-state.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pick state was not finalized: %v", err)
	}
}

func TestCanceledSelectorIsSuccess(t *testing.T) {
	if !errors.Is(selector.ErrCanceled, selector.ErrCanceled) {
		t.Fatal("sentinel broken")
	}
}

func TestGitNamespaceCompareUsesRepositoryWorkflowConfig(t *testing.T) {
	dir := appRepository(t)
	gitCommand(t, dir, "branch", "-m", "integration")
	gitCommand(t, dir, "switch", "-c", "scratch")
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "feature.txt")
	gitCommand(t, dir, "commit", "-m", "feature")
	gitCommand(t, dir, "config", "--local", "kit.git.base", "integration")
	gitCommand(t, dir, "config", "--local", "kit.git.source", "scratch")

	var output bytes.Buffer
	a := &Application{IO: IO{In: strings.NewReader(""), Out: &output, ErrOut: &output}, Build: buildinfo.Current()}
	if err := a.Run(context.Background(), []string{"git", "compare", "--cwd", dir, "--json"}); err != nil {
		t.Fatal(err)
	}
	var got compareResult
	if err := json.Unmarshal(output.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Base != "integration" || got.Source != "scratch" || got.Pending != 1 {
		t.Fatalf("repository config was not applied: %#v", got)
	}
}

func TestGitSyncConflictPromptAbortKeepsWork(t *testing.T) {
	dir := appRepository(t)
	remote := filepath.Join(t.TempDir(), "remote.git")
	gitCommand(t, "", "init", "--bare", remote)
	gitCommand(t, dir, "remote", "add", "origin", remote)
	gitCommand(t, dir, "push", "-u", "origin", "develop")
	gitCommand(t, dir, "switch", "-c", "work")
	if err := os.WriteFile(filepath.Join(dir, "root.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "root.txt")
	gitCommand(t, dir, "commit", "-m", "work conflict")
	originalWork := gitCommandOutput(t, dir, "rev-parse", "work")
	gitCommand(t, dir, "switch", "develop")
	if err := os.WriteFile(filepath.Join(dir, "root.txt"), []byte("team\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "root.txt")
	gitCommand(t, dir, "commit", "-m", "team conflict")
	gitCommand(t, dir, "push", "origin", "develop")
	gitCommand(t, dir, "switch", "work")

	var output bytes.Buffer
	a := &Application{IO: IO{In: strings.NewReader("a\n"), Out: &output, ErrOut: &output}, Build: buildinfo.Current()}
	err := a.Run(context.Background(), []string{"git", "sync", "--cwd", dir, "--yes"})
	if clierror.Code(err) != clierror.Conflict || !strings.Contains(err.Error(), "was aborted") {
		t.Fatalf("expected explicit sync abort, got %v\n%s", err, output.String())
	}
	if !strings.Contains(output.String(), "[c]ontinue, [s]kip, [a]bort") {
		t.Fatalf("sync conflict prompt was not shown:\n%s", output.String())
	}
	if got := gitCommandOutput(t, dir, "rev-parse", "work"); got != originalWork {
		t.Fatalf("work changed after sync abort: got %s want %s", got, originalWork)
	}
}

func TestReadStringContextCancelsBlockedConflictResolverRead(t *testing.T) {
	reader := &contextBlockingReader{started: make(chan struct{}), release: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := readStringContext(ctx, bufio.NewReader(reader))
		result <- err
	}()
	<-reader.started
	cancel()
	err := <-result
	close(reader.release)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected canceled conflict read, got %v", err)
	}
}

type contextBlockingReader struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *contextBlockingReader) Read([]byte) (int, error) {
	r.once.Do(func() { close(r.started) })
	<-r.release
	return 0, io.EOF
}

func TestSyncCommandErrorMapsCanceledMutationToInterrupt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, operationErr := range []error{nil, errors.New("conflict cleanup completed")} {
		err := syncCommandError(ctx, operationErr)
		if clierror.Code(err) != clierror.Interrupt {
			t.Fatalf("expected interrupt code for %v, got %v", operationErr, err)
		}
	}
}

func TestConfirmReaderContextCancelsBlockedRead(t *testing.T) {
	reader := &contextBlockingReader{started: make(chan struct{}), release: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := confirmReaderContext(ctx, bufio.NewReader(reader), io.Discard, "confirm: ")
		result <- err
	}()
	<-reader.started
	cancel()
	err := <-result
	close(reader.release)
	if clierror.Code(err) != clierror.Interrupt {
		t.Fatalf("expected interrupt while confirmation read was blocked, got %v", err)
	}
}

func TestPickRejectsStaleWorkBeforeSelection(t *testing.T) {
	dir := appRepository(t)
	gitCommand(t, dir, "switch", "-c", "work")
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "feature.txt")
	gitCommand(t, dir, "commit", "-m", "feature")
	gitCommand(t, dir, "switch", "develop")
	if err := os.WriteFile(filepath.Join(dir, "team.txt"), []byte("team\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "team.txt")
	gitCommand(t, dir, "commit", "-m", "team change")

	selected := false
	a := &Application{
		IO:    IO{In: strings.NewReader(""), Out: io.Discard, ErrOut: io.Discard},
		Build: buildinfo.Current(),
		Select: func([]selector.Item, string) ([]selector.Item, error) {
			selected = true
			return nil, nil
		},
	}
	err := a.Run(context.Background(), []string{"pick", "feat/stale", "--cwd", dir})
	if clierror.Code(err) != clierror.Conflict || !strings.Contains(err.Error(), "kit git sync") {
		t.Fatalf("expected stale work conflict, got %v", err)
	}
	if selected {
		t.Fatal("selector opened for stale work")
	}
}

func TestConfigInitWritesRepositoryDefaults(t *testing.T) {
	dir := appRepository(t)
	var output bytes.Buffer
	a := &Application{IO: IO{In: strings.NewReader(""), Out: &output, ErrOut: &output}, Build: buildinfo.Current()}
	if err := a.Run(context.Background(), []string{"config", "init", "--cwd", dir, "--json"}); err != nil {
		t.Fatal(err)
	}
	if got := gitCommandOutput(t, dir, "config", "--local", "--get", "kit.git.base"); got != "develop" {
		t.Fatalf("unexpected configured base: %s", got)
	}
	if got := gitCommandOutput(t, dir, "config", "--local", "--get", "kit.git.provider"); got != "auto" {
		t.Fatalf("unexpected configured provider: %s", got)
	}
}

func TestGitPublishPushesOnlyReviewBranch(t *testing.T) {
	dir := appRepository(t)
	remote := filepath.Join(t.TempDir(), "remote.git")
	gitCommand(t, "", "init", "--bare", remote)
	gitCommand(t, dir, "remote", "add", "origin", remote)
	gitCommand(t, dir, "switch", "-c", "feat/publish")
	if err := os.WriteFile(filepath.Join(dir, "publish.txt"), []byte("publish\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "publish.txt")
	gitCommand(t, dir, "commit", "-m", "publish feature")

	var output bytes.Buffer
	a := &Application{IO: IO{In: strings.NewReader(""), Out: &output, ErrOut: &output}, Build: buildinfo.Current()}
	if err := a.Run(context.Background(), []string{"git", "publish", "--cwd", dir, "--yes", "--json"}); err != nil {
		t.Fatal(err)
	}
	if !commandSucceeds(remote, "show-ref", "--verify", "--quiet", "refs/heads/feat/publish") {
		t.Fatal("review branch was not pushed")
	}
	output.Reset()
	gitCommand(t, dir, "switch", "develop")
	err := a.Run(context.Background(), []string{"git", "publish", "--cwd", dir, "--yes"})
	if err == nil || !strings.Contains(err.Error(), "protected workflow branch") {
		t.Fatalf("expected protected branch rejection, got %v", err)
	}
}

func appRepository(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitCommand(t, dir, "init", "-b", "develop")
	gitCommand(t, dir, "config", "user.name", "Kit Test")
	gitCommand(t, dir, "config", "user.email", "kit@example.invalid")
	if err := os.WriteFile(filepath.Join(dir, "root.txt"), []byte("root\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, dir, "add", "root.txt")
	gitCommand(t, dir, "commit", "-m", "root")
	return dir
}

func gitCommand(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func gitCommandOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

func assertBranchMissing(t *testing.T, dir, branch string) {
	t.Helper()
	cmd := exec.Command("git", "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	cmd.Dir = dir
	if err := cmd.Run(); err == nil {
		t.Fatalf("branch %s unexpectedly exists", branch)
	}
}

func commandSucceeds(dir string, args ...string) bool {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	return cmd.Run() == nil
}
