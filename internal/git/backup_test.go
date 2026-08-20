package git

import (
	"strings"
	"testing"
)

func TestWorkBackupV2RoundTripAndExactSourceOwnership(t *testing.T) {
	name, err := FormatWorkBackupRef("a/b", WorkBackupAuto, "20260820-120000-abcdef12")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(name, "kit/backup/v2/") || strings.Contains(name, "a-b") {
		t.Fatalf("unexpected v2 name: %s", name)
	}
	parsed, ok := ParseWorkBackupRef(name, "a/b")
	if !ok || parsed.Kind != WorkBackupAuto || parsed.ID != "20260820-120000-abcdef12" || parsed.Legacy {
		t.Fatalf("unexpected parsed ref: %#v ok=%v", parsed, ok)
	}
	if _, ok := ParseWorkBackupRef(name, "a-b"); ok {
		t.Fatal("source with the same legacy-sanitized form claimed a/b backup")
	}
}

func TestWorkBackupParserRejectsMalformedAndPrefixSpoofedRefs(t *testing.T) {
	valid, err := FormatWorkBackupRef("work", WorkBackupManual, "abcdef12")
	if err != nil {
		t.Fatal(err)
	}
	cases := []string{
		valid + "-suffix",
		valid + "/suffix",
		strings.Replace(valid, "/manual/", "/unknown/", 1),
		strings.Replace(valid, "abcdef12", "ABCDEF12", 1),
		strings.Replace(valid, "abcdef12", "abcdef1", 1),
	}
	for _, candidate := range cases {
		if _, ok := ParseWorkBackupRef(candidate, "work"); ok {
			t.Errorf("accepted malformed backup ref %q", candidate)
		}
	}
}

func TestWorkBackupLegacyCompatibilityIsRestrictedToExactWork(t *testing.T) {
	cases := map[string]WorkBackupKind{
		"kit/backup/work-20260820-120000-abcdef12":                WorkBackupAuto,
		"kit/backup/work-manual-abcdef12":                         WorkBackupManual,
		"kit/backup/work-before-restore-20260820-120000-abcdef12": WorkBackupBeforeRestore,
	}
	for name, kind := range cases {
		parsed, ok := ParseWorkBackupRef(name, "work")
		if !ok || !parsed.Legacy || parsed.Kind != kind {
			t.Errorf("legacy ref was not accepted: %s %#v ok=%v", name, parsed, ok)
		}
		if _, ok := ParseWorkBackupRef(name, "work/topic"); ok {
			t.Errorf("non-work source claimed legacy ref %s", name)
		}
	}
	for _, malformed := range []string{
		"kit/backup/work-20261399-999999-abcdef12",
		"kit/backup/work-20260820-120000-abcdef12-extra",
		"kit/backup/work-manual-abcdef12-extra",
		"kit/backup/work-before-restore-20260820-120000-ABCDEF12",
	} {
		if _, ok := ParseWorkBackupRef(malformed, "work"); ok {
			t.Errorf("accepted malformed legacy ref %s", malformed)
		}
	}
}
