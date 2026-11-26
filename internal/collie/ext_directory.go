package collie

import (
	"fmt"
	"log"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/TaoSama/collie/extensions/wechat"
	"github.com/TaoSama/collie/internal/extproto"
)

// The session list asked for on its own in the bot's chat ("查看会话列表",
// "有哪些空闲任务") is answered by Collie straight away, without the Router:
// the workers by folder and computer, busy ones first in each.

const (
	wechatExt = "wechat"
	// directoryMostChars keeps the list to one WeChat message with its footer.
	directoryMostChars = 3000
	directoryTaskChars = 40
)

// directoryAsk: the request is a standalone ask for the session list, in an
// extension whose bot answers that itself, from you.
func directoryAsk(m *ExtManifest, in incomingMsg, request string) (status string, ok bool) {
	if m.ID != wechatExt || !in.fromYou() {
		return "", false
	}
	return wechat.DirectoryQuery(request)
}

// answerDirectory answers the request with the session list, in the
// conversation, as the answer to it.
func (h *extHost) answerDirectory(conv InboxConv, in incomingMsg, status string) {
	h.store.update(conv.ID, func(c *InboxConv) {
		c.AskMsg, c.AskAt, c.AskBy, c.Handed = in.ID, time.Now(), in.SenderName, in.At
		c.Unread = 0
	})
	ri := runInfo{Agent: "Collie", Model: "会话列表", Worker: "Collie"}
	if _, err := h.sendOut(conv.thread(), h.sp.directoryText(status), extproto.KindAnswer, ri, ""); err != nil {
		log.Printf("ext: answer the session list in %s: %v", conv.ID, err)
	}
}

// directoryStatus is a worker's state in the list: 忙 闲 完 等, or "" for one
// that ended.
func directoryStatus(w *Worker) string {
	if w.Ended {
		return ""
	}
	switch w.Status {
	case "working", "starting":
		return "忙"
	case "idle":
		return "闲"
	case "done":
		return "完"
	case "blocked":
		return "等"
	}
	return "未知"
}

func directoryWanted(label, status string) bool {
	switch status {
	case wechat.DirectoryActive:
		return label == "忙"
	case wechat.DirectoryIdle:
		return label == "闲" || label == "完"
	}
	return label != ""
}

// directoryText lists the workers that are still there, status "" (all),
// active or idle.
func (sp *Super) directoryText(status string) string {
	type row struct {
		label, name, task, machine, dir string
		at                              time.Time
	}
	sp.mu.Lock()
	var rows []row
	for _, w := range sp.st.Workers {
		label := directoryStatus(w)
		if !directoryWanted(label, status) {
			continue
		}
		at := w.CreatedAt
		if w.ReportAt.After(at) {
			at = w.ReportAt
		}
		rows = append(rows, row{label: label, name: w.Name, task: clip(firstLine(w.Task), directoryTaskChars), machine: w.Machine, dir: w.Cwd, at: at})
	}
	sp.mu.Unlock()
	if len(rows) == 0 {
		switch status {
		case wechat.DirectoryActive:
			return "现在没有正在运行的会话。"
		case wechat.DirectoryIdle:
			return "现在没有空闲的会话。"
		}
		return "现在没有会话。"
	}
	type group struct {
		head string
		rows []row
	}
	byKey := map[string]*group{}
	var groups []*group
	for _, r := range rows {
		key := r.machine + "\x00" + r.dir
		g := byKey[key]
		if g == nil {
			name := filepath.Base(strings.TrimRight(r.dir, "/"))
			if r.dir == "" || name == "." {
				name = "未关联目录"
			}
			g = &group{head: fmt.Sprintf("%s（%s）", name, sp.machineLabel(r.machine))}
			byKey[key] = g
			groups = append(groups, g)
		}
		g.rows = append(g.rows, r)
	}
	for _, g := range groups {
		sort.Slice(g.rows, func(i, j int) bool { return g.rows[i].at.After(g.rows[j].at) })
	}
	sort.SliceStable(groups, func(i, j int) bool { return groups[i].rows[0].at.After(groups[j].rows[0].at) })
	var b strings.Builder
	b.WriteString("Collie 的会话：")
	n, size := 0, 0
	for _, g := range groups {
		for i, r := range g.rows {
			line := fmt.Sprintf("%d. [%s] %s", n+1, r.label, r.name)
			if r.task != "" {
				line += "：" + r.task
			}
			if i == 0 {
				line = "\n\n" + g.head + "\n" + line
			} else {
				line = "\n" + line
			}
			if size += utf8.RuneCountInString(line); size > directoryMostChars {
				fmt.Fprintf(&b, "\n\n另有 %d 个会话放不下，在 Collie 里看全部。", len(rows)-n)
				return b.String()
			}
			b.WriteString(line)
			n++
		}
	}
	return b.String()
}
