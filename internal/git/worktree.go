package git

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

type Worktree struct {
	Path     string `json:"path"`
	Head     string `json:"head"`
	Branch   string `json:"branch,omitempty"`
	Bare     bool   `json:"bare,omitempty"`
	Detached bool   `json:"detached,omitempty"`
	Locked   bool   `json:"locked,omitempty"`
	Prunable bool   `json:"prunable,omitempty"`
}

func (s Service) TopLevel(ctx context.Context) (string, error) {
	out, err := s.run(ctx, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(string(out))
	if value == "" {
		return "", fmt.Errorf("repository top-level path is empty")
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	return filepath.Clean(absolute), nil
}

func (s Service) Worktrees(ctx context.Context) ([]Worktree, error) {
	out, err := s.run(ctx, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	text := strings.ReplaceAll(string(out), "\r\n", "\n")
	blocks := strings.Split(strings.TrimSpace(text), "\n\n")
	result := make([]Worktree, 0, len(blocks))
	for _, block := range blocks {
		if strings.TrimSpace(block) == "" {
			continue
		}
		var item Worktree
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "worktree "):
				item.Path = strings.TrimPrefix(line, "worktree ")
			case strings.HasPrefix(line, "HEAD "):
				item.Head = strings.TrimSpace(strings.TrimPrefix(line, "HEAD "))
			case strings.HasPrefix(line, "branch "):
				ref := strings.TrimSpace(strings.TrimPrefix(line, "branch "))
				item.Branch = strings.TrimPrefix(ref, "refs/heads/")
			case line == "bare":
				item.Bare = true
			case line == "detached":
				item.Detached = true
			case line == "locked" || strings.HasPrefix(line, "locked "):
				item.Locked = true
			case line == "prunable" || strings.HasPrefix(line, "prunable "):
				item.Prunable = true
			}
		}
		if item.Path == "" || item.Head == "" {
			return nil, fmt.Errorf("unexpected git worktree porcelain record %q", block)
		}
		result = append(result, item)
	}
	return result, nil
}

func (s Service) AddWorktree(ctx context.Context, path, branch string) error {
	if err := s.ValidateBranchName(ctx, branch); err != nil {
		return err
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("worktree path must be absolute")
	}
	_, err := s.run(ctx, "worktree", "add", filepath.Clean(path), branch)
	return err
}

func (s Service) AddWorktreeBranch(ctx context.Context, path, branch, base string) error {
	if err := s.ValidateBranchName(ctx, branch); err != nil {
		return err
	}
	if err := s.VerifyRevision(ctx, base); err != nil {
		return err
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("worktree path must be absolute")
	}
	_, err := s.run(ctx, "worktree", "add", "-b", branch, filepath.Clean(path), base)
	return err
}

func (s Service) RemoveWorktree(ctx context.Context, path string, force bool) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("worktree path must be absolute")
	}
	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, filepath.Clean(path))
	_, err := s.run(ctx, args...)
	return err
}

func (s Service) PruneWorktrees(ctx context.Context) error {
	_, err := s.run(ctx, "worktree", "prune")
	return err
}
