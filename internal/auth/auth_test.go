package auth

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	keyring "github.com/zalando/go-keyring"
)

type memoryBackend struct {
	values    map[string]string
	getErr    error
	setErr    error
	deleteErr error
	afterSet  func()
}

func newMemoryBackend() *memoryBackend { return &memoryBackend{values: map[string]string{}} }

func (b *memoryBackend) key(service, user string) string { return service + "\x00" + user }

func (b *memoryBackend) Set(service, user, secret string) error {
	if b.setErr != nil {
		return b.setErr
	}
	b.values[b.key(service, user)] = secret
	if b.afterSet != nil {
		b.afterSet()
	}
	return nil
}

func (b *memoryBackend) Get(service, user string) (string, error) {
	if b.getErr != nil {
		return "", b.getErr
	}
	value, ok := b.values[b.key(service, user)]
	if !ok {
		return "", keyring.ErrNotFound
	}
	return value, nil
}

func (b *memoryBackend) Delete(service, user string) error {
	if b.deleteErr != nil {
		return b.deleteErr
	}
	key := b.key(service, user)
	if _, ok := b.values[key]; !ok {
		return keyring.ErrNotFound
	}
	delete(b.values, key)
	return nil
}

func TestManagerKeyringProfilesAreExactAndTokenless(t *testing.T) {
	backend := newMemoryBackend()
	manager := NewManager(filepath.Join(t.TempDir(), "auth"), "darwin", backend)
	first, err := manager.Login("gitea", "git.example.com", "first-secret", StoreAuto)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Login("gitea", "git.example.com:8443", "second-secret", StoreKeyring); err != nil {
		t.Fatal(err)
	}
	if first.Store != StoreKeyring {
		t.Fatalf("auto did not use keyring: %#v", first)
	}
	profiles, err := manager.List()
	if err != nil || len(profiles) != 2 {
		t.Fatalf("unexpected profiles: %#v, %v", profiles, err)
	}
	encoded, err := json.Marshal(profiles)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "secret") || strings.Contains(string(encoded), "token") {
		t.Fatalf("credential reached list model: %s", encoded)
	}
	if got, err := manager.Lookup("gitea", "git.example.com"); err != nil || got != "first-secret" {
		t.Fatalf("unexpected lookup: %q, %v", got, err)
	}
	if _, _, err := NormalizeProfile("gitea", "Git.Example.com"); err == nil {
		t.Fatal("uppercase host was accepted")
	}
}

func TestFileFallbackUsesSecureFilesAndLogoutIsProfileScoped(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "auth")
	manager := NewManager(dir, "linux", newMemoryBackend())
	if _, err := manager.Login("gitea", "one.example.com", "one-token", StoreFile); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Login("gitea", "two.example.com", "two-token", StoreFile); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("unsafe config directory: %v, %v", info, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || info.Mode().Perm() != 0o600 || !info.Mode().IsRegular() {
			t.Fatalf("unsafe auth file %s: %v, %v", entry.Name(), info, err)
		}
	}
	if _, err := manager.Logout("gitea", "one.example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Lookup("gitea", "one.example.com"); !errors.Is(err, ErrCredentialNotFound) {
		t.Fatalf("deleted profile still resolves: %v", err)
	}
	if got, err := manager.Lookup("gitea", "two.example.com"); err != nil || got != "two-token" {
		t.Fatalf("other profile was affected: %q, %v", got, err)
	}
}

func TestFileFallbackSupportsOfficialDesktopTargets(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		t.Run(goos, func(t *testing.T) {
			manager := NewManager(filepath.Join(t.TempDir(), "auth"), goos, newMemoryBackend())
			if _, err := manager.Login("gitea", "git.example.com", "secret", StoreFile); err != nil {
				t.Fatalf("explicit %s file fallback was rejected: %v", goos, err)
			}
		})
	}
	manager := NewManager(filepath.Join(t.TempDir(), "auth"), "windows", newMemoryBackend())
	if _, err := manager.Login("gitea", "git.example.com", "secret", StoreFile); err == nil {
		t.Fatal("unsupported OS file fallback was accepted")
	}
}

func TestKeyringLogoutTreatsSuccessfulDeleteAsSuccess(t *testing.T) {
	backend := newMemoryBackend()
	manager := NewManager(filepath.Join(t.TempDir(), "auth"), "darwin", backend)
	if _, err := manager.Login("gitea", "git.example.com", "secret", StoreKeyring); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Logout("gitea", "git.example.com"); err != nil {
		t.Fatalf("successful keyring delete became an error: %v", err)
	}
	if _, err := manager.Status("gitea", "git.example.com"); !errors.Is(err, ErrCredentialNotFound) {
		t.Fatalf("logged-out profile still exists: %v", err)
	}
}

func TestStatusDetectsMetadataCredentialMismatch(t *testing.T) {
	backend := newMemoryBackend()
	manager := NewManager(filepath.Join(t.TempDir(), "auth"), "linux", backend)
	if _, err := manager.Login("gitea", "git.example.com", "secret", StoreKeyring); err != nil {
		t.Fatal(err)
	}
	delete(backend.values, backend.key("kit.gitea", "git.example.com"))
	if _, err := manager.Status("gitea", "git.example.com"); !errors.Is(err, ErrCredentialNotFound) {
		t.Fatalf("status accepted missing credential: %v", err)
	}
}

