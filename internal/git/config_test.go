package git

import (
	"context"
	"testing"
)

func TestWorkflowConfigDefaultsAndRepositoryOverrides(t *testing.T) {
	dir := initRepository(t)
	service := Service{Dir: dir}
	defaults := service.WorkflowConfig(context.Background())
	if defaults.Provider != "gitea" || defaults.Remote != "origin" || defaults.Stable != "main" || defaults.Base != "develop" || defaults.Source != "work" || defaults.AllowInsecureHTTP {
		t.Fatalf("unexpected defaults: %#v", defaults)
	}
	if err := service.ConfigSet(context.Background(), "git.provider", "gitea"); err != nil {
		t.Fatal(err)
	}
	if err := service.ConfigSet(context.Background(), "git.source", "scratch"); err != nil {
		t.Fatal(err)
	}
	config := service.WorkflowConfig(context.Background())
	if config.Provider != "gitea" || config.Source != "scratch" || config.Base != "develop" {
		t.Fatalf("unexpected overrides: %#v", config)
	}
}

func TestAllowInsecureHTTPConfigRequiresCanonicalBoolean(t *testing.T) {
	dir := initRepository(t)
	service := Service{Dir: dir}
	for _, value := range []string{"TRUE", "False", "1", "yes", ""} {
		if err := service.ConfigSet(context.Background(), "git.allow-insecure-http", value); err == nil {
			t.Fatalf("accepted non-canonical boolean %q", value)
		}
	}
	if err := service.ConfigSet(context.Background(), "git.allow-insecure-http", "true"); err != nil {
		t.Fatal(err)
	}
	if !service.WorkflowConfig(context.Background()).AllowInsecureHTTP {
		t.Fatal("true opt-in was not resolved")
	}
	if err := service.ConfigSet(context.Background(), "git.allow-insecure-http", "false"); err != nil {
		t.Fatal(err)
	}
	if service.WorkflowConfig(context.Background()).AllowInsecureHTTP {
		t.Fatal("false opt-in was not resolved")
	}
}

func TestInitializeWorkflowConfigWritesInsecureHTTPDefault(t *testing.T) {
	dir := initRepository(t)
	service := Service{Dir: dir}
	if _, err := service.InitializeWorkflowConfig(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, err := service.ConfigGet(context.Background(), "git.allow-insecure-http"); err != nil || got != "false" {
		t.Fatalf("unexpected initialized opt-in: %q, %v", got, err)
	}
}

func TestWorkflowConfigKeepsLegacyProvidersReadable(t *testing.T) {
	dir := initRepository(t)
	service := Service{Dir: dir}
	for _, provider := range []string{"gitlab", "forgejo"} {
		if err := service.ConfigSet(context.Background(), "git.provider", provider); err == nil {
			t.Fatalf("new legacy provider %s must be rejected", provider)
		}
		if _, err := service.run(context.Background(), "config", "--local", "kit.git.provider", provider); err != nil {
			t.Fatal(err)
		}
		if got := service.WorkflowConfig(context.Background()).Provider; got != provider {
			t.Fatalf("legacy provider %s was not readable: %s", provider, got)
		}
		if err := service.ConfigSet(context.Background(), "git.provider", provider); err != nil {
			t.Fatalf("unchanged legacy provider %s must remain writable during config init: %v", provider, err)
		}
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
