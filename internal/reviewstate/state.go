package reviewstate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"kit/internal/review"
)

const SchemaVersion = 1
const maxStateBytes = 1 << 20

var ErrNotFound = errors.New("review state was not found")

type Stage string

const (
	StagePicked    Stage = "picked"
	StagePublished Stage = "published"
	StageOpen      Stage = "open"
	StageMerged    Stage = "merged"
	StageSynced    Stage = "synced"
	StageCleaned   Stage = "cleaned"
	StageClosed    Stage = "closed"
)

type State struct {
	SchemaVersion int           `json:"schema_version"`
	Stage         Stage         `json:"stage"`
	Provider      string        `json:"provider,omitempty"`
	Remote        string        `json:"remote,omitempty"`
	Branch        string        `json:"branch"`
	SourceBranch  string        `json:"source_branch,omitempty"`
	TargetBranch  string        `json:"target_branch"`
	SourceCommits []string      `json:"source_commits,omitempty"`
	ReviewID      string        `json:"review_id,omitempty"`
	ReviewNumber  int64         `json:"review_number,omitempty"`
	ReviewURL     string        `json:"review_url,omitempty"`
	Status        review.Status `json:"status,omitempty"`
	PickedTip     string        `json:"picked_tip"`
	PublishedTip  string        `json:"published_tip,omitempty"`
	MergeSHA      string        `json:"merge_sha,omitempty"`
	CreatedAt     time.Time     `json:"created_at"`
	UpdatedAt     time.Time     `json:"updated_at"`
	MergedAt      *time.Time    `json:"merged_at,omitempty"`
	SyncedAt      *time.Time    `json:"synced_at,omitempty"`
	CleanedAt     *time.Time    `json:"cleaned_at,omitempty"`
}

type GitPathResolver interface {
	GitPath(ctx context.Context, name string) (string, error)
}

func Load(ctx context.Context, resolver GitPathResolver, branch string) (State, error) {
	filename, err := statePath(ctx, resolver, branch)
	if err != nil {
		return State{}, err
	}
	return loadFile(filename, branch)
}

func List(ctx context.Context, resolver GitPathResolver) ([]State, error) {
	directory, err := reviewsDirectory(ctx, resolver)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return []State{}, nil
	}
	if err != nil {
		return nil, err
	}
	states := make([]State, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		filename := filepath.Join(directory, entry.Name())
		state, err := loadFile(filename, "")
		if err != nil {
			return nil, err
		}
		if entry.Name() != stateKey(state.Branch)+".json" {
			return nil, fmt.Errorf("review state filename does not match branch key: %s", entry.Name())
		}
		states = append(states, state)
	}
	sort.Slice(states, func(left, right int) bool { return states[left].Branch < states[right].Branch })
	return states, nil
}

func Save(ctx context.Context, resolver GitPathResolver, state State) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	now := time.Now().UTC()
	if state.SchemaVersion == 0 {
		state.SchemaVersion = SchemaVersion
	}
	if state.CreatedAt.IsZero() {
		state.CreatedAt = now
	}
	state.UpdatedAt = now
	if err := validate(state); err != nil {
		return err
	}
	filename, err := statePath(ctx, resolver, state.Branch)
	if err != nil {
		return err
	}
	directory := filepath.Dir(filename)
	if err := ensureDirectory(directory); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')

	temporary, err := os.CreateTemp(directory, ".review-*.tmp")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	removeTemporary := true
	defer func() {
		_ = temporary.Close()
		if removeTemporary {
			_ = os.Remove(temporaryName)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return err
	}
	if _, err := temporary.Write(encoded); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryName, filename); err != nil {
		return err
	}
	removeTemporary = false
	if err := os.Chmod(filename, 0o600); err != nil {
		return err
	}
	return syncDirectory(directory)
}

func Delete(ctx context.Context, resolver GitPathResolver, branch string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	filename, err := statePath(ctx, resolver, branch)
	if err != nil {
		return err
	}
	if err := os.Remove(filename); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return ctx.Err()
	}
	return nil
}

