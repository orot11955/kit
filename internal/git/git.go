package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

type Runner interface {
	Run(ctx context.Context, dir string, args ...string) ([]byte, error)
	RunInput(ctx context.Context, dir string, input []byte, args ...string) ([]byte, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	return run(ctx, dir, nil, args...)
}

func (ExecRunner) RunInput(ctx context.Context, dir string, input []byte, args ...string) ([]byte, error) {
	return run(ctx, dir, input, args...)
}

func run(ctx context.Context, dir string, input []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = gitEnvironment(os.Environ())
	if input != nil {
		cmd.Stdin = bytes.NewReader(input)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), commandFailure(args, strings.TrimSpace(stderr.String()), err)
	}
	return stdout.Bytes(), nil
}

func gitEnvironment(environment []string) []string {
	filtered := make([]string, 0, len(environment))
	for _, entry := range environment {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "KIT_") && strings.HasSuffix(name, "_TOKEN") {
			continue
		}
		filtered = append(filtered, entry)
	}
	return filtered
}

type Service struct {
	Runner Runner
	Dir    string
}

type Commit struct {
	Hash      string `json:"hash"`
	ShortHash string `json:"short_hash"`
	Date      string `json:"date"`
	Subject   string `json:"message"`
	Applied   bool   `json:"applied"`
}

const MinimumVersion = "2.34.0"

var gitVersionPattern = regexp.MustCompile(`(?i)^git version ([0-9]+)\.([0-9]+)(?:\.([0-9]+))?`)
var gitURLUserPattern = regexp.MustCompile(`(?i)(https?://)[^/@[:space:]]+@`)
var gitQuerySecretPattern = regexp.MustCompile(`(?i)((?:access_token|private_token|password|token)=)[^&[:space:]]+`)

func redactGitError(message string) string {
	message = gitURLUserPattern.ReplaceAllString(message, `${1}<redacted>@`)
	message = gitQuerySecretPattern.ReplaceAllString(message, `${1}<redacted>`)
	for _, entry := range os.Environ() {
		name, value, found := strings.Cut(entry, "=")
		if found && value != "" && strings.HasPrefix(name, "KIT_") && strings.HasSuffix(name, "_TOKEN") {
			message = strings.ReplaceAll(message, value, "<redacted>")
		}
	}
	return message
}

func (s Service) runner() Runner {
	if s.Runner == nil {
		return ExecRunner{}
	}
	return s.Runner
}

func (s Service) ValidateDependency(ctx context.Context) error {
	// Dependency detection must not inherit --cwd: a missing or invalid repository
	// path is separate from whether Git itself is installed.
	out, err := s.runner().Run(ctx, "", "--version")
	if err != nil {
		return fmt.Errorf("Git is not installed or is not available in PATH; %s: %w", gitInstallHint(runtime.GOOS), err)
	}
	version, err := parseGitVersion(strings.TrimSpace(string(out)))
	if err != nil {
		return fmt.Errorf("could not determine the installed Git version from %q; install Git %s or newer", strings.TrimSpace(string(out)), MinimumVersion)
	}
	minimum := [3]int{2, 34, 0}
	if compareVersion(version, minimum) < 0 {
		return fmt.Errorf("Git %d.%d.%d is unsupported; kit requires Git %s or newer; %s", version[0], version[1], version[2], MinimumVersion, gitInstallHint(runtime.GOOS))
	}
	return nil
}

func parseGitVersion(output string) ([3]int, error) {
	match := gitVersionPattern.FindStringSubmatch(output)
	if match == nil {
		return [3]int{}, errors.New("unrecognized git version output")
	}
	var version [3]int
	for i := range version {
		if i+1 >= len(match) || match[i+1] == "" {
			continue
		}
		value, err := strconv.Atoi(match[i+1])
		if err != nil {
			return [3]int{}, err
		}
		version[i] = value
	}
	return version, nil
}

func compareVersion(left, right [3]int) int {
	for i := range left {
		if left[i] < right[i] {
			return -1
		}
		if left[i] > right[i] {
			return 1
		}
	}
	return 0
}

func gitInstallHint(goos string) string {
	switch goos {
	case "darwin":
		return "install it with 'xcode-select --install' or 'brew install git'"
	case "linux":
		return "on Ubuntu, install it with 'sudo apt update && sudo apt install git'"
	default:
		return "install Git from https://git-scm.com/downloads"
	}
}

