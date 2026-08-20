package reviewstate

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kit/internal/review"
)

type testResolver struct{ gitDirectory string }

func (resolver testResolver) GitPath(_ context.Context, name string) (string, error) {
	return filepath.Join(resolver.gitDirectory, filepath.FromSlash(name)), nil
}

func TestSaveLoadListAndDeleteReviewState(t *testing.T) {
	gitDirectory := filepath.Join(t.TempDir(), ".git")
	resolver := testResolver{gitDirectory: gitDirectory}
	state := pickedState("feat/login")
	if err := Save(context.Background(), resolver, state); err != nil {
		t.Fatal(err)
	}

	filename := filepath.Join(gitDirectory, "kit", "reviews", stateKey(state.Branch)+".json")
	info, err := os.Stat(filename)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("unexpected file mode: %o", info.Mode().Perm())
	}
	directoryInfo, err := os.Stat(filepath.Dir(filename))
	if err != nil {
		t.Fatal(err)
	}
	if directoryInfo.Mode().Perm() != 0o700 {
		t.Fatalf("unexpected directory mode: %o", directoryInfo.Mode().Perm())
	}

	loaded, err := Load(context.Background(), resolver, state.Branch)
	if err != nil || loaded.Branch != state.Branch || loaded.SchemaVersion != SchemaVersion || loaded.CreatedAt.IsZero() || loaded.UpdatedAt.IsZero() {
		t.Fatalf("unexpected loaded state: %#v, %v", loaded, err)
	}
	if strings.Contains(filename, "feat") || strings.Contains(filename, "login") {
		t.Fatalf("branch leaked into filename: %s", filename)
	}

	second := pickedState("feat/logout")
	if err := Save(context.Background(), resolver, second); err != nil {
		t.Fatal(err)
	}
	states, err := List(context.Background(), resolver)
	if err != nil || len(states) != 2 || states[0].Branch != "feat/login" || states[1].Branch != "feat/logout" {
		t.Fatalf("unexpected states: %#v, %v", states, err)
	}
	if err := Delete(context.Background(), resolver, state.Branch); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(context.Background(), resolver, state.Branch); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected not found, got %v", err)
	}
}

func TestSavePreservesCleanedState(t *testing.T) {
	resolver := testResolver{gitDirectory: filepath.Join(t.TempDir(), ".git")}
	now := time.Now().UTC()
	state := pickedState("feat/login")
	state.Stage = StageCleaned
	state.Provider = "gitlab"
	state.Remote = "origin"
	state.PublishedTip = "published-tip"
	state.ReviewID = "42"
	state.ReviewNumber = 42
	state.ReviewURL = "https://gitlab.example.com/owner/repo/-/merge_requests/42"
	state.Status = review.StatusMerged
	state.MergedAt = &now
	state.SyncedAt = &now
	state.CleanedAt = &now
	if err := Save(context.Background(), resolver, state); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(context.Background(), resolver, state.Branch)
	if err != nil || loaded.Stage != StageCleaned || loaded.CleanedAt == nil {
		t.Fatalf("cleaned state was not preserved: %#v, %v", loaded, err)
	}
}

func TestSaveSupportsGiteaAndLegacySchemaOneProviders(t *testing.T) {
	for _, provider := range []string{"gitea", "gitlab", "forgejo"} {
		t.Run(provider, func(t *testing.T) {
			resolver := testResolver{gitDirectory: filepath.Join(t.TempDir(), ".git")}
			state := pickedState("feat/" + provider)
			state.Stage = StagePublished
			state.Provider = provider
			state.Remote = "origin"
			state.PublishedTip = "published-tip"
			if err := Save(context.Background(), resolver, state); err != nil {
				t.Fatalf("save schema 1 %s state: %v", provider, err)
			}
			loaded, err := Load(context.Background(), resolver, state.Branch)
			if err != nil || loaded.Provider != provider || loaded.SchemaVersion != SchemaVersion {
				t.Fatalf("load schema 1 %s state: %#v, %v", provider, loaded, err)
			}
		})
	}
}

func TestLoadLegacySchemaOneFixture(t *testing.T) {
	resolver := testResolver{gitDirectory: filepath.Join(t.TempDir(), ".git")}
	const branch = "feat/legacy-gitlab"
	filename, err := statePath(context.Background(), resolver, branch)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(filename), 0o700); err != nil {
		t.Fatal(err)
	}
	fixture := `{
  "schema_version": 1,
  "stage": "open",
  "provider": "gitlab",
  "remote": "origin",
  "branch": "feat/legacy-gitlab",
  "source_branch": "work",
  "target_branch": "develop",
  "source_commits": ["legacy-commit"],
  "review_id": "42",
  "review_number": 42,
  "review_url": "https://gitlab.example.com/owner/repo/-/merge_requests/42",
  "status": "open",
  "picked_tip": "picked-tip",
  "published_tip": "published-tip",
  "created_at": "2026-01-01T00:00:00Z",
  "updated_at": "2026-01-01T00:01:00Z"
}`
	if err := os.WriteFile(filename, []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(context.Background(), resolver, branch)
	if err != nil {
		t.Fatalf("load legacy schema 1 fixture: %v", err)
	}
	if loaded.Provider != "gitlab" || loaded.Stage != StageOpen || loaded.ReviewNumber != 42 {
		t.Fatalf("unexpected legacy state: %#v", loaded)
	}
}

