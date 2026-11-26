package wechat

import "errors"

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
