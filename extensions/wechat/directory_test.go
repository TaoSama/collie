package wechat

import "testing"

// from: gateway/tests/leader/directory-query.test.ts: shared parser accepts only standalone directories and preserves requested status
func TestDirectoryQueryStandaloneOnly(t *testing.T) {
	for _, text := range []string{"查看所有会话", "会话列表", "列出当前会话", "看看现在所有的任务", "查一下当前都有哪些会话", "看一下我当前有哪些会话",
		"有哪些任务？", "当前会话列表", "请帮我看看所有会话", " 查看 所有 会话 ", "查看会话列表"} {
		if s, ok := DirectoryQuery(text); !ok || s != "" {
			t.Errorf("%q: %q %v, want all", text, s, ok)
		}
	}
	for _, text := range []string{"列出运行中的会话", "活跃会话列表", "有哪些正在运行的任务", "看看当前运行中的会话"} {
		if s, ok := DirectoryQuery(text); !ok || s != DirectoryActive {
			t.Errorf("%q: %q %v, want active", text, s, ok)
		}
	}
	for _, text := range []string{"空闲会话列表", "查看当前空闲的任务", "有哪些空闲会话"} {
		if s, ok := DirectoryQuery(text); !ok || s != DirectoryIdle {
			t.Errorf("%q: %q %v, want idle", text, s, ok)
		}
	}
	for _, text := range []string{"", "会话", "列出会话并发送", "列出会话，然后给第三个发消息", "查看所有会话\n继续第三个",
		"会话列表\n", "会话列表\u2028", "查看会话。继续第三个", "查看所有会话的进度", "查看会话模型", "列出macair会话",
		"查看项目A的任务", "记住以后查看所有会话", "‘查看所有会话’", "查看所有会话的标题", "取消任务列表",
		"把标题改成会话列表", "看看第三个任务", "运行会话", "列出会话；发消息", "列出会话并记住偏好"} {
		if _, ok := DirectoryQuery(text); ok {
			t.Errorf("%q is not a standalone directory request", text)
		}
	}
}
