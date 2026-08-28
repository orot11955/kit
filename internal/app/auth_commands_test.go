package app

import (
	"bytes"
	"context"
	"errors"
	"os"
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

type retryAuthService struct {
	lookupCalls int
	lookup      []error
	token       string
}

func (f *retryAuthService) Login(string, string, string, auth.Store) (auth.Profile, error) {
	return auth.Profile{}, errors.New("not implemented")
}
func (f *retryAuthService) Lookup(string, string) (string, error) {
	f.lookupCalls++
	if len(f.lookup) > 0 {
		err := f.lookup[0]
		f.lookup = f.lookup[1:]
		if err != nil {
			return "", err
		}
	}
	return f.token, nil
}
func (f *retryAuthService) Status(string, string) (auth.Profile, error) {
	return auth.Profile{}, errors.New("not implemented")
}
func (f *retryAuthService) List() ([]auth.Profile, error) { return nil, errors.New("not implemented") }
func (f *retryAuthService) Logout(string, string) (auth.Profile, error) {
	return auth.Profile{}, errors.New("not implemented")
}
func (f *retryAuthService) ConfigDir() string { return "" }

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

func TestReviewCredentialUnlocksKeychainOnceThenRetriesLookup(t *testing.T) {
	terminal, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer terminal.Close()

	service := &retryAuthService{lookup: []error{auth.ErrKeychainInteractionRequired}, token: "never-print-this-token"}
	var output bytes.Buffer
	unlocks := 0
	application := &Application{
		IO:                  IO{In: strings.NewReader(""), Out: &output, ErrOut: &output, InFile: terminal},
		allowKeychainUnlock: true,
		KeychainSupported:   func() bool { return true },
		IsKeychainTTY:       func(*os.File) bool { return true },
		UnlockKeychain:      func(*os.File) error { unlocks++; return nil },
	}
	token, err := application.lookupReviewCredential(service, "gitea", "git.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if token != "never-print-this-token" || service.lookupCalls != 2 || unlocks != 1 {
		t.Fatalf("unexpected retry: token=%q lookupCalls=%d unlocks=%d", token, service.lookupCalls, unlocks)
	}
	if strings.Contains(output.String(), token) {
		t.Fatalf("credential reached output: %q", output.String())
	}
}

func TestReviewCredentialStopsWhenKeychainUnlockFails(t *testing.T) {
	terminal, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer terminal.Close()

	service := &retryAuthService{lookup: []error{auth.ErrKeychainInteractionRequired}}
	application := &Application{
		IO:                  IO{InFile: terminal, ErrOut: &bytes.Buffer{}},
		allowKeychainUnlock: true,
		KeychainSupported:   func() bool { return true },
		IsKeychainTTY:       func(*os.File) bool { return true },
		UnlockKeychain:      func(*os.File) error { return errors.New("unlock failed") },
	}
	_, err = application.lookupReviewCredential(service, "gitea", "git.example.com")
	if err == nil || !strings.Contains(err.Error(), "unlock macOS login Keychain") {
		t.Fatalf("unexpected error: %v", err)
	}
	if service.lookupCalls != 1 {
		t.Fatalf("lookup retried after unlock failure: %d", service.lookupCalls)
	}
}

func TestReviewCredentialDoesNotUnlockForNonTTYOrJSON(t *testing.T) {
	for _, test := range []struct {
		name  string
		allow bool
		tty   bool
	}{
		{name: "non TTY", allow: true, tty: false},
		{name: "JSON", allow: false, tty: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			terminal, err := os.Open(os.DevNull)
			if err != nil {
				t.Fatal(err)
			}
			defer terminal.Close()
			service := &retryAuthService{lookup: []error{auth.ErrKeychainInteractionRequired}}
			unlocks := 0
			application := &Application{
				IO:                  IO{InFile: terminal, ErrOut: &bytes.Buffer{}},
				allowKeychainUnlock: test.allow,
				KeychainSupported:   func() bool { return true },
				IsKeychainTTY:       func(*os.File) bool { return test.tty },
				UnlockKeychain:      func(*os.File) error { unlocks++; return nil },
			}
			_, err = application.lookupReviewCredential(service, "gitea", "git.example.com")
			if err == nil || !strings.Contains(err.Error(), "security unlock-keychain") {
				t.Fatalf("unexpected guidance: %v", err)
			}
			if unlocks != 0 || service.lookupCalls != 1 {
				t.Fatalf("unexpected subprocess/retry: unlocks=%d lookupCalls=%d", unlocks, service.lookupCalls)
			}
		})
	}
}

func TestReviewSubmitJSONFormsDoNotUnlockKeychain(t *testing.T) {
	terminal, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer terminal.Close()
	for _, args := range [][]string{{"submit", "--json"}, {"--json", "submit"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			if !argumentsContainJSON(args) {
				t.Fatalf("JSON flag was not recognized in %v", args)
			}
			service := &retryAuthService{lookup: []error{auth.ErrKeychainInteractionRequired}}
			unlocks := 0
			application := &Application{
				IO:                  IO{InFile: terminal, ErrOut: &bytes.Buffer{}},
				allowKeychainUnlock: !argumentsContainJSON(args),
				KeychainSupported:   func() bool { return true },
				IsKeychainTTY:       func(*os.File) bool { return true },
				UnlockKeychain:      func(*os.File) error { unlocks++; return nil },
			}
			_, lookupErr := application.lookupReviewCredential(service, "gitea", "git.example.com")
			if lookupErr == nil || unlocks != 0 || service.lookupCalls != 1 {
				t.Fatalf("JSON review submit attempted unlock/retry: unlocks=%d lookups=%d err=%v", unlocks, service.lookupCalls, lookupErr)
			}
		})
	}
}

func TestReviewCredentialReturnsSecondLookupFailure(t *testing.T) {
	terminal, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer terminal.Close()
	retryErr := errors.New("still locked")
	service := &retryAuthService{lookup: []error{auth.ErrKeychainInteractionRequired, retryErr}}
	application := &Application{
		IO:                  IO{InFile: terminal, ErrOut: &bytes.Buffer{}},
		allowKeychainUnlock: true,
		KeychainSupported:   func() bool { return true },
		IsKeychainTTY:       func(*os.File) bool { return true },
		UnlockKeychain:      func(*os.File) error { return nil },
	}
	_, err = application.lookupReviewCredential(service, "gitea", "git.example.com")
	if !errors.Is(err, retryErr) || service.lookupCalls != 2 {
		t.Fatalf("second lookup error was not preserved: err=%v calls=%d", err, service.lookupCalls)
	}
}
