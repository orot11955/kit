package git

import (
	"context"
	"errors"
	"testing"
)

func TestWorkflowConfigPushRemoteFallsBackToRemote(t *testing.T) {
	config := DefaultWorkflowConfig()
	if config.PushRemote != "" {
		t.Fatalf("default push remote should be unset: %#v", config)
	}
	if got := config.PushRemoteName(); got != "origin" {
		t.Fatalf("PushRemoteName()=%q want origin", got)
	}
}

func TestWorkflowConfigReadsOptionalPushRemote(t *testing.T) {
	dir := initRepository(t)
	service := Service{Dir: dir}
	ctx := context.Background()
	if err := service.ConfigSet(ctx, "git.remote", "upstream"); err != nil {
		t.Fatal(err)
	}
	if err := service.ConfigSet(ctx, "git.push-remote", "origin"); err != nil {
		t.Fatal(err)
	}
	config, err := service.WorkflowConfigStrict(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if config.Remote != "upstream" || config.PushRemote != "origin" || config.PushRemoteName() != "origin" {
		t.Fatalf("unexpected workflow config: %#v", config)
	}
	if err := service.ConfigUnset(ctx, "git.push-remote"); err != nil {
		t.Fatal(err)
	}
	config, err = service.WorkflowConfigStrict(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if config.PushRemote != "" || config.PushRemoteName() != "upstream" {
		t.Fatalf("unset push remote did not fall back: %#v", config)
	}
}

func TestInitializeWorkflowConfigDoesNotWriteDefaultPushRemote(t *testing.T) {
	dir := initRepository(t)
	service := Service{Dir: dir}
	if _, err := service.InitializeWorkflowConfig(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ConfigGet(context.Background(), "git.push-remote"); !errors.Is(err, ErrConfigNotSet) {
		t.Fatalf("config init unexpectedly persisted optional push remote: %v", err)
	}
}
