package system

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

var (
	ErrVoicemailPathTraversal = errors.New("voicemail path traversal rejected")
	ErrVoicemailInvalidAsset  = errors.New("invalid voicemail asset")
)

type VoicemailMessage struct {
	ID           string `json:"id"`
	Date         string `json:"date,omitempty"`
	UnixTime     int64  `json:"unix_time,omitempty"`
	Read         bool   `json:"read"`
	HasThumbnail bool   `json:"has_thumbnail"`
	HasVideo     bool   `json:"has_video"`
}

func ListVoicemailMessages(baseDir string) ([]VoicemailMessage, error) {
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return []VoicemailMessage{}, nil
		}
		return nil, err
	}

	out := make([]VoicemailMessage, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		msgID := strings.TrimSpace(entry.Name())
		if msgID == "" {
			continue
		}
		msgPath := filepath.Join(baseDir, msgID)
		infoPath := filepath.Join(msgPath, "msg_info.ini")
		info, _ := parseINISection(infoPath, "Message Information")

		unixTime, _ := strconv.ParseInt(strings.TrimSpace(info["UnixTime"]), 10, 64)
		out = append(out, VoicemailMessage{
			ID:           msgID,
			Date:         strings.TrimSpace(info["Date"]),
			UnixTime:     unixTime,
			Read:         strings.TrimSpace(info["Read"]) == "1",
			HasThumbnail: fileExists(filepath.Join(msgPath, "aswm.jpg")),
			HasVideo:     fileExists(filepath.Join(msgPath, "aswm.avi")),
		})
	}

	sort.Slice(out, func(i, j int) bool {
		return out[i].UnixTime < out[j].UnixTime
	})
	return out, nil
}

func VoicemailAssetPath(baseDir string, messageID string, asset string) (string, string, error) {
	baseDir = strings.TrimSpace(baseDir)
	if baseDir == "" {
		return "", "", ErrVoicemailPathTraversal
	}
	msg := strings.TrimSpace(messageID)
	if msg == "" || strings.Contains(msg, "/") || strings.Contains(msg, "\\") || strings.Contains(msg, "..") {
		return "", "", ErrVoicemailPathTraversal
	}
	asset = strings.TrimSpace(asset)

	contentType := ""
	switch asset {
	case "aswm.jpg":
		contentType = "image/jpeg"
	case "aswm.avi":
		contentType = "video/x-msvideo"
	default:
		return "", "", ErrVoicemailInvalidAsset
	}

	full := filepath.Join(baseDir, msg, asset)
	rel, err := filepath.Rel(baseDir, full)
	if err != nil {
		return "", "", fmt.Errorf("%w: %v", ErrVoicemailPathTraversal, err)
	}
	if strings.HasPrefix(rel, "..") {
		return "", "", ErrVoicemailPathTraversal
	}
	return full, contentType, nil
}

func parseINISection(path string, section string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	out := map[string]string{}
	inSection := false
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, ";") || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			name := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "["), "]"))
			inSection = name == section
			continue
		}
		if !inSection {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