func loadFile(filename, expectedBranch string) (State, error) {
	info, err := os.Lstat(filename)
	if errors.Is(err, os.ErrNotExist) {
		return State{}, ErrNotFound
	}
	if err != nil {
		return State{}, err
	}
	if !info.Mode().IsRegular() {
		return State{}, fmt.Errorf("review state is not a regular file: %s", filename)
	}
	if info.Size() > maxStateBytes {
		return State{}, fmt.Errorf("review state exceeds 1 MiB: %s", filename)
	}
	file, err := os.Open(filename)
	if err != nil {
		return State{}, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxStateBytes+1))
	if err != nil {
		return State{}, err
	}
	if len(data) > maxStateBytes {
		return State{}, fmt.Errorf("review state exceeds 1 MiB: %s", filename)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var state State
	if err := decoder.Decode(&state); err != nil {
		return State{}, fmt.Errorf("decode review state %s: %w", filename, err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return State{}, fmt.Errorf("decode review state %s: %w", filename, err)
	}
	if err := validate(state); err != nil {
		return State{}, fmt.Errorf("invalid review state %s: %w", filename, err)
	}
	if expectedBranch != "" && state.Branch != expectedBranch {
		return State{}, fmt.Errorf("review state branch does not match key: %s", filename)
	}
	return state, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return err
	}
	return errors.New("multiple JSON values")
}

func validate(state State) error {
	if state.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported schema version %d", state.SchemaVersion)
	}
	if state.Branch == "" || state.TargetBranch == "" || state.PickedTip == "" {
		return errors.New("branch, target branch, and picked tip are required")
	}
	if state.CreatedAt.IsZero() || state.UpdatedAt.IsZero() {
		return errors.New("created and updated timestamps are required")
	}
	if !validStage(state.Stage) {
		return fmt.Errorf("invalid stage %q", state.Stage)
	}
	if state.Status != "" && state.Status != review.StatusOpen && state.Status != review.StatusMerged && state.Status != review.StatusClosed {
		return fmt.Errorf("invalid review status %q", state.Status)
	}
	if state.ReviewURL != "" {
		parsed, err := url.Parse(state.ReviewURL)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil {
			return errors.New("review URL must be an HTTPS URL without credentials")
		}
	}
	if stageAtLeastPublished(state.Stage) {
		if state.Provider != "gitlab" && state.Provider != "forgejo" {
			return errors.New("published review state requires a supported provider")
		}
		if state.Remote == "" || state.PublishedTip == "" {
			return errors.New("published review state requires remote and published tip")
		}
	}
	if stageHasReview(state.Stage) {
		if state.ReviewID == "" || state.ReviewNumber <= 0 || state.ReviewURL == "" {
			return errors.New("review stage requires review ID, number, and URL")
		}
	}
	if state.Stage == StageOpen && state.Status != review.StatusOpen {
		return errors.New("open stage requires open review status")
	}
	if state.Stage == StageMerged || state.Stage == StageSynced || state.Stage == StageCleaned {
		if state.Status != review.StatusMerged {
			return errors.New("merged, synced, and cleaned stages require merged review status")
		}
	}
	if state.Stage == StageClosed && state.Status != review.StatusClosed {
		return errors.New("closed stage requires closed review status")
	}
	if (state.Stage == StageSynced || state.Stage == StageCleaned) && state.SyncedAt == nil {
		return errors.New("synced and cleaned stages require synced timestamp")
	}
	if state.Stage == StageCleaned && state.CleanedAt == nil {
		return errors.New("cleaned stage requires cleaned timestamp")
	}
	return nil
}

func validStage(stage Stage) bool {
	switch stage {
	case StagePicked, StagePublished, StageOpen, StageMerged, StageSynced, StageCleaned, StageClosed:
		return true
	default:
		return false
	}
}

func stageAtLeastPublished(stage Stage) bool {
	return stage != StagePicked
}

func stageHasReview(stage Stage) bool {
	return stage == StageOpen || stage == StageMerged || stage == StageSynced || stage == StageCleaned || stage == StageClosed
}

func ensureDirectory(directory string) error {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("review state directory is not a directory: %s", directory)
	}
	return os.Chmod(directory, 0o700)
}

func syncDirectory(directory string) error {
	handle, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer handle.Close()
	return handle.Sync()
}

func statePath(ctx context.Context, resolver GitPathResolver, branch string) (string, error) {
	if strings.TrimSpace(branch) == "" {
		return "", errors.New("review branch is required")
	}
	return resolver.GitPath(ctx, "kit/reviews/"+stateKey(branch)+".json")
}

func reviewsDirectory(ctx context.Context, resolver GitPathResolver) (string, error) {
	marker, err := resolver.GitPath(ctx, "kit/reviews/.directory")
	if err != nil {
		return "", err
	}
	return filepath.Dir(marker), nil
}

func stateKey(branch string) string {
	digest := sha256.Sum256([]byte(branch))
	return hex.EncodeToString(digest[:])
}
