package pickstate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	gitservice "kit/internal/git"
)

var ErrNotFound = errors.New("no kit pick operation is in progress")

type State struct {
	OriginalHash    string   `json:"original_hash"`
	OriginalBranch  string   `json:"original_branch,omitempty"`
	TargetBranch    string   `json:"target_branch"`
	BaseBranch      string   `json:"base_branch"`
	SourceBranch    string   `json:"source_branch"`
	Commits         []string `json:"commits"`
	Next            int      `json:"next"`
	HeadBefore      string   `json:"head_before,omitempty"`
	SubmitAfterPick bool     `json:"submit_after_pick,omitempty"`
	WaitAfterSubmit bool     `json:"wait_after_submit,omitempty"`
}

func Load(ctx context.Context, service gitservice.Service) (State, error) {
	path, err := statePath(ctx, service)
	if err != nil {
		return State{}, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return State{}, ErrNotFound
	}
	if err != nil {
		return State{}, err
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return State{}, fmt.Errorf("decode %s: %w", path, err)
	}
	if state.OriginalHash == "" || state.TargetBranch == "" || state.BaseBranch == "" || state.SourceBranch == "" || len(state.Commits) == 0 || state.Next < 0 || state.Next > len(state.Commits) {
		return State{}, fmt.Errorf("invalid pick state in %s", path)
	}
	return state, nil
}

func Save(ctx context.Context, service gitservice.Service, state State) error {
	path, err := statePath(ctx, service)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, append(data, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return nil
}

func Remove(ctx context.Context, service gitservice.Service) error {
	path, err := statePath(ctx, service)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func statePath(ctx context.Context, service gitservice.Service) (string, error) {
	return service.GitPath(ctx, "kit/pick-state.json")
}
