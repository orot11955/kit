package update

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"kit/internal/buildinfo"
)

func installedExecutable(cfg Config) (string, string, error) {
	executable, err := filepath.EvalSymlinks(cfg.Executable)
	if err != nil {
		return "", "", fmt.Errorf("resolve current executable: %w", err)
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return "", "", err
	}
	expected, err := filepath.Abs(cfg.ExpectedPath)
	if err != nil {
		return "", "", err
	}
	expected, err = filepath.EvalSymlinks(expected)
	if err != nil {
		return "", "", fmt.Errorf("resolve expected install path: %w", err)
	}
	if executable != expected {
		return "", "", fmt.Errorf("self-update is only allowed for %s (currently running %s); reinstall with curl -fsSL https://kit.2juho.com/install.sh | sh", expected, executable)
	}
	installInfo, err := os.Lstat(cfg.ExpectedPath)
	if err != nil {
		return "", "", fmt.Errorf("inspect expected install path: %w", err)
	}
	if installInfo.Mode()&os.ModeSymlink != 0 {
		return "", "", fmt.Errorf("self-update refuses symlink installation %s; reinstall with curl -fsSL https://kit.2juho.com/install.sh | sh", cfg.ExpectedPath)
	}
	if !installInfo.Mode().IsRegular() {
		return "", "", fmt.Errorf("self-update requires a regular executable at %s", cfg.ExpectedPath)
	}
	return executable, expected, nil
}

func previousBinaryPath(cfg Config, expected string) (string, error) {
	path := cfg.PreviousPath
	if path == "" {
		path = expected + ".previous"
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if absolute == expected {
		return "", errors.New("previous binary path must differ from the installed executable")
	}
	if filepath.Dir(absolute) != filepath.Dir(expected) {
		return "", errors.New("previous binary must be stored next to the installed executable")
	}
	if info, err := os.Lstat(absolute); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return "", fmt.Errorf("previous binary path must be a regular non-symlink file: %s", absolute)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return absolute, nil
}

func preserveCurrentBinary(executable, previous string) error {
	temporary, err := unusedTempPath(filepath.Dir(executable), ".kit-previous-*")
	if err != nil {
		return err
	}
	removeTemporary := true
	defer func() {
		if removeTemporary {
			_ = os.Remove(temporary)
		}
	}()
	if err := os.Link(executable, temporary); err != nil {
		return fmt.Errorf("link current binary: %w", err)
	}
	if err := os.Rename(temporary, previous); err != nil {
		return fmt.Errorf("publish previous binary: %w", err)
	}
	removeTemporary = false
	return nil
}

func rollback(ctx context.Context, cfg Config) (Result, error) {
	executable, expected, err := installedExecutable(cfg)
	if err != nil {
		return Result{}, err
	}
	previous, err := previousBinaryPath(cfg, expected)
	if err != nil {
		return Result{}, err
	}
	if _, err := os.Lstat(previous); errors.Is(err, os.ErrNotExist) {
		return Result{}, fmt.Errorf("no previous kit binary is available at %s; rollback becomes available after a successful update made by this version", previous)
	} else if err != nil {
		return Result{}, err
	}
	previousInfo, err := cfg.RunVersion(ctx, previous)
	if err != nil {
		return Result{}, fmt.Errorf("verify previous binary: %w", err)
	}
	if err := validateRollbackInfo(previousInfo); err != nil {
		return Result{}, fmt.Errorf("verify previous binary: %w", err)
	}
	if cfg.Current.Version == previousInfo.Version && cfg.Current.Commit == previousInfo.Commit {
		return Result{}, errors.New("previous binary is identical to the current binary")
	}
	if err := exchangeInstalledBinaries(executable, previous); err != nil {
		return Result{}, err
	}
	installedInfo, verifyErr := cfg.RunVersion(ctx, executable)
	if verifyErr != nil || installedInfo.Version != previousInfo.Version || installedInfo.Commit != previousInfo.Commit || installedInfo.Target != previousInfo.Target {
		restoreErr := exchangeInstalledBinaries(executable, previous)
		if verifyErr != nil {
			return Result{}, fmt.Errorf("rollback post-verification failed: %v; restore current binary: %v", verifyErr, restoreErr)
		}
		return Result{}, fmt.Errorf("rollback post-verification returned unexpected binary metadata; restore current binary: %v", restoreErr)
	}
	return Result{
		Current:    cfg.Current.Version,
		Previous:   previousInfo.Version,
		RolledBack: true,
		Path:       executable,
	}, nil
}

func validateRollbackInfo(info buildinfo.Info) error {
	if _, err := parseSemver(info.Version); err != nil {
		return fmt.Errorf("invalid version %q: %w", info.Version, err)
	}
	if len(info.Commit) != 40 {
		return errors.New("previous binary has an invalid commit")
	}
	if _, err := hex.DecodeString(info.Commit); err != nil {
		return errors.New("previous binary has an invalid commit")
	}
	if _, err := time.Parse(time.RFC3339, info.BuildDate); err != nil {
		return errors.New("previous binary has an invalid build date")
	}
	expectedTarget := runtime.GOOS + "/" + runtime.GOARCH
	if info.Target != expectedTarget {
		return fmt.Errorf("previous binary target %s does not match %s", info.Target, expectedTarget)
	}
	return nil
}

func exchangeInstalledBinaries(executable, previous string) error {
	directory := filepath.Dir(executable)
	previousTemp, err := unusedTempPath(directory, ".kit-rollback-target-*")
	if err != nil {
		return err
	}
	currentTemp, err := unusedTempPath(directory, ".kit-rollback-current-*")
	if err != nil {
		return err
	}
	cleanupPrevious := true
	cleanupCurrent := true
	defer func() {
		if cleanupPrevious {
			_ = os.Remove(previousTemp)
		}
		if cleanupCurrent {
			_ = os.Remove(currentTemp)
		}
	}()
	if err := os.Link(previous, previousTemp); err != nil {
		return fmt.Errorf("stage previous binary: %w", err)
	}
	if err := os.Link(executable, currentTemp); err != nil {
		return fmt.Errorf("stage current binary: %w", err)
	}
	if err := os.Rename(currentTemp, previous); err != nil {
		return fmt.Errorf("rotate current binary into previous slot: %w", err)
	}
	cleanupCurrent = false
	if err := os.Rename(previousTemp, executable); err != nil {
		restoreErr := os.Rename(previousTemp, previous)
		if restoreErr == nil {
			cleanupPrevious = false
		}
		return fmt.Errorf("activate previous binary: %w; restore previous slot: %v", err, restoreErr)
	}
	cleanupPrevious = false
	return nil
}

func unusedTempPath(directory, pattern string) (string, error) {
	file, err := os.CreateTemp(directory, pattern)
	if err != nil {
		return "", err
	}
	name := file.Name()
	if err := file.Close(); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	if err := os.Remove(name); err != nil {
		return "", err
	}
	return name, nil
}

func normalizeVersion(value string) string {
	return strings.TrimSpace(value)
}
