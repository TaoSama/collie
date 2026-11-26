package wechat

import (
	"errors"
	"regexp"
	"strings"
)

// DefaultMessageMaxChars is the longest text one WeChat message carries, in
// UTF-16 units.
const DefaultMessageMaxChars = 3800

var (
	errBadMaxChars = errors.New("maxChars must be a positive safe integer")
	errCannotFit   = errors.New("maxChars cannot fit the next Unicode character")
)

// SplitWeChatReply cuts text into messages of at most maxChars UTF-16 units,
// preferring the end of a line. It keeps separators and indentation:
// splitting must not change code semantics. Markdown is sent as written.
func SplitWeChatReply(text string, maxChars int) ([]string, error) {
	if maxChars <= 0 {
		return nil, errBadMaxChars
	}
	if text == "" {
		return nil, nil
	}
	u := toUTF16(text)
	if len(u) <= maxChars {
		return []string{text}, nil
	}
	var chunks []string
	for len(u) > maxChars {
		splitAt := lastNewline(u, maxChars-1) + 1
		if splitAt <= 0 {
			splitAt = unicodeSafeBoundary(u, maxChars)
			if splitAt == 0 {
				return nil, errCannotFit
			}
		}
		chunks = append(chunks, fromUTF16(u[:splitAt]))
		u = u[splitAt:]
	}
	if len(u) > 0 {
		chunks = append(chunks, fromUTF16(u))
	}
	return chunks, nil
}

// lastNewline is lastIndexOf("\n", from) over UTF-16 units.
func lastNewline(u []uint16, from int) int {
	if from >= len(u) {
		from = len(u) - 1
	}
	for i := from; i >= 0; i-- {
		if u[i] == '\n' {
			return i
		}
	}
	return -1
}

const (
	directoryNoun   = `(?:会话|任务|sessions?|tasks?|agents?)`
	directoryStatus = `(?P<status>运行中(?:的)?|正在运行(?:的)?|活跃(?:的)?|空闲(?:的)?)?`
	directoryScope  = `(?:(?:当前|现在)(?:的)?)?(?:(?:所有|全部)(?:的)?)?(?:都)?(?:有哪些)?`
	// DirectoryActive and DirectoryIdle are the filters DirectoryQuery names.
	DirectoryActive = "active"
	DirectoryIdle   = "idle"
)

var (
	directoryPatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)^(?:请)?(?:帮我|给我)?(?:查看|查询|列出|看看|看一下|看下|查一下|查下|展示|显示)(?:我)?` + directoryScope + directoryStatus + directoryNoun + `(?:列表)?[？?。!！]?$`),
		regexp.MustCompile(`(?i)^(?:(?:当前|现在))?(?:都)?有哪些` + directoryStatus + directoryNoun + `[？?。!！]?$`),
		regexp.MustCompile(`(?i)^` + directoryScope + directoryStatus + directoryNoun + `列表[？?。!！]?$`),
	}
	lineBreaks = regexp.MustCompile(`[\r\n\x{2028}\x{2029}]`)
	spaceTabs  = regexp.MustCompile(`[ \t]+`)
)

// DirectoryQuery recognizes a standalone request for the session list ("查看
// 会话列表", "有哪些空闲任务"): ok, and status "" (all), "active" or "idle".
// Any extra intent falls through to the model. Collie answers it; the
// extension only forwards the text.
func DirectoryQuery(text string) (status string, ok bool) {
	if lineBreaks.MatchString(text) {
		return "", false
	}
	normalized := spaceTabs.ReplaceAllString(strings.TrimSpace(text), "")
	for _, pattern := range directoryPatterns {
		m := pattern.FindStringSubmatch(normalized)
		if m == nil {
			continue
		}
		s := m[pattern.SubexpIndex("status")]
		switch {
		case s == "":
			return "", true
		case strings.HasPrefix(s, "空闲"):
			return DirectoryIdle, true
		default:
			return DirectoryActive, true
		}
	}
	return "", false
}
