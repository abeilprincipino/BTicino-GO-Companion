package system

import (
	"os"
	"path/filepath"
	"testing"
)

func TestListVoicemailMessagesMissingDirReturnsEmpty(t *testing.T) {
	out, err := ListVoicemailMessages(filepath.Join(t.TempDir(), "missing"))
	if err != nil {
		t.Fatalf("ListVoicemailMessages returned error: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("expected empty list, got %+v", out)
	}
}

func TestListVoicemailMessagesParsesAndSorts(t *testing.T) {
	base := t.TempDir()
	newerDir := filepath.Join(base, "1002")
	olderDir := filepath.Join(base, "1001")
	if err := os.MkdirAll(newerDir, 0o755); err != nil {
		t.Fatalf("mkdir newer: %v", err)
	}
	if err := os.MkdirAll(olderDir, 0o755); err != nil {
		t.Fatalf("mkdir older: %v", err)
	}

	newerInfo := "[Message Information]\nDate=2026-01-01\nUnixTime=200\nRead=1\n"
	olderInfo := "[Message Information]\nDate=2025-01-01\nUnixTime=100\nRead=0\n"
	if err := os.WriteFile(filepath.Join(newerDir, "msg_info.ini"), []byte(newerInfo), 0o644); err != nil {
		t.Fatalf("write newer info: %v", err)
	}
	if err := os.WriteFile(filepath.Join(olderDir, "msg_info.ini"), []byte(olderInfo), 0o644); err != nil {
		t.Fatalf("write older info: %v", err)
	}
	if err := os.WriteFile(filepath.Join(newerDir, "aswm.jpg"), []byte("jpg"), 0o644); err != nil {
		t.Fatalf("write newer jpg: %v", err)
	}
	if err := os.WriteFile(filepath.Join(olderDir, "aswm.avi"), []byte("avi"), 0o644); err != nil {
		t.Fatalf("write older avi: %v", err)
	}

	out, err := ListVoicemailMessages(base)
	if err != nil {
		t.Fatalf("ListVoicemailMessages returned error: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("expected 2 messages, got %d (%+v)", len(out), out)
	}
	if out[0].ID != "1001" || out[1].ID != "1002" {
		t.Fatalf("expected ascending order by unix time, got %+v", out)
	}
	if out[0].Read {
		t.Fatalf("expected first message unread, got %+v", out[0])
	}
	if !out[1].Read {
		t.Fatalf("expected second message read, got %+v", out[1])
	}
	if !out[0].HasVideo || out[0].HasThumbnail {
		t.Fatalf("unexpected assets for first message: %+v", out[0])
	}
	if !out[1].HasThumbnail || out[1].HasVideo {
		t.Fatalf("unexpected assets for second message: %+v", out[1])
	}
}

func TestVoicemailAssetPathValidationAndTypes(t *testing.T) {
	base := t.TempDir()

	if _, _, err := VoicemailAssetPath("", "1001", "aswm.jpg"); err != ErrVoicemailPathTraversal {
		t.Fatalf("expected path traversal error for empty base, got %v", err)
	}
	if _, _, err := VoicemailAssetPath(base, "../1001", "aswm.jpg"); err != ErrVoicemailPathTraversal {
		t.Fatalf("expected path traversal error for invalid message id, got %v", err)
	}
	if _, _, err := VoicemailAssetPath(base, "1001", "bad.file"); err != ErrVoicemailInvalidAsset {
		t.Fatalf("expected invalid asset error, got %v", err)
	}

	jpgPath, jpgType, err := VoicemailAssetPath(base, "1001", "aswm.jpg")
	if err != nil {
		t.Fatalf("valid jpg path failed: %v", err)
	}
	if jpgType != "image/jpeg" {
		t.Fatalf("unexpected jpg type: %s", jpgType)
	}
	if jpgPath != filepath.Join(base, "1001", "aswm.jpg") {
		t.Fatalf("unexpected jpg path: %s", jpgPath)
	}

	aviPath, aviType, err := VoicemailAssetPath(base, "1001", "aswm.avi")
	if err != nil {
		t.Fatalf("valid avi path failed: %v", err)
	}
	if aviType != "video/x-msvideo" {
		t.Fatalf("unexpected avi type: %s", aviType)
	}
	if aviPath != filepath.Join(base, "1001", "aswm.avi") {
		t.Fatalf("unexpected avi path: %s", aviPath)
	}
}

func TestParseINISectionAndFileExists(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "msg_info.ini")
	content := `
; comment
[Message Information]
UnixTime = 123
Read=1
ignored_without_equals

[Other]
UnixTime=999
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write ini: %v", err)
	}

	section, err := parseINISection(path, "Message Information")
	if err != nil {
		t.Fatalf("parseINISection failed: %v", err)
	}
	if section["UnixTime"] != "123" || section["Read"] != "1" {
		t.Fatalf("unexpected section values: %+v", section)
	}

	if _, err := parseINISection(filepath.Join(base, "missing.ini"), "Message Information"); err == nil {
		t.Fatal("expected error for missing ini file")
	}

	if !fileExists(path) {
		t.Fatalf("expected fileExists true for %s", path)
	}
	if fileExists(filepath.Join(base, "missing")) {
		t.Fatal("expected fileExists false for missing path")
	}
}

