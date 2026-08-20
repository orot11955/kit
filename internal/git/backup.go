package git

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"time"
)

type WorkBackupKind string

const (
	WorkBackupAuto          WorkBackupKind = "auto"
	WorkBackupManual        WorkBackupKind = "manual"
	WorkBackupBeforeRestore WorkBackupKind = "before-restore"

	workBackupRoot = "kit/backup"
)

var (
	workBackupShortHashPattern = regexp.MustCompile("^[0-9a-f]{8}$")
	workBackupTimedIDPattern   = regexp.MustCompile("^([0-9]{8}-[0-9]{6})-([0-9a-f]{8})$")
	legacyAutoPattern          = regexp.MustCompile("^kit/backup/work-([0-9]{8}-[0-9]{6})-([0-9a-f]{8})$")
	legacyManualPattern        = regexp.MustCompile("^kit/backup/work-manual-([0-9a-f]{8})$")
	legacyBeforeRestorePattern = regexp.MustCompile("^kit/backup/work-before-restore-([0-9]{8}-[0-9]{6})-([0-9a-f]{8})$")
)

type WorkBackupRef struct {
	Name   string
	Source string
	Kind   WorkBackupKind
	ID     string
	Legacy bool
}

func FormatWorkBackupRef(source string, kind WorkBackupKind, id string) (string, error) {
	if source == "" {
		return "", fmt.Errorf("backup source must not be empty")
	}
	if !validWorkBackupID(kind, id) {
		return "", fmt.Errorf("invalid %s work backup id %q", kind, id)
	}
	sum := sha256.Sum256([]byte(source))
	return fmt.Sprintf("%s/v2/%s/%s/%s", workBackupRoot, hex.EncodeToString(sum[:]), kind, id), nil
}

func ParseWorkBackupRef(name, source string) (WorkBackupRef, bool) {
	if source == "" {
		return WorkBackupRef{}, false
	}
	parts := strings.Split(name, "/")
	if len(parts) == 6 && parts[0] == "kit" && parts[1] == "backup" && parts[2] == "v2" {
		sum := sha256.Sum256([]byte(source))
		if parts[3] != hex.EncodeToString(sum[:]) {
			return WorkBackupRef{}, false
		}
		kind := WorkBackupKind(parts[4])
		if !validWorkBackupID(kind, parts[5]) {
			return WorkBackupRef{}, false
		}
		return WorkBackupRef{Name: name, Source: source, Kind: kind, ID: parts[5]}, true
	}
	if source != "work" {
		return WorkBackupRef{}, false
	}
	if match := legacyAutoPattern.FindStringSubmatch(name); match != nil && validBackupTimestamp(match[1]) {
		return WorkBackupRef{Name: name, Source: source, Kind: WorkBackupAuto, ID: match[1] + "-" + match[2], Legacy: true}, true
	}
	if match := legacyManualPattern.FindStringSubmatch(name); match != nil {
		return WorkBackupRef{Name: name, Source: source, Kind: WorkBackupManual, ID: match[1], Legacy: true}, true
	}
	if match := legacyBeforeRestorePattern.FindStringSubmatch(name); match != nil && validBackupTimestamp(match[1]) {
		return WorkBackupRef{Name: name, Source: source, Kind: WorkBackupBeforeRestore, ID: match[1] + "-" + match[2], Legacy: true}, true
	}
	return WorkBackupRef{}, false
}

func validWorkBackupID(kind WorkBackupKind, id string) bool {
	switch kind {
	case WorkBackupManual:
		return workBackupShortHashPattern.MatchString(id)
	case WorkBackupAuto, WorkBackupBeforeRestore:
		match := workBackupTimedIDPattern.FindStringSubmatch(id)
		return match != nil && validBackupTimestamp(match[1])
	default:
		return false
	}
}

func validBackupTimestamp(value string) bool {
	parsed, err := time.Parse("20060102-150405", value)
	return err == nil && parsed.UTC().Format("20060102-150405") == value
}
