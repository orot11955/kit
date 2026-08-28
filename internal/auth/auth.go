package auth

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"sync"

	keyring "github.com/zalando/go-keyring"
)

const ProviderGitea = "gitea"

type Store string

const (
	StoreAuto    Store = "auto"
	StoreKeyring Store = "keyring"
	StoreFile    Store = "file"
)

var (
	ErrCredentialNotFound          = errors.New("credential not found")
	ErrKeyringUnavailable          = errors.New("keyring is unavailable")
	ErrKeychainInteractionRequired = errors.New("macOS Keychain interaction required")
)

// Profile is safe to serialize for status and list output. It intentionally
// has no credential or token field.
type Profile struct {
	Provider string `json:"provider"`
	Host     string `json:"host"`
	Store    Store  `json:"store"`
}

type Backend interface {
	Set(service, user, secret string) error
	Get(service, user string) (string, error)
	Delete(service, user string) error
}

type keyringBackend struct{}

func (keyringBackend) Set(service, user, secret string) error {
	if runtime.GOOS == "darwin" {
		return setDarwinKeychain(service, user, secret, runDarwinSecurityCommand, keyring.Get)
	}
	return keyring.Set(service, user, secret)
}
func (keyringBackend) Get(service, user string) (string, error) { return keyring.Get(service, user) }
func (keyringBackend) Delete(service, user string) error        { return keyring.Delete(service, user) }

const darwinEncodingPrefix = "go-keyring-base64:"

type keychainGet func(service, user string) (string, error)
type securityCommandRunner func(command string) error

func setDarwinKeychain(service, user, secret string, run securityCommandRunner, get keychainGet) error {
	encoded := darwinEncodingPrefix + base64.StdEncoding.EncodeToString([]byte(secret))
	update := fmt.Sprintf("add-generic-password -U -s %s -a %s -w %s", securityQuote(service), securityQuote(user), securityQuote(encoded))
	err := run(update)
	if err == nil {
		return nil
	}
	if stored, getErr := get(service, user); getErr == nil && stored == secret {
		return nil
	}
	if !exitCodeIs(err, 36) {
		return err
	}

	// Some macOS versions return 36 when -U is used for an item that does not
	// exist. Retry once as an explicit create, keeping the secret on stdin and
	// out of the process argument list.
	create := fmt.Sprintf("add-generic-password -s %s -a %s -w %s", securityQuote(service), securityQuote(user), securityQuote(encoded))
	if createErr := run(create); createErr != nil {
		return fmt.Errorf("update failed: %v; create retry failed: %w", err, createErr)
	}
	stored, getErr := get(service, user)
	if getErr != nil {
		return fmt.Errorf("verify created Keychain item: %w", getErr)
	}
	if stored != secret {
		return errors.New("verify created Keychain item: stored value differs")
	}
	return nil
}

func runDarwinSecurityCommand(command string) error {
	if len(command) > 4096 {
		return errors.New("Keychain command exceeds size limit")
	}
	cmd := exec.Command("/usr/bin/security", "-i")
	cmd.Stdin = strings.NewReader(command + "\n")
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd.Run()
}

func securityQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func exitCodeIs(err error, code int) bool {
	type exitCoder interface{ ExitCode() int }
	var coded exitCoder
	return errors.As(err, &coded) && coded.ExitCode() == code
}

type Manager struct {
	configDir string
	goos      string
	keyring   Backend
	mu        sync.Mutex
}

func NewDefault() (*Manager, error) {
	dir, err := defaultConfigDir()
	if err != nil {
		return nil, err
	}
	return NewManager(dir, runtime.GOOS, keyringBackend{}), nil
}

func NewManager(configDir, goos string, backend Backend) *Manager {
	return &Manager{configDir: configDir, goos: goos, keyring: backend}
}

func (m *Manager) ConfigDir() string { return m.configDir }

func ParseStore(value string) (Store, error) {
	store := Store(value)
	switch store {
	case StoreAuto, StoreKeyring, StoreFile:
		return store, nil
	default:
		return "", fmt.Errorf("store must be one of auto, keyring, or file")
	}
}

func NormalizeProfile(provider, host string) (string, string, error) {
	provider = strings.ToLower(provider)
	if provider != ProviderGitea {
		return "", "", fmt.Errorf("unsupported auth provider %q", provider)
	}
	if host == "" || host != strings.ToLower(host) || strings.ContainsAny(host, "/?#@") {
		return "", "", errors.New("host must be exact lowercase host[:port] without a scheme or path")
	}
	parsed, err := url.Parse("https://" + host)
	if err != nil || parsed.Host != host || parsed.Hostname() == "" || parsed.User != nil {
		return "", "", errors.New("host must be a valid exact lowercase host[:port]")
	}
	return provider, host, nil
}

