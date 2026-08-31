package git

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"
)

// CherryPickedFrom returns the original commit recorded by git cherry-pick -x.
// It is intentionally strict: multiple conflicting trailers are rejected.
func (s Service) CherryPickedFrom(ctx context.Context, revision string) (string, bool, error) {
	if err := s.VerifyRevision(ctx, revision); err != nil {
		return "", false, err
	}
	out, err := s.run(ctx, "show", "-s", "--format=%B", revision)
	if err != nil {
		return "", false, err
	}
	matches := pickedPattern.FindAllStringSubmatch(string(out), -1)
	if len(matches) == 0 {
		return "", false, nil
	}
	original := strings.ToLower(matches[0][1])
	for _, match := range matches[1:] {
		if strings.ToLower(match[1]) != original {
			return "", false, fmt.Errorf("commit %s contains conflicting cherry-pick source trailers", revision)
		}
	}
	return original, true, nil
}

func (s Service) RemoteBranchHash(ctx context.Context, remote, branch string) (string, bool, error) {
	if err := validateRemoteName(remote); err != nil {
		return "", false, err
	}
	if err := s.ValidateBranchName(ctx, branch); err != nil {
		return "", false, err
	}
	ref := "refs/heads/" + branch
	out, err := s.run(ctx, "ls-remote", "--heads", remote, ref)
	if err != nil {
		return "", false, err
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		return "", false, nil
	}
	fields := strings.Fields(line)
	if len(fields) != 2 || fields[1] != ref || len(fields[0]) != 40 {
		return "", false, fmt.Errorf("unexpected remote branch response for %s/%s", remote, branch)
	}
	if _, err := hex.DecodeString(fields[0]); err != nil {
		return "", false, fmt.Errorf("remote branch %s/%s returned an invalid object id", remote, branch)
	}
	return strings.ToLower(fields[0]), true, nil
}

// DeleteRemoteBranchIfMatches removes a remote review branch only when its
// current tip is exactly the provider-observed published tip. A missing branch
// already satisfies the desired postcondition.
func (s Service) DeleteRemoteBranchIfMatches(ctx context.Context, remote, branch, expectedHash string) (bool, error) {
	expectedHash = strings.ToLower(strings.TrimSpace(expectedHash))
	if len(expectedHash) != 40 {
		return false, fmt.Errorf("expected remote branch hash must be a full commit id")
	}
	if _, err := hex.DecodeString(expectedHash); err != nil {
		return false, fmt.Errorf("expected remote branch hash is invalid")
	}
	current, exists, err := s.RemoteBranchHash(ctx, remote, branch)
	if err != nil {
		return false, err
	}
	if !exists {
		return false, nil
	}
	if current != expectedHash {
		return false, fmt.Errorf("refusing to delete %s/%s: remote tip %s does not match published tip %s", remote, branch, current, expectedHash)
	}
	if _, err := s.run(ctx, "push", "--delete", remote, branch); err != nil {
		return false, err
	}
	if _, exists, err := s.RemoteBranchHash(ctx, remote, branch); err != nil {
		return false, fmt.Errorf("verify remote branch deletion: %w", err)
	} else if exists {
		return false, fmt.Errorf("verify remote branch deletion: %s/%s still exists", remote, branch)
	}
	return true, nil
}
