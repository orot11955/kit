package git

import (
	"context"
	"strings"
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

func TestKitCreatedBranchMarkerLifecycle(t *testing.T) {
	dir := initRepository(t)
	service := Service{Dir: dir}
	ctx := context.Background()
	for _, branch := range []string{"review/one", "Review.Two"} {
		if err := service.MarkKitCreatedBranch(ctx, branch); err != nil {
			t.Fatal(err)
		}
	}
	branches, err := service.KitCreatedBranches(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(branches) != 2 || branches[0] != "Review.Two" || branches[1] != "review/one" {
		t.Fatalf("unexpected Kit branch markers: %#v", branches)
	}
	if err := service.ClearKitCreatedBranch(ctx, "review/one"); err != nil {
		t.Fatal(err)
	}
	if marked, err := service.IsKitCreatedBranch(ctx, "review/one"); err != nil || marked {
		t.Fatalf("marker remained after clear: marked=%v err=%v", marked, err)
	}
	if err := service.ClearKitCreatedBranch(ctx, "review/one"); err != nil {
		t.Fatalf("clearing an absent marker should be harmless: %v", err)
	}
}

func TestKitCreatedBranchesRequireExactlyOneCanonicalTrueMarker(t *testing.T) {
	dir := initRepository(t)
	service := Service{Dir: dir}
	ctx := context.Background()
	for branch, values := range map[string][]string{
		"review/false":     {"false"},
		"review/arbitrary": {"yes"},
		"review/case":      {"TRUE"},
		"review/multiple":  {"true", "true"},
		"review/valid":     {"true"},
	} {
		for _, value := range values {
			if _, err := service.run(ctx, "config", "--local", "--add", "branch."+branch+".kitCreated", value); err != nil {
				t.Fatal(err)
			}
		}
	}
	branches, err := service.KitCreatedBranches(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(branches) != 1 || branches[0] != "review/valid" {
		t.Fatalf("unexpected trusted Kit markers: %#v", branches)
	}
	if err := service.ClearKitCreatedBranch(ctx, "review/false"); err != nil {
		t.Fatal(err)
	}
	out, err := service.run(ctx, "config", "--local", "--get-all", "branch.review/false.kitCreated")
	if err == nil || strings.TrimSpace(string(out)) != "" {
		t.Fatalf("corrupted marker remained after clear: %q, %v", out, err)
	}
}
