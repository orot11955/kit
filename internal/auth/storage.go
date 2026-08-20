package auth

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const (
	maxIndexBytes  = 1 << 20
	maxSecretBytes = 64 << 10
	maxTokenBytes  = 32 << 10
)

type indexFile struct {
	Version  int       `json:"version"`
	Profiles []Profile `json:"profiles"`
}

type secretFile struct {
	Token string `json:"token"`
}

func defaultConfigDir() (string, error) {
	root, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve user config directory: %w", err)
	}
	return filepath.Join(root, "kit", "auth"), nil
}

func (m *Manager) readIndex() ([]Profile, error) {
	if err := checkSecureDirIfPresent(m.configDir); err != nil {
		return nil, fmt.Errorf("read credential metadata: %w", err)
	}
	path := filepath.Join(m.configDir, "profiles.json")
	data, exists, err := readSecureFile(path, maxIndexBytes)
	if err != nil {
		return nil, fmt.Errorf("read credential metadata: %w", err)
	}
	if !exists {
		return []Profile{}, nil
	}
	var index indexFile
	if err := decodeStrict(data, &index); err != nil {
		return nil, fmt.Errorf("read credential metadata: %w", err)
	}
	if index.Version != 1 || index.Profiles == nil {
		return nil, errors.New("read credential metadata: unsupported or incomplete metadata format")
	}
	seen := make(map[string]struct{}, len(index.Profiles))
	for _, profile := range index.Profiles {
		provider, host, err := NormalizeProfile(profile.Provider, profile.Host)
		if err != nil || provider != profile.Provider || host != profile.Host || (profile.Store != StoreKeyring && profile.Store != StoreFile) {
			return nil, errors.New("read credential metadata: invalid profile")
		}
		key := provider + "\x00" + host
		if _, exists := seen[key]; exists {
			return nil, errors.New("read credential metadata: duplicate profile")
		}
		seen[key] = struct{}{}
	}
	return append([]Profile(nil), index.Profiles...), nil
}

func (m *Manager) writeIndex(profiles []Profile) error {
	data, err := json.Marshal(indexFile{Version: 1, Profiles: profiles})
	if err != nil {
		return err
	}
	return writeSecureAtomic(m.configDir, "profiles.json", append(data, '\n'))
}

func (m *Manager) readSecretFile(provider, host string) (string, bool, error) {
	path := filepath.Join(m.configDir, secretFilename(provider, host))
	data, exists, err := readSecureFile(path, maxSecretBytes)
	if err != nil || !exists {
		return "", exists, err
	}
	var secret secretFile
	if err := decodeStrict(data, &secret); err != nil {
		return "", true, fmt.Errorf("read file credential: %w", err)
	}
	if secret.Token == "" {
		return "", true, errors.New("read file credential: token is empty")
	}
	return secret.Token, true, nil
}

func (m *Manager) writeSecretFile(provider, host, token string) error {
	data, err := json.Marshal(secretFile{Token: token})
	if err != nil {
		return err
	}
	return writeSecureAtomic(m.configDir, secretFilename(provider, host), append(data, '\n'))
}

func (m *Manager) removeSecretFile(provider, host string) error {
	path := filepath.Join(m.configDir, secretFilename(provider, host))
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("credential path is not a regular file")
	}
	return os.Remove(path)
}

func secretFilename(provider, host string) string {
	digest := sha256.Sum256([]byte(provider + "\x00" + host))
	return "credential-" + hex.EncodeToString(digest[:]) + ".json"
}

func readSecureFile(path string, limit int64) ([]byte, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, false, errors.New("path is not a regular file")
	}
	if info.Mode().Perm() != 0o600 {
		return nil, false, fmt.Errorf("unsafe file permissions %04o; expected 0600", info.Mode().Perm())
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, false, err
	}
	if !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) {
		return nil, false, errors.New("path changed while opening secure file")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(data)) > limit {
		return nil, false, errors.New("file exceeds size limit")
	}
	return data, true, nil
}

func decodeStrict(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values are not allowed")
		}
		return err
	}
	return nil
}

func writeSecureAtomic(dir, name string, data []byte) error {
	if err := ensureSecureDir(dir); err != nil {
		return err
	}
	target := filepath.Join(dir, name)
	if info, err := os.Lstat(target); err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("target path is not a regular file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	temporary, err := os.CreateTemp(dir, ".kit-auth-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, target); err != nil {
		return err
	}
	directory, err := os.Open(dir)
	if err == nil {
		err = directory.Sync()
		_ = directory.Close()
	}
	return err
}

func ensureSecureDir(dir string) error {
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		info, err = os.Lstat(dir)
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("auth config path is not a regular directory")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	return nil
}

func checkSecureDirIfPresent(dir string) error {
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("auth config path is not a regular directory")
	}
	if info.Mode().Perm() != 0o700 {
		return fmt.Errorf("unsafe directory permissions %04o; expected 0700", info.Mode().Perm())
	}
	return nil
}
