package app

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"kit/internal/auth"
)

type fakeAuthService struct {
	loginToken string
	loginStore auth.Store
	profile    auth.Profile
	profiles   []auth.Profile
	logoutHost string
	configDir  string
	err        error
}

func (f *fakeAuthService) Login(provider, host, token string, store auth.Store) (auth.Profile, error) {
	f.loginToken, f.loginStore = token, store
	if f.err != nil {
		return auth.Profile{}, f.err
	}
	if f.profile.Provider == "" {
		return auth.Profile{Provider: provider, Host: host, Store: auth.StoreKeyring}, nil
	}
	return f.profile, nil
}

func (f *fakeAuthService) Lookup(string, string) (string, error) { return "stored", f.err }
func (f *fakeAuthService) Status(string, string) (auth.Profile, error) {
	return f.profile, f.err
}
func (f *fakeAuthService) List() ([]auth.Profile, error) { return f.profiles, f.err }
func (f *fakeAuthService) Logout(_ string, host string) (auth.Profile, error) {
	f.logoutHost = host
	return f.profile, f.err
}
func (f *fakeAuthService) ConfigDir() string { return f.configDir }

func TestAuthLoginReadsInjectedSecretWithoutExposingIt(t *testing.T) {
	var output, errorOutput bytes.Buffer
	service := &fakeAuthService{}
	application := &Application{
		IO:   IO{In: strings.NewReader(""), Out: &output, ErrOut: &errorOutput},
		Auth: service,
		ReadSecret: func(prompt string) (string, error) {
			if prompt != "Gitea token: " {
				t.Fatalf("unexpected prompt: %q", prompt)
			}
			return "top-secret-token", nil
		},
	}
	if err := application.Run(context.Background(), []string{"auth", "login", "gitea", "--host", "git.example.com", "--json"}); err != nil {
		t.Fatal(err)
	}
	if service.loginToken != "top-secret-token" || service.loginStore != auth.StoreAuto {
		t.Fatalf("secret reader boundary was not used: %#v", service)
	}
	combined := output.String() + errorOutput.String()
	if strings.Contains(combined, "top-secret-token") || strings.Contains(combined, `"token"`) {
		t.Fatalf("token reached command output: %s", combined)
	}
}

func TestAuthLoginRejectsTokenArgumentBeforeReadingSecret(t *testing.T) {
	called := false
	application := &Application{
		IO:   IO{In: strings.NewReader(""), Out: &bytes.Buffer{}, ErrOut: &bytes.Buffer{}},
		Auth: &fakeAuthService{},
		ReadSecret: func(string) (string, error) {
			called = true
			return "", errors.New("must not run")
		},
	}
	err := application.Run(context.Background(), []string{"auth", "login", "gitea", "--host", "git.example.com", "--token", "unsafe"})
	if err == nil || called {
		t.Fatalf("CLI token argument was accepted: %v, reader called=%t", err, called)
	}
}

func TestAuthFileStorePrintsSanitizedPlaintextWarning(t *testing.T) {
	var output, errorOutput bytes.Buffer
	service := &fakeAuthService{
		profile:   auth.Profile{Provider: "gitea", Host: "git.example.com", Store: auth.StoreFile},
		configDir: "/tmp/kit\nspoof",
	}
	application := &Application{
		IO:         IO{In: strings.NewReader(""), Out: &output, ErrOut: &errorOutput},
		Auth:       service,
		ReadSecret: func(string) (string, error) { return "secret", nil },
	}
	if err := application.Run(context.Background(), []string{"auth", "login", "gitea", "--host", "git.example.com", "--store", "file"}); err != nil {
		t.Fatal(err)
	}
	warning := errorOutput.String()
	if !strings.Contains(warning, "평문") || !strings.Contains(warning, "/tmp/kit spoof") || strings.Contains(warning, "/tmp/kit\nspoof") {
		t.Fatalf("missing or unsafe file warning: %q", warning)
	}
}

func TestAuthLogoutRequiresConfirmationAndTargetsOneHost(t *testing.T) {
	profile := auth.Profile{Provider: "gitea", Host: "one.example.com", Store: auth.StoreKeyring}
	service := &fakeAuthService{profile: profile}
	application := &Application{
		IO:   IO{In: strings.NewReader("n\n"), Out: &bytes.Buffer{}, ErrOut: &bytes.Buffer{}},
		Auth: service,
	}
	if err := application.Run(context.Background(), []string{"auth", "logout", "gitea", "--host", "one.example.com"}); err != nil {
		t.Fatal(err)
	}
	if service.logoutHost != "" {
		t.Fatal("logout ran after confirmation was declined")
	}
	if err := application.Run(context.Background(), []string{"auth", "logout", "gitea", "--host", "one.example.com", "--yes", "--json"}); err != nil {
		t.Fatal(err)
	}
	if service.logoutHost != "one.example.com" {
		t.Fatalf("wrong logout target: %q", service.logoutHost)
	}
}
