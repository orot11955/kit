package git

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

func validateRemoteName(remote string) error {
	if remote == "" || strings.HasPrefix(remote, "-") || strings.ContainsAny(remote, " \t\r\n") {
		return fmt.Errorf("invalid Git remote name %q", remote)
	}
	return nil
}

func (s Service) GitPath(ctx context.Context, name string) (string, error) {
	out, err := s.run(ctx, "rev-parse", "--git-path", name)
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(string(out))
	if !filepath.IsAbs(value) {
		value = filepath.Join(s.Dir, value)
	}
	return filepath.Clean(value), nil
}

func (s Service) RevisionHash(ctx context.Context, revision string) (string, error) {
	out, err := s.run(ctx, "rev-parse", "--verify", revision+"^{commit}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func (s Service) IsAncestor(ctx context.Context, ancestor, descendant string) (bool, error) {
	_, err := s.run(ctx, "merge-base", "--is-ancestor", ancestor, descendant)
	if err == nil {
		return true, nil
	}
	// Verify both revisions so a missing branch is not reported as a normal false result.
	if verifyErr := s.VerifyRevision(ctx, ancestor); verifyErr != nil {
		return false, verifyErr
	}
	if verifyErr := s.VerifyRevision(ctx, descendant); verifyErr != nil {
		return false, verifyErr
	}
	return false, nil
}

func (s Service) MergeCount(ctx context.Context, base, source string) (int, error) {
	out, err := s.run(ctx, "rev-list", "--count", "--first-parent", "--right-only", "--merges", base+"..."+source)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(out)))
}

func (s Service) AheadBehind(ctx context.Context, left, right string) (ahead, behind int, err error) {
	out, err := s.run(ctx, "rev-list", "--left-right", "--count", left+"..."+right)
	if err != nil {
		return 0, 0, err
	}
	fields := strings.Fields(string(out))
	if len(fields) != 2 {
		return 0, 0, fmt.Errorf("unexpected ahead/behind result %q", strings.TrimSpace(string(out)))
	}
	ahead, err = strconv.Atoi(fields[0])
	if err != nil {
		return 0, 0, err
	}
	behind, err = strconv.Atoi(fields[1])
	return ahead, behind, err
}

func (s Service) Fetch(ctx context.Context, remote string) error {
	if err := validateRemoteName(remote); err != nil {
		return err
	}
	_, err := s.run(ctx, "fetch", "--prune", remote)
	return err
}

func (s Service) Switch(ctx context.Context, branch string) error {
	_, err := s.run(ctx, "switch", branch)
	return err
}

func (s Service) SwitchDetach(ctx context.Context, revision string) error {
	_, err := s.run(ctx, "switch", "--detach", revision)
	return err
}

func (s Service) MergeFFOnly(ctx context.Context, revision string) error {
	_, err := s.run(ctx, "merge", "--ff-only", revision)
	return err
}

func (s Service) CreateBranchAt(ctx context.Context, branch, revision string) error {
	_, err := s.run(ctx, "branch", branch, revision)
	return err
}

func (s Service) ForceBranch(ctx context.Context, branch, revision string) error {
	_, err := s.run(ctx, "branch", "-f", branch, revision)
	return err
}

func (s Service) DeleteBranch(ctx context.Context, branch string, force bool) error {
	flag := "-d"
	if force {
		flag = "-D"
	}
	_, err := s.run(ctx, "branch", flag, branch)
	return err
}

func (s Service) CherryPickOne(ctx context.Context, hash string) error {
	_, err := s.run(ctx, "cherry-pick", "-x", hash)
	return err
}

func (s Service) RemoteTrackingBranchExists(ctx context.Context, remote, branch string) (bool, error) {
	if err := validateRemoteName(remote); err != nil {
		return false, err
	}
	ref := "refs/remotes/" + remote + "/" + branch
	out, err := s.run(ctx, "for-each-ref", "--format=%(refname)", ref)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) == ref, nil
}

func (s Service) RemoteBranchExists(ctx context.Context, remote, branch string) (bool, error) {
	if err := validateRemoteName(remote); err != nil {
		return false, err
	}
	out, err := s.run(ctx, "ls-remote", "--heads", remote, "refs/heads/"+branch)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) != "", nil
}

func (s Service) RemoteURL(ctx context.Context, remote string) (string, error) {
	if err := validateRemoteName(remote); err != nil {
		return "", err
	}
	out, err := s.run(ctx, "remote", "get-url", remote)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func (s Service) Upstream(ctx context.Context) (string, error) {
	out, err := s.run(ctx, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}")
	if err != nil {
		return "", nil
	}
	return strings.TrimSpace(string(out)), nil
}

func (s Service) UpstreamForBranch(ctx context.Context, branch string) (string, error) {
	if err := s.ValidateBranchName(ctx, branch); err != nil {
		return "", err
	}
	out, err := s.run(ctx, "for-each-ref", "--format=%(upstream:short)", "refs/heads/"+branch)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func (s Service) PushCurrent(ctx context.Context, remote, branch string, setUpstream bool) error {
	if err := validateRemoteName(remote); err != nil {
		return err
	}
	args := []string{"push"}
	if setUpstream {
		args = append(args, "--set-upstream")
	}
	args = append(args, remote, "HEAD:refs/heads/"+branch)
	_, err := s.run(ctx, args...)
	return err
}

func (s Service) ListRefs(ctx context.Context, prefix string) ([]string, error) {
	out, err := s.run(ctx, "for-each-ref", "--format=%(refname:short)", "refs/heads/"+prefix)
	if err != nil {
		return nil, err
	}
	return strings.Fields(string(out)), nil
}
