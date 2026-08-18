package historyhub

import (
	"strings"
	"testing"
)

// Verifies mergeSSELines against the exact example in 02.md's N204 discussion:
// consecutive data: chunks differing in one string field get that field
// concatenated; blank lines dropped; structure/key-order preserved.
func TestMergeSSELinesExample(t *testing.T) {
	in := "data: {\"id\":\"chatcmpl-9f765448aa17d217\",\"object\":\"chat.completion.chunk\",\"created\":1786617257,\"model\":\"GD-LLM\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"},\"logprobs\":null,\"finish_reason\":null}],\"prompt_token_ids\":null,\"prompt_text\":null}\n" +
		"\n" +
		"data: {\"id\":\"chatcmpl-9f765448aa17d217\",\"object\":\"chat.completion.chunk\",\"created\":1786617257,\"model\":\"GD-LLM\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning\":\"Here\"},\"logprobs\":null,\"finish_reason\":null,\"token_ids\":null}]}\n" +
		"\n" +
		"data: {\"id\":\"chatcmpl-9f765448aa17d217\",\"object\":\"chat.completion.chunk\",\"created\":1786617257,\"model\":\"GD-LLM\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning\":\"'s a thinking\"},\"logprobs\":null,\"finish_reason\":null,\"token_ids\":null}]}\n" +
		"\n" +
		"data: {\"id\":\"chatcmpl-9f765448aa17d217\",\"object\":\"chat.completion.chunk\",\"created\":1786617257,\"model\":\"GD-LLM\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"\\n\\n\",\"reasoning\":\"\\n\"},\"logprobs\":null,\"finish_reason\":null,\"token_ids\":null}]}\n" +
		"\n" +
		"data: {\"id\":\"chatcmpl-9f765448aa17d217\",\"object\":\"chat.completion.chunk\",\"created\":1786617257,\"model\":\"GD-LLM\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"你好！看到\"},\"logprobs\":null,\"finish_reason\":null,\"token_ids\":null}]}\n" +
		"\n" +
		"data: {\"id\":\"chatcmpl-9f765448aa17d217\",\"object\":\"chat.completion.chunk\",\"created\":1786617257,\"model\":\"GD-LLM\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"解答或协助\"},\"logprobs\":null,\"finish_reason\":null,\"token_ids\":null}]}\n" +
		"\n" +
		"data: {\"id\":\"chatcmpl-9f765448aa17d217\",\"object\":\"chat.completion.chunk\",\"created\":1786617257,\"model\":\"GD-LLM\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"！😊\"},\"logprobs\":null,\"finish_reason\":\"stop\",\"stop_reason\":null,\"token_ids\":null}],\"system_fingerprint\":\"vllm-0.23.0-tp2-7d909903\"}\n" +
		"\n" +
		"data: [DONE]\n"

	want := strings.Join([]string{
		`data: {"id":"chatcmpl-9f765448aa17d217","object":"chat.completion.chunk","created":1786617257,"model":"GD-LLM","choices":[{"index":0,"delta":{"role":"assistant","content":""},"logprobs":null,"finish_reason":null}],"prompt_token_ids":null,"prompt_text":null}`,
		`data: {"id":"chatcmpl-9f765448aa17d217","object":"chat.completion.chunk","created":1786617257,"model":"GD-LLM","choices":[{"index":0,"delta":{"reasoning":"Here's a thinking"},"logprobs":null,"finish_reason":null,"token_ids":null}]}`,
		`data: {"id":"chatcmpl-9f765448aa17d217","object":"chat.completion.chunk","created":1786617257,"model":"GD-LLM","choices":[{"index":0,"delta":{"content":"\n\n","reasoning":"\n"},"logprobs":null,"finish_reason":null,"token_ids":null}]}`,
		`data: {"id":"chatcmpl-9f765448aa17d217","object":"chat.completion.chunk","created":1786617257,"model":"GD-LLM","choices":[{"index":0,"delta":{"content":"你好！看到解答或协助"},"logprobs":null,"finish_reason":null,"token_ids":null}]}`,
		`data: {"id":"chatcmpl-9f765448aa17d217","object":"chat.completion.chunk","created":1786617257,"model":"GD-LLM","choices":[{"index":0,"delta":{"content":"！😊"},"logprobs":null,"finish_reason":"stop","stop_reason":null,"token_ids":null}],"system_fingerprint":"vllm-0.23.0-tp2-7d909903"}`,
		`data: [DONE]`,
		``,
	}, "\n")

	got := mergeSSELines([]byte(in))
	if got != want {
		t.Fatalf("merge mismatch\n--- want ---\n%s\n--- got ---\n%s", want, got)
	}
}

// Empty containers must survive the merge path as valid JSON: an upstream
// finish chunk commonly carries "delta":{} — it used to render as "delta":.
func TestMergeSSELinesEmptyContainers(t *testing.T) {
	in := "data: {\"choices\":[{\"delta\":{\"content\":\"a\"},\"index\":0}]}\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"b\"},\"index\":0}]}\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\",\"index\":0}],\"usage\":{\"completion_tokens\":2}}\n" +
		"data: [DONE]\n\n"
	want := "data: {\"choices\":[{\"delta\":{\"content\":\"ab\"},\"index\":0}]}\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\",\"index\":0}],\"usage\":{\"completion_tokens\":2}}\n" +
		"data: [DONE]\n"
	if got := mergeSSELines([]byte(in)); got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

// computeTPS: 输出阶段口径; 突发(输出窗口<500ms)与 ttft 未知时全程兜底。
// 63 的实测样本: 1264 tokens, ttft=11204ms, dur=11219ms → 全程 ≈112/s(而非 84000+/s)。
func TestComputeTPS(t *testing.T) {
	cases := []struct {
		name      string
		tokOut    int
		ttft, dur int64
		want      int64
	}{
		{"normal-output-phase", 180, 5, 1299, 139},
		{"burst-fallback-whole-dur", 1264, 11204, 11219, 112},
		{"no-ttft-whole-dur", 100, 0, 2000, 50},
		{"no-tokens", 0, 100, 1000, 0},
		{"zero-dur", 10, 0, 0, 0},
	}
	for _, c := range cases {
		if got := computeTPS(c.tokOut, c.ttft, c.dur); got != c.want {
			t.Errorf("%s: got %d want %d", c.name, got, c.want)
		}
	}
}