func (s Service) run(ctx context.Context, args ...string) ([]byte, error) {
	return s.runner().Run(ctx, s.Dir, args...)
}

func (s Service) ValidateRepository(ctx context.Context) error {
	out, err := s.run(ctx, "rev-parse", "--is-inside-work-tree")
	if err != nil || strings.TrimSpace(string(out)) != "true" {
		return errors.New("not inside a Git repository")
	}
	return nil
}

func (s Service) VerifyRevision(ctx context.Context, revision string) error {
	if revision == "" || strings.HasPrefix(revision, "-") {
		return fmt.Errorf("invalid revision %q", revision)
	}
	_, err := s.run(ctx, "rev-parse", "--verify", revision+"^{commit}")
	return err
}

func (s Service) ValidateBranchName(ctx context.Context, branch string) error {
	if branch == "" || strings.HasPrefix(branch, "-") {
		return fmt.Errorf("invalid branch name %q", branch)
	}
	_, err := s.run(ctx, "check-ref-format", "--branch", branch)
	return err
}

func (s Service) LocalBranchExists(ctx context.Context, branch string) (bool, error) {
	_, err := s.run(ctx, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	if err == nil {
		return true, nil
	}
	if IsExitCode(err, 1) {
		return false, nil
	}
	return false, err
}

func (s Service) IsClean(ctx context.Context) (bool, error) {
	out, err := s.run(ctx, "status", "--porcelain")
	return len(bytes.TrimSpace(out)) == 0, err
}

func (s Service) Head(ctx context.Context) (string, string, error) {
	hashOut, err := s.run(ctx, "rev-parse", "HEAD")
	if err != nil {
		return "", "", err
	}
	branchOut, err := s.run(ctx, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		if IsExitCode(err, 1) {
			return strings.TrimSpace(string(hashOut)), "", nil
		}
		return "", "", err
	}
	return strings.TrimSpace(string(hashOut)), strings.TrimSpace(string(branchOut)), nil
}

func (s Service) Candidates(ctx context.Context, base, source string, oldestFirst bool) ([]Commit, error) {
	out, err := s.run(ctx, "rev-list", "--topo-order", "--first-parent", "--right-only", "--no-merges", base+"..."+source)
	if err != nil {
		return nil, err
	}
	hashes := strings.Fields(string(out))
	if oldestFirst {
		slices.Reverse(hashes)
	}
	commits := make([]Commit, 0, len(hashes))
	for _, hash := range hashes {
		commit, err := s.Commit(ctx, hash)
		if err != nil {
			return nil, err
		}
		commits = append(commits, commit)
	}
	return commits, nil
}

func (s Service) Commit(ctx context.Context, revision string) (Commit, error) {
	out, err := s.run(ctx, "show", "-s", "--format=%H%x00%h%x00%ct%x00%s", revision)
	if err != nil {
		return Commit{}, err
	}
	parts := strings.SplitN(strings.TrimSuffix(string(out), "\n"), "\x00", 4)
	if len(parts) != 4 {
		return Commit{}, fmt.Errorf("unexpected commit metadata for %s", revision)
	}
	seconds, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return Commit{}, fmt.Errorf("parse commit date for %s: %w", revision, err)
	}
	return Commit{
		Hash:      parts[0],
		ShortHash: parts[1],
		Date:      time.Unix(seconds, 0).Local().Format("2006-01-02 15:04"),
		Subject:   parts[3],
	}, nil
}

var pickedPattern = regexp.MustCompile(`(?m)^\(cherry picked from commit ([0-9a-fA-F]{40})\)$`)

func (s Service) Applied(ctx context.Context, base string, candidates []Commit) ([]Commit, error) {
	result := make([]Commit, len(candidates))
	copy(result, candidates)
	if len(result) == 0 {
		return result, nil
	}

	picked, err := s.pickedHashes(ctx, base)
	if err != nil {
		return nil, err
	}
	remaining := make([]Commit, 0, len(result))
	for i := range result {
		if _, ok := picked[strings.ToLower(result[i].Hash)]; ok {
			result[i].Applied = true
			continue
		}
		remaining = append(remaining, result[i])
	}
	if len(remaining) == 0 {
		return result, nil
	}

	basePatches, err := s.basePatchIDs(ctx, base)
	if err != nil {
		return nil, err
	}
	candidatePatches, err := s.candidatePatchIDs(ctx, remaining)
	if err != nil {
		return nil, err
	}
	for i := range result {
		if result[i].Applied {
			continue
		}
		patch := candidatePatches[strings.ToLower(result[i].Hash)]
		if patch == "" {
			continue
		}
		_, result[i].Applied = basePatches[patch]
	}
	return result, nil
}

