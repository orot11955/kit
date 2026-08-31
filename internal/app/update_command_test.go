package app

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"kit/internal/buildinfo"
	"kit/internal/update"
)

func TestUpdateCheckPassesCheckOnlyAndPrintsAvailability(t *testing.T) {
	var got update.Config
	var output bytes.Buffer
	a := &Application{
		IO:    IO{In: strings.NewReader(""), Out: &output, ErrOut: &output},
		Build: buildinfo.Info{Version: "v1.2.3"},
		Update: func(_ context.Context, cfg update.Config) (update.Result, error) {
			got = cfg
			return update.Result{Current: "v1.2.3", Latest: "v1.2.4", UpdateAvailable: true}, nil
		},
	}
	if err := a.Run(context.Background(), []string{"update", "--check"}); err != nil {
		t.Fatal(err)
	}
	if !got.CheckOnly || got.Rollback {
		t.Fatalf("unexpected config: %#v", got)
	}
	if !strings.Contains(output.String(), "v1.2.3 → v1.2.4") {
		t.Fatalf("availability output missing: %q", output.String())
	}
}

func TestUpdateRollbackPassesRollbackAndPrintsResult(t *testing.T) {
	var got update.Config
	var output bytes.Buffer
	a := &Application{
		IO:    IO{In: strings.NewReader(""), Out: &output, ErrOut: &output},
		Build: buildinfo.Info{Version: "v1.2.4"},
		Update: func(_ context.Context, cfg update.Config) (update.Result, error) {
			got = cfg
			return update.Result{Current: "v1.2.4", Previous: "v1.2.3", RolledBack: true}, nil
		},
	}
	if err := a.Run(context.Background(), []string{"update", "--rollback"}); err != nil {
		t.Fatal(err)
	}
	if !got.Rollback || got.CheckOnly {
		t.Fatalf("unexpected config: %#v", got)
	}
	if !strings.Contains(output.String(), "v1.2.4 → v1.2.3") || !strings.Contains(output.String(), "롤백") {
		t.Fatalf("rollback output missing: %q", output.String())
	}
}

func TestUpdateCheckAndRollbackAreMutuallyExclusive(t *testing.T) {
	called := false
	a := &Application{
		IO:    IO{In: strings.NewReader(""), Out: &bytes.Buffer{}, ErrOut: &bytes.Buffer{}},
		Build: buildinfo.Info{Version: "v1.2.4"},
		Update: func(context.Context, update.Config) (update.Result, error) {
			called = true
			return update.Result{}, nil
		},
	}
	err := a.Run(context.Background(), []string{"update", "--check", "--rollback"})
	if err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("expected mutually exclusive error, got %v", err)
	}
	if called {
		t.Fatal("update runner must not be called for invalid options")
	}
}

func TestUpdateCheckJSONKeepsMachineReadableResult(t *testing.T) {
	var output bytes.Buffer
	a := &Application{
		IO:    IO{In: strings.NewReader(""), Out: &output, ErrOut: &bytes.Buffer{}},
		Build: buildinfo.Info{Version: "v1.2.3"},
		Update: func(_ context.Context, cfg update.Config) (update.Result, error) {
			if !cfg.CheckOnly {
				t.Fatal("expected check-only config")
			}
			return update.Result{Current: "v1.2.3", Latest: "v1.2.4", UpdateAvailable: true}, nil
		},
	}
	if err := a.Run(context.Background(), []string{"update", "--check", "--json"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"update_available":true`) || !strings.Contains(output.String(), `"latest":"v1.2.4"`) {
		t.Fatalf("unexpected JSON: %q", output.String())
	}
}
