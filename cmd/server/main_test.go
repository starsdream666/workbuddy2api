package main

import (
	"os"
	"strings"
	"testing"
)

// TestServerHasNoWriteTimeout SSE 长连接依赖"服务端不设 WriteTimeout"这一部署约束。
//
// 为什么用源码断言而不是运行时构造：http.Server 由 main() 内联构造，
// 没有可注入的构造函数（进程级 main 难以在测试里驱动）。而这条约束一旦被破坏，
// 后果是**静默的**——SSE 长连接会在超时后被强制掐断，表现为控制台实时更新
// 反复断连重连、且没有任何错误日志可查，极难定位。
//
// 所以这里直接读源码断言：
//   - 允许 ReadHeaderTimeout（只作用于请求头阶段，不影响长连接）；
//   - 禁止 WriteTimeout / ReadTimeout（作用于整个连接生命周期，会掐断 SSE）。
//
// 若将来确实需要给普通接口设超时，正确做法是按路由分组用
// http.ResponseController.SetWriteDeadline，而不是给整个 Server 设
// WriteTimeout——那时请先改这条测试，并在改动说明里解释为何 SSE 仍安全。
func TestServerHasNoWriteTimeout(t *testing.T) {
	raw, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	src := string(raw)

	// 定位 http.Server 的构造块（从 `&http.Server{` 到配对的 `}`）。
	const anchor = "&http.Server{"
	i := strings.Index(src, anchor)
	if i < 0 {
		t.Fatal("main.go 里找不到 http.Server 构造——若已重构为独立构造函数，" +
			"请把这个测试改为直接检查该构造函数，并保留本约束")
	}
	// 取锚点后 400 字符足够覆盖该结构体字面量（当前只有 Addr/Handler/ReadHeaderTimeout）。
	end := i + 400
	if end > len(src) {
		end = len(src)
	}
	block := src[i:end]

	if strings.Contains(block, "WriteTimeout") {
		t.Error("http.Server 设置了 WriteTimeout：这会掐断 SSE 长连接，" +
			"导致控制台实时更新静默失效。请移除，或改用按路由的 SetWriteDeadline")
	}
	if strings.Contains(block, "ReadTimeout:") {
		t.Error("http.Server 设置了 ReadTimeout：该字段覆盖整个连接读周期，" +
			"对 SSE 长连接有风险（应只用 ReadHeaderTimeout）")
	}
	// 正向断言：ReadHeaderTimeout 应当保留（防请求头慢速攻击）。
	if !strings.Contains(block, "ReadHeaderTimeout") {
		t.Error("http.Server 缺少 ReadHeaderTimeout：应保留它防慢速请求头攻击，" +
			"它只作用于请求头阶段，不影响 SSE")
	}
}

// TestServerWiresEventBus main 必须把事件总线接到池与 handler。
//
// 三条接线缺任何一个，实时推送都不会生效（且都是静默失效）：
//   - eventbus.New()          → 没有总线，池的通知无处可去
//   - p.SetChangeNotifier(...) → 池不通知，页面永远等不到推送
//   - server.Config{Events:}   → handler 拿不到事件源，SSE 端点永不推送
func TestServerWiresEventBus(t *testing.T) {
	raw, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	src := string(raw)

	for _, need := range []struct{ needle, why string }{
		{"eventbus.New()", "创建事件总线"},
		{"SetChangeNotifier(", "把总线注入池（否则池内变化不会广播）"},
		{"Events:", "把事件源交给 handler（否则 SSE 端点收不到信号）"},
		{"Notify:", "把通知器交给 handler（否则用量日志等**非池路径**的变化不广播）"},
	} {
		if !strings.Contains(src, need.needle) {
			t.Errorf("main.go 缺少 %q —— %s", need.needle, need.why)
		}
	}
}