func (s Service) basePatchIDs(ctx context.Context, base string) (map[string]struct{}, error) {
	out, err := s.runPipeline(ctx,
		[]string{"log", base, "--no-merges", "-p", "--pretty=format:%H"},
		[]string{"patch-id", "--stable"},
	)
	if err != nil {
		return nil, err
	}
	ids := make(map[string]struct{})
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 {
			ids[fields[0]] = struct{}{}
		}
	}
	return ids, nil
}

func (s Service) candidatePatchIDs(ctx context.Context, candidates []Commit) (map[string]string, error) {
	result := make(map[string]string, len(candidates))
	if len(candidates) == 0 {
		return result, nil
	}
	args := []string{"show", "--format=%H", "--patch"}
	for _, commit := range candidates {
		args = append(args, commit.Hash)
	}
	show, err := s.run(ctx, args...)
	if err != nil {
		return nil, err
	}
	out, err := s.runner().RunInput(ctx, s.Dir, show, "patch-id", "--stable")
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			result[strings.ToLower(fields[1])] = fields[0]
		}
	}
	return result, nil
}

func (s Service) patchID(ctx context.Context, hash string) (string, error) {
	show, err := s.run(ctx, "show", "--pretty=format:", "--patch", hash)
	if err != nil {
		return "", err
	}
	out, err := s.runner().RunInput(ctx, s.Dir, show, "patch-id", "--stable")
	if err != nil {
		return "", err
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return "", nil
	}
	return fields[0], nil
}

func (s Service) pickedHashes(ctx context.Context, base string) (map[string]struct{}, error) {
	out, err := s.run(ctx, "log", base, "--format=%B%x00", "--fixed-strings", "--grep=cherry picked from commit")
	if err != nil {
		return nil, err
	}
	result := make(map[string]struct{})
	for _, match := range pickedPattern.FindAllStringSubmatch(string(out), -1) {
		result[strings.ToLower(match[1])] = struct{}{}
	}
	return result, nil
}

func (s Service) CreateBranch(ctx context.Context, target, base string) error {
	_, err := s.run(ctx, "switch", "-c", target, base)
	return err
}

func (s Service) CherryPick(ctx context.Context, hashes []string) error {
	args := append([]string{"cherry-pick", "-x"}, hashes...)
	_, err := s.run(ctx, args...)
	return err
}

func (s Service) Continue(ctx context.Context) error {
	_, err := s.run(ctx, "cherry-pick", "--continue")
	return err
}

func (s Service) Skip(ctx context.Context) error {
	_, err := s.run(ctx, "cherry-pick", "--skip")
	return err
}

func (s Service) Abort(ctx context.Context) error {
	_, err := s.run(ctx, "cherry-pick", "--abort")
	return err
}

func (s Service) CherryPickInProgress(ctx context.Context) (bool, error) {
	path, err := s.GitPath(ctx, "CHERRY_PICK_HEAD")
	if err != nil {
		return false, err
	}
	if _, err := os.Stat(path); err == nil {
		return true, nil
	} else if errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else {
		return false, err
	}
}

func (s Service) Unresolved(ctx context.Context) ([]string, error) {
	out, err := s.run(ctx, "diff", "--name-only", "--diff-filter=U")
	return strings.Fields(string(out)), err
}

func (s Service) StatusShort(ctx context.Context) string {
	out, _ := s.run(ctx, "status", "--short")
	return strings.TrimSpace(string(out))
}

func (s Service) RestoreAndDeleteBranch(ctx context.Context, originalHash, originalBranch, target string) error {
	if originalBranch != "" {
		if _, err := s.run(ctx, "switch", originalBranch); err != nil {
			return err
		}
	} else if _, err := s.run(ctx, "switch", "--detach", originalHash); err != nil {
		return err
	}
	_, err := s.run(ctx, "branch", "-D", target)
	return err
}
