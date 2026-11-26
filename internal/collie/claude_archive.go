package collie

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func (s *Server) claudeArchive(ctx context.Context, id string, archived bool) (string, error) {
	path, err := claudeArchiveIndex(s.claudeDesktopDirs(), id)
	if err != nil {
		return "", err
	}
	if path == "" {
		return "Claude Code 命令行没有会话归档。", nil
	}
	// Claude Desktop owns this file and can rewrite its in-memory copy.
	// Change the record only when the app has released it.
	out, err := exec.CommandContext(ctx, "ps", "-axww", "-o", "comm=").Output()
	if err != nil {
		return "", fmt.Errorf("无法确认 Claude 桌面版是否在运行：%w", err)
	}
	if strings.Contains(string(out), "/Claude.app/Contents/") || strings.Contains(string(out), "/Claude-3p.app/Contents/") {
		return "", errors.New("Claude 桌面版正在运行，请在桌面版里归档或恢复，或退出桌面版后重试")
	}
	return "", setClaudeArchiveIndex(path, id, archived)
}

func claudeArchiveIndex(dirs []string, id string) (string, error) {
	var path string
	for _, dir := range dirs {
		files, err := filepath.Glob(filepath.Join(dir, "claude-code-sessions", "*", "*", "local_*.json"))
		if err != nil {
			return "", err
		}
		for _, file := range files {
			b, err := os.ReadFile(file)
			if err != nil {
				return "", err
			}
			var record claudeDesktopJSON
			if json.Unmarshal(b, &record) != nil || record.CliSessionID != id {
				continue
			}
			if path != "" {
				return "", errors.New("Claude 桌面版有多个索引条目指向这个会话")
			}
			path = file
		}
	}
	return path, nil
}

func setClaudeArchiveIndex(path, id string, archived bool) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return errors.New("Claude 的会话索引不是普通文件")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var record claudeDesktopJSON
	if err := json.Unmarshal(b, &record); err != nil {
		return err
	}
	if record.CliSessionID != id {
		return errors.New("Claude 的会话索引已变化，请重试")
	}
	if record.Archived == archived {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b, &fields); err != nil {
		return err
	}
	if archived {
		fields["isArchived"] = json.RawMessage("true")
	} else {
		fields["isArchived"] = json.RawMessage("false")
		delete(fields, "keptDirtyWorktree")
		fields["autoArchiveExempt"] = json.RawMessage("true")
		fields["unarchivedCliSessionId"] = fields["cliSessionId"]
	}
	next, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	return replaceArchiveFile(path, next, fi.Mode().Perm())
}