func TestInvalidStateDoesNotReplaceExistingAtomicFile(t *testing.T) {
	resolver := testResolver{gitDirectory: filepath.Join(t.TempDir(), ".git")}
	state := pickedState("feat/login")
	if err := Save(context.Background(), resolver, state); err != nil {
		t.Fatal(err)
	}
	state.Stage = "invalid"
	if err := Save(context.Background(), resolver, state); err == nil {
		t.Fatal("expected validation failure")
	}
	loaded, err := Load(context.Background(), resolver, state.Branch)
	if err != nil || loaded.Stage != StagePicked {
		t.Fatalf("valid state was replaced: %#v, %v", loaded, err)
	}
	entries, err := os.ReadDir(filepath.Join(resolver.gitDirectory, "kit", "reviews"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tmp") {
			t.Fatalf("temporary file was not cleaned: %s", entry.Name())
		}
	}
}

func TestLoadRejectsUnknownFieldsAndInvalidSchema(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "unknown token", mutate: func(value map[string]any) { value["token"] = "must-not-load" }},
		{name: "future schema", mutate: func(value map[string]any) { value["schema_version"] = 2 }},
		{name: "invalid stage", mutate: func(value map[string]any) { value["stage"] = "unknown" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resolver := testResolver{gitDirectory: filepath.Join(t.TempDir(), ".git")}
			state := pickedState("feat/login")
			state.SchemaVersion = SchemaVersion
			state.CreatedAt = time.Now().UTC()
			state.UpdatedAt = state.CreatedAt
			data, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			var value map[string]any
			if err := json.Unmarshal(data, &value); err != nil {
				t.Fatal(err)
			}
			test.mutate(value)
			data, err = json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			filename, err := statePath(context.Background(), resolver, state.Branch)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(filename), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filename, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(context.Background(), resolver, state.Branch); err == nil {
				t.Fatal("expected invalid state error")
			}
		})
	}
}

func TestLoadRejectsMismatchedHashKey(t *testing.T) {
	resolver := testResolver{gitDirectory: filepath.Join(t.TempDir(), ".git")}
	state := pickedState("feat/login")
	if err := Save(context.Background(), resolver, state); err != nil {
		t.Fatal(err)
	}
	filename, err := statePath(context.Background(), resolver, state.Branch)
	if err != nil {
		t.Fatal(err)
	}
	otherFilename, err := statePath(context.Background(), resolver, "feat/other")
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(otherFilename, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(context.Background(), resolver, "feat/other"); err == nil {
		t.Fatal("expected branch key mismatch")
	}
}

func TestReviewStateAllowsOnlyPrivateLiteralGiteaHTTPURL(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		url      string
		valid    bool
	}{
		{name: "RFC1918", provider: "gitea", url: "http://192.168.1.20:3000/owner/repo/pulls/7", valid: true},
		{name: "IPv6 ULA", provider: "gitea", url: "http://[fd00::20]:3000/owner/repo/pulls/7", valid: true},
		{name: "loopback", provider: "gitea", url: "http://127.0.0.1:3000/owner/repo/pulls/7", valid: true},
		{name: "hostname", provider: "gitea", url: "http://gitea.lan/owner/repo/pulls/7"},
		{name: "public IP", provider: "gitea", url: "http://8.8.8.8/owner/repo/pulls/7"},
		{name: "legacy provider", provider: "forgejo", url: "http://10.0.0.2/owner/repo/pulls/7"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resolver := testResolver{gitDirectory: filepath.Join(t.TempDir(), ".git")}
			state := pickedState("feat/http")
			state.Stage = StageOpen
			state.Provider = test.provider
			state.Remote = "origin"
			state.PublishedTip = "published-tip"
			state.ReviewID = "7"
			state.ReviewNumber = 7
			state.ReviewURL = test.url
			state.Status = review.StatusOpen
			err := Save(context.Background(), resolver, state)
			if test.valid && err != nil {
				t.Fatalf("valid private HTTP state rejected: %v", err)
			}
			if !test.valid && err == nil {
				t.Fatalf("unsafe HTTP state accepted: %#v", state)
			}
		})
	}
}

func pickedState(branch string) State {
	return State{
		Stage:         StagePicked,
		Branch:        branch,
		SourceBranch:  "work",
		TargetBranch:  "develop",
		SourceCommits: []string{"commit-1"},
		PickedTip:     "picked-tip",
	}
}
