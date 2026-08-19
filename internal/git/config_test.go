package git

import (
	"context"
	"testing"
)

func TestWorkflowConfigDefaultsAndRepositoryOverrides(t *testing.T) {
	dir := initRepository(t)
	service := Service{Dir: dir}
	defaults := service.WorkflowConfig(context.Background())
	if defaults.Provider != "auto" || defaults.Remote != "origin" || defaults.Stable != "main" || defaults.Base != "develop" || defaults.Source != "work" {
		t.Fatalf("unexpected defaults: %#v", defaults)
	}
	if err := service.ConfigSet(context.Background(), "git.provider", "gitlab"); err != nil {
		t.Fatal(err)
	}
	if err := service.ConfigSet(context.Background(), "git.source", "scratch"); err != nil {
		t.Fatal(err)
	}
	config := service.WorkflowConfig(context.Background())
	if config.Provider != "gitlab" || config.Source != "scratch" || config.Base != "develop" {
		t.Fatalf("unexpected overrides: %#v", config)
	}
}

func TestWorkflowConfigRejectsUnknownProvider(t *testing.T) {
	dir := initRepository(t)
	err := (Service{Dir: dir}).ConfigSet(context.Background(), "git.provider", "github")
	if err == nil {
		t.Fatal("expected invalid provider error")
	}
}

func TestWorkflowConfigRejectsOptionLikeRemote(t *testing.T) {
	dir := initRepository(t)
	err := (Service{Dir: dir}).ConfigSet(context.Background(), "git.remote", "--upload-pack=evil")
	if err == nil {
		t.Fatal("expected invalid remote error")
	}
}
