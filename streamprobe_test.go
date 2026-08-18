package historyhub

// 90: SSE 探测与 stream:false 补丁的单元测试。
// 场景背景见 proxy.go sseProbe 注释 —— 渠道不支持流式时主服务中转会把 JSON
// 答案吞成"usage+[DONE]"空 SSE，探测须在写字节前发现它。

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// newProbeW 构造挂在 httptest 录制器上的探测(w=gin 测试响应写入器)。
func newProbeW(body string) (*sseProbe, *httptest.ResponseRecorder) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	p := &sseProbe{
		w:      c.Writer,
		body:   strings.NewReader(body),
		start:  time.Now(),
		header: map[string][]string{"Content-Type": {"text/event-stream"}},
		senId:  7,
	}
	return p, rec
}

// 真流式：role 首块(前奏，不提交) → 正文 chunk(提交并补发) → ... → DONE。
// 直通必须字节不增不减，入库文本=各 delta.content 拼接，头已盖章。
func TestSseProbeRealStreamTeesVerbatim(t *testing.T) {
	in := "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"},\"index\":0}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"想一想\"},\"index\":0}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"你\"},\"index\":0}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"好\"},\"index\":0}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\",\"index\":0}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":2}}\n\n" +
		"data: [DONE]\n\n"
	p, rec := newProbeW(in)
	if !p.run() {
		t.Fatal("真流式不应判定为空流")
	}
	if got := rec.Body.String(); got != in {
		t.Fatalf("直通字节必须原样:\n got %q\nwant %q", got, in)
	}
	if got := p.content(); got != "你好" {
		t.Fatalf("入库文本=delta.content 拼接, got %q", got)
	}
	if p.ttftMS < 0 {
		t.Fatalf("ttft 不应为负: %d", p.ttftMS)
	}
	if rec.Code != 200 {
		t.Fatalf("状态码 200, got %d", rec.Code)
	}
	if rec.Header().Get("X-Sen-Id") != "7" {
		t.Fatalf("X-Sen-Id 未盖章: %q", rec.Header().Get("X-Sen-Id"))
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "event-stream") {
		t.Fatalf("上游头未回显: %q", ct)
	}
}

// 空流(主服务中转对不支持流式渠道的产物)：只有 usage + [DONE]，无任何内容
// chunk → run()=false 且一个字节都没写给客户端；原始字节留待回退/日志。
func TestSseProbeEmptyStreamRetries(t *testing.T) {
	in := "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":0}}\n\n" +
		"data: [DONE]\n\n"
	p, rec := newProbeW(in)
	if p.run() {
		t.Fatal("空流应返回 false")
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("空流不得向客户端写字节, got %q", rec.Body.String())
	}
	if rec.Header().Get("X-Sen-Id") != "" {
		t.Fatal("空流不得提前盖章响应头")
	}
	// DONE 行本身已计入 raw，其后随的空行因提前 return 不会读到
	if want := strings.TrimRight(in, "\n") + "\n"; p.raw.String() != want {
		t.Fatalf("原始字节应保留供回退/日志: got %q want %q", p.raw.String(), want)
	}
}

// 流中途的错误对象也算"有内容"：提交直通，前端照常显示错误。
func TestSseProbeErrorChunkPassesThrough(t *testing.T) {
	in := "data: {\"error\":{\"message\":\"额度不足\"}}\n\n" + "data: [DONE]\n\n"
	p, rec := newProbeW(in)
	if !p.run() {
		t.Fatal("错误 chunk 应视为真流式直通")
	}
	if rec.Body.String() != in {
		t.Fatalf("错误流应原样直通, got %q", rec.Body.String())
	}
}

// 只有 role/usage/finish、没有正文的流同样判空(DONE 或 EOF 两种收尾)。
func TestSseProbePreludeOnlyIsEmpty(t *testing.T) {
	for name, in := range map[string]string{
		"done收尾": "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\ndata: [DONE]\n\n",
		"eof收尾":  "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n",
	} {
		p, rec := newProbeW(in)
		if p.run() {
			t.Fatalf("%s: 无正文流应判空", name)
		}
		if rec.Body.Len() != 0 {
			t.Fatalf("%s: 不得写字节", name)
		}
	}
}

// patchStreamFalse 只改 stream 字段，其余(含嵌套 vision 内容、自定义参数)原样。
func TestPatchStreamFalse(t *testing.T) {
	in := []byte(`{"model":"m","stream":true,"temperature":0.7,"seed":12345678901234567,` +
		`"messages":[{"role":"user","content":[{"type":"text","text":"看图"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAA"}}]}]}`)
	out, ok := patchStreamFalse(in)
	if !ok {
		t.Fatal("合法 JSON 应可补丁")
	}
	s := string(out)
	if !strings.Contains(s, `"stream":false`) {
		t.Fatalf("stream 应改为 false: %s", s)
	}
	if !strings.Contains(s, `"seed":12345678901234567`) {
		t.Fatalf("大整数 seed 不得失真: %s", s)
	}
	if !strings.Contains(s, "data:image/png;base64,AAA") {
		t.Fatalf("嵌套 vision 内容不得丢失: %s", s)
	}
	if !strings.Contains(s, `"temperature":0.7`) {
		t.Fatalf("自定义参数不得丢失: %s", s)
	}
	if _, ok := patchStreamFalse([]byte("not json")); ok {
		t.Fatal("非法 JSON 应返回 false")
	}
}

// sseDeltaOf 的"带内容"判定：正文/思考/工具调用/错误=是；role/usage/finish=否。
func TestSseDeltaOfBears(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		bears   bool
		content string
	}{
		{"role首块", `{"choices":[{"delta":{"role":"assistant"}}]}`, false, ""},
		{"usage尾块", `{"choices":[],"usage":{"prompt_tokens":1}}`, false, ""},
		{"finish空块", `{"choices":[{"delta":{},"finish_reason":"stop"}]}`, false, ""},
		{"正文", `{"choices":[{"delta":{"content":"甲"}}]}`, true, "甲"},
		{"思考", `{"choices":[{"delta":{"reasoning_content":"想"}}]}`, true, ""},
		{"思考2", `{"choices":[{"delta":{"reasoning":"想"}}]}`, true, ""},
		{"工具调用", `{"choices":[{"delta":{"tool_calls":[{"index":0}]}}]}`, true, ""},
		{"错误", `{"error":{"message":"x"}}`, true, ""},
		{"空choices", `{"choices":[]}`, false, ""},
		{"坏JSON", `{bad`, false, ""},
	}
	for _, c := range cases {
		content, bears := sseDeltaOf(c.payload)
		if bears != c.bears || content != c.content {
			t.Fatalf("%s: got (content=%q,bears=%v) want (%q,%v)", c.name, content, bears, c.content, c.bears)
		}
	}
}