func (m *Manager) Login(provider, host, token string, requested Store) (Profile, error) {
	provider, host, err := NormalizeProfile(provider, host)
	if err != nil {
		return Profile{}, err
	}
	if token == "" {
		return Profile{}, errors.New("token must not be empty")
	}
	if len(token) > maxTokenBytes {
		return Profile{}, errors.New("token exceeds size limit")
	}
	if requested == StoreAuto {
		requested = StoreKeyring
	}
	if requested != StoreKeyring && requested != StoreFile {
		return Profile{}, errors.New("invalid credential store")
	}
	if requested == StoreFile && m.goos != "linux" && m.goos != "darwin" {
		return Profile{}, errors.New("file credential storage is only available as an explicit macOS or Linux fallback")
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	profiles, err := m.readIndex()
	if err != nil {
		return Profile{}, err
	}
	originalProfiles := append([]Profile(nil), profiles...)
	profile := Profile{Provider: provider, Host: host, Store: requested}
	previous, hadPrevious := findProfile(profiles, provider, host)

	var rollback func() error
	if requested == StoreKeyring {
		if m.keyring == nil {
			return Profile{}, errors.New("keyring backend is not configured")
		}
		old, getErr := m.keyring.Get(serviceName(provider), host)
		hadOld := getErr == nil
		if getErr != nil && !errors.Is(getErr, keyring.ErrNotFound) {
			return Profile{}, m.keyringError(getErr)
		}
		if err := m.keyring.Set(serviceName(provider), host, token); err != nil {
			return Profile{}, m.keyringError(err)
		}
		rollback = func() error {
			if hadOld {
				return m.keyring.Set(serviceName(provider), host, old)
			}
			err := m.keyring.Delete(serviceName(provider), host)
			if errors.Is(err, keyring.ErrNotFound) {
				return nil
			}
			return err
		}
	} else {
		old, existed, readErr := m.readSecretFile(provider, host)
		if readErr != nil {
			return Profile{}, readErr
		}
		if err := m.writeSecretFile(provider, host, token); err != nil {
			return Profile{}, err
		}
		rollback = func() error {
			if existed {
				return m.writeSecretFile(provider, host, old)
			}
			return m.removeSecretFile(provider, host)
		}
	}

	profiles = upsertProfile(profiles, profile)
	if err := m.writeIndex(profiles); err != nil {
		if rollbackErr := rollback(); rollbackErr != nil {
			return Profile{}, fmt.Errorf("write credential metadata: %v (credential rollback failed: %v)", err, rollbackErr)
		}
		return Profile{}, fmt.Errorf("write credential metadata: %w", err)
	}
	if hadPrevious && previous.Store != requested {
		if deleteErr := m.deleteSecret(previous); deleteErr != nil {
			// Keep the previously valid credential authoritative. If metadata cannot
			// be restored, leave the newly indexed credential intact rather than
			// creating an index that points at a credential we rolled back.
			if restoreErr := m.writeIndex(originalProfiles); restoreErr != nil {
				return Profile{}, fmt.Errorf("delete obsolete credential: %v (metadata restore failed: %v)", deleteErr, restoreErr)
			}
			if rollbackErr := rollback(); rollbackErr != nil {
				return Profile{}, fmt.Errorf("delete obsolete credential: %v (new credential rollback failed: %v)", deleteErr, rollbackErr)
			}
			return Profile{}, fmt.Errorf("delete obsolete credential: %w", deleteErr)
		}
	}
	return profile, nil
}

func (m *Manager) Lookup(provider, host string) (string, error) {
	provider, host, err := NormalizeProfile(provider, host)
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	profiles, err := m.readIndex()
	if err != nil {
		return "", err
	}
	profile, ok := findProfile(profiles, provider, host)
	if !ok {
		return "", ErrCredentialNotFound
	}
	token, err := m.readCredential(profile)
	if err != nil {
		return "", err
	}
	return token, nil
}

func (m *Manager) readCredential(profile Profile) (string, error) {
	if profile.Store == StoreKeyring {
		if m.keyring == nil {
			return "", errors.New("keyring backend is not configured")
		}
		token, err := m.keyring.Get(serviceName(profile.Provider), profile.Host)
		if errors.Is(err, keyring.ErrNotFound) {
			return "", ErrCredentialNotFound
		}
		if err != nil {
			return "", m.keyringError(err)
		}
		if token == "" {
			return "", ErrCredentialNotFound
		}
		return token, nil
	}
	token, exists, err := m.readSecretFile(profile.Provider, profile.Host)
	if err != nil {
		return "", err
	}
	if !exists || token == "" {
		return "", ErrCredentialNotFound
	}
	return token, nil
}

func (m *Manager) Status(provider, host string) (Profile, error) {
	provider, host, err := NormalizeProfile(provider, host)
	if err != nil {
		return Profile{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	profiles, err := m.readIndex()
	if err != nil {
		return Profile{}, err
	}
	profile, ok := findProfile(profiles, provider, host)
	if !ok {
		return Profile{}, ErrCredentialNotFound
	}
	if _, err := m.readCredential(profile); err != nil {
		return Profile{}, fmt.Errorf("credential metadata does not match storage: %w", err)
	}
	return profile, nil
}

func (m *Manager) List() ([]Profile, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	profiles, err := m.readIndex()
	if err != nil {
		return nil, err
	}
	sort.Slice(profiles, func(i, j int) bool {
		if profiles[i].Provider == profiles[j].Provider {
			return profiles[i].Host < profiles[j].Host
		}
		return profiles[i].Provider < profiles[j].Provider
	})
	return profiles, nil
}

func (m *Manager) Logout(provider, host string) (Profile, error) {
	provider, host, err := NormalizeProfile(provider, host)
	if err != nil {
		return Profile{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	profiles, err := m.readIndex()
	if err != nil {
		return Profile{}, err
	}
	profile, ok := findProfile(profiles, provider, host)
	if !ok {
		return Profile{}, ErrCredentialNotFound
	}
	remaining := removeProfile(profiles, provider, host)
	if err := m.writeIndex(remaining); err != nil {
		return Profile{}, err
	}
	if err := m.deleteSecret(profile); err != nil {
		if restoreErr := m.writeIndex(profiles); restoreErr != nil {
			return Profile{}, fmt.Errorf("delete credential: %v (metadata restore failed: %v)", err, restoreErr)
		}
		return Profile{}, fmt.Errorf("delete credential: %w", err)
	}
	return profile, nil
}

func serviceName(provider string) string { return "kit." + provider }

func findProfile(profiles []Profile, provider, host string) (Profile, bool) {
	for _, profile := range profiles {
		if profile.Provider == provider && profile.Host == host {
			return profile, true
		}
	}
	return Profile{}, false
}

func upsertProfile(profiles []Profile, replacement Profile) []Profile {
	result := make([]Profile, 0, len(profiles)+1)
	for _, profile := range profiles {
		if profile.Provider != replacement.Provider || profile.Host != replacement.Host {
			result = append(result, profile)
		}
	}
	return append(result, replacement)
}

func removeProfile(profiles []Profile, provider, host string) []Profile {
	result := make([]Profile, 0, len(profiles))
	for _, profile := range profiles {
		if profile.Provider != provider || profile.Host != host {
			result = append(result, profile)
		}
	}
	return result
}

func (m *Manager) deleteSecret(profile Profile) error {
	if profile.Store == StoreKeyring {
		err := m.keyring.Delete(serviceName(profile.Provider), profile.Host)
		if err == nil {
			return nil
		}
		if errors.Is(err, keyring.ErrNotFound) {
			return nil
		}
		return m.keyringError(err)
	}
	return m.removeSecretFile(profile.Provider, profile.Host)
}

func (m *Manager) keyringError(err error) error {
	if errors.Is(err, ErrKeyringUnavailable) {
		return err
	}
	if m.goos == "linux" && keyringUnavailable(err) {
		return fmt.Errorf("%w: %v", ErrKeyringUnavailable, err)
	}
	if m.goos == "darwin" {
		if exitCodeIs(err, 36) {
			return fmt.Errorf("%w: %v", ErrKeychainInteractionRequired, err)
		}
		return fmt.Errorf("macOS Keychain operation failed: %w", err)
	}
	return fmt.Errorf("keyring operation failed: %w", err)
}

func keyringUnavailable(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	for _, marker := range []string{
		"couldn't determine address of session bus",
		"org.freedesktop.DBus.Error.ServiceUnknown",
		"org.freedesktop.DBus.Error.NameHasNoOwner",
		"org.freedesktop.DBus.Error.Spawn.ServiceNotFound",
		"org.freedesktop.secrets was not provided by any .service files",
		"org.freedesktop.secrets does not exist",
		"connect: no such file or directory",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}