func TestStoreTransitionRestoresOldProfileWhenObsoleteDeleteFails(t *testing.T) {
	backend := newMemoryBackend()
	dir := filepath.Join(t.TempDir(), "auth")
	manager := NewManager(dir, "linux", backend)
	if _, err := manager.Login("gitea", "git.example.com", "keyring-secret", StoreKeyring); err != nil {
		t.Fatal(err)
	}
	backend.deleteErr = errors.New("keyring is locked")
	if _, err := manager.Login("gitea", "git.example.com", "file-secret", StoreFile); err == nil {
		t.Fatal("obsolete backend deletion failure was hidden")
	}
	profile, err := manager.Status("gitea", "git.example.com")
	if err != nil || profile.Store != StoreKeyring {
		t.Fatalf("old profile was not restored: %#v, %v", profile, err)
	}
	if got, err := manager.Lookup("gitea", "git.example.com"); err != nil || got != "keyring-secret" {
		t.Fatalf("old credential was not preserved: %q, %v", got, err)
	}
	secretPath := filepath.Join(dir, secretFilename("gitea", "git.example.com"))
	if _, err := os.Stat(secretPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("new file credential was not rolled back: %v", err)
	}
}

func TestMetadataRejectsUnknownFieldsSymlinksAndOversize(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, dir string)
	}{
		{
			name: "unknown field",
			setup: func(t *testing.T, dir string) {
				writeAuthFixture(t, dir, []byte(`{"version":1,"profiles":[],"extra":true}`))
			},
		},
		{
			name: "oversize",
			setup: func(t *testing.T, dir string) {
				writeAuthFixture(t, dir, []byte(strings.Repeat("x", maxIndexBytes+1)))
			},
		},
		{
			name: "symlink",
			setup: func(t *testing.T, dir string) {
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				target := filepath.Join(t.TempDir(), "outside.json")
				if err := os.WriteFile(target, []byte(`{"version":1,"profiles":[]}`), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, filepath.Join(dir, "profiles.json")); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "auth")
			test.setup(t, dir)
			manager := NewManager(dir, "linux", newMemoryBackend())
			if _, err := manager.List(); err == nil {
				t.Fatal("unsafe metadata was accepted")
			}
		})
	}
}

func TestKeyringWriteRollsBackWhenMetadataWriteFails(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "auth")
	backend := newMemoryBackend()
	backend.afterSet = func() {
		// readIndex already completed. Replacing the future config directory with
		// a regular file makes the metadata write fail deterministically.
		_ = os.WriteFile(dir, []byte("blocked"), 0o600)
	}
	manager := NewManager(dir, "linux", backend)
	_, err := manager.Login("gitea", "git.example.com", "must-rollback", StoreKeyring)
	if err == nil {
		t.Fatal("metadata failure was not returned")
	}
	if len(backend.values) != 0 {
		t.Fatalf("keyring credential was not rolled back: %#v", backend.values)
	}
}

func TestOnlyUnavailableKeyringErrorsEnableFallbackClassification(t *testing.T) {
	manager := NewManager(filepath.Join(t.TempDir(), "auth"), "linux", &memoryBackend{values: map[string]string{}, getErr: errors.New("dbus: couldn't determine address of session bus")})
	_, err := manager.Login("gitea", "git.example.com", "secret", StoreAuto)
	if !errors.Is(err, ErrKeyringUnavailable) {
		t.Fatalf("unavailable backend was not classified: %v", err)
	}

	for _, backendErr := range []error{errors.New("collection is locked"), errors.New("permission denied"), errors.New("corrupt keyring response")} {
		manager := NewManager(filepath.Join(t.TempDir(), "auth"), "linux", &memoryBackend{values: map[string]string{}, getErr: backendErr})
		_, err := manager.Login("gitea", "git.example.com", "secret", StoreAuto)
		if errors.Is(err, ErrKeyringUnavailable) {
			t.Fatalf("unsafe fallback classification for %v", backendErr)
		}
	}
}

type fakeExitError int

func (e fakeExitError) Error() string { return "exit status" }
func (e fakeExitError) ExitCode() int { return int(e) }

func TestDarwinKeychainRetriesExit36AsCreateAndVerifies(t *testing.T) {
	const secret = "plain-secret-must-not-be-in-command"
	var commands []string
	created := false
	run := func(command string) error {
		commands = append(commands, command)
		if len(commands) == 1 {
			return fakeExitError(36)
		}
		created = true
		return nil
	}
	get := func(_, _ string) (string, error) {
		if !created {
			return "", keyring.ErrNotFound
		}
		return secret, nil
	}
	if err := setDarwinKeychain("kit.gitea", "git.example.com", secret, run, get); err != nil {
		t.Fatal(err)
	}
	if len(commands) != 2 || !strings.Contains(commands[0], " -U ") || strings.Contains(commands[1], " -U ") {
		t.Fatalf("unexpected update/create sequence: %#v", commands)
	}
	for _, command := range commands {
		if strings.Contains(command, secret) {
			t.Fatal("raw token was included in the security command")
		}
	}
}

func TestDarwinKeychainDoesNotRetryOtherFailures(t *testing.T) {
	runs := 0
	err := setDarwinKeychain("kit.gitea", "git.example.com", "secret", func(string) error {
		runs++
		return fakeExitError(1)
	}, func(string, string) (string, error) {
		return "", keyring.ErrNotFound
	})
	if err == nil || runs != 1 {
		t.Fatalf("unexpected retry for non-36 failure: runs=%d err=%v", runs, err)
	}
}

func TestProfileTypeHasNoTokenField(t *testing.T) {
	typeOfProfile := reflect.TypeOf(Profile{})
	for i := 0; i < typeOfProfile.NumField(); i++ {
		name := strings.ToLower(typeOfProfile.Field(i).Name)
		if strings.Contains(name, "token") || strings.Contains(name, "secret") || strings.Contains(name, "credential") {
			t.Fatalf("unsafe Profile field: %s", typeOfProfile.Field(i).Name)
		}
	}
}

func writeAuthFixture(t *testing.T, dir string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "profiles.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}
