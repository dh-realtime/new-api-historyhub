package historyhub

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"

	"github.com/gin-gonic/gin"
)

// The upstream HTTP client is built lazily on first use (sync.Once) so it reads
// the project's concurrency knobs (common.RelayMaxIdleConns etc., which are set
// by common.InitEnv() during main) only after main has finished initializing.
//
// It mirrors new-api's own relay pooling: RELAY_MAX_IDLE_CONNS (500),
// RELAY_MAX_IDLE_CONNS_PER_HOST (100), RELAY_IDLE_CONN_TIMEOUT (90s). The extra
// maxConnsPerHost (default 0 = unlimited, like the main server) can
// impose a hard active cap if you want one.
var (
	upstreamClientOnce sync.Once
	upstreamClient     *http.Client
)

func client() *http.Client {
	upstreamClientOnce.Do(func() {
		maxConnsPerHost, _ := strconv.Atoi(os.Getenv("maxConnsPerHost"))
		if flagMaxConnsPerHost != nil && *flagMaxConnsPerHost > 0 {
			maxConnsPerHost = *flagMaxConnsPerHost
		}
		tr := &http.Transport{
			Proxy:               http.ProxyFromEnvironment, // honors NO_PROXY for 127.0.0.1
			MaxIdleConns:        common.RelayMaxIdleConns,
			MaxIdleConnsPerHost: common.RelayMaxIdleConnsPerHost,
			MaxConnsPerHost:     maxConnsPerHost, // 0 = unlimited
			IdleConnTimeout:     time.Duration(common.RelayIdleConnTimeout) * time.Second,
		}
		upstreamClient = &http.Client{Transport: tr, Timeout: 0} // no timeout: streaming
	})
	return upstreamClient
}

type chatMessage struct {
	Role    string      `json:"role"`
	Content interface{} `json:"content"`
}

type chatReq struct {
	Model    string        `json:"model"`
	Stream   bool          `json:"stream"`
	Messages []chatMessage `json:"messages"`
}

// aModelCache memoizes the "channel/model" label per (group,model). model.GetChannel
// does a weighted-random pick each call, so caching keeps the displayed label stable
// and avoids per-request DB work under load (N103). The cache is TTL-bounded
// (02.md N303): if an admin renames a channel, new turns pick up the new name
// within aModelTTL (default 5m). Historical rows keep the name they were
// recorded with — they reflect the channel that actually served them.
type ttlCache struct {
	mu  sync.Mutex
	m   map[string]ttlEntry
	ttl time.Duration
}

type ttlEntry struct {
	val string
	exp time.Time
}

func newTTLCache(ttl time.Duration) *ttlCache {
	return &ttlCache{m: map[string]ttlEntry{}, ttl: ttl}
}

func (c *ttlCache) get(k string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[k]
	if !ok || time.Now().After(e.exp) {
		delete(c.m, k)
		return "", false
	}
	return e.val, true
}

func (c *ttlCache) set(k, v string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[k] = ttlEntry{val: v, exp: time.Now().Add(c.ttl)}
}

func aModelTTL() time.Duration {
	if flagAModelTTLSecs != nil && *flagAModelTTLSecs > 0 {
		return time.Duration(*flagAModelTTLSecs) * time.Second
	}
	if n, err := strconv.Atoi(os.Getenv("aModelTTLSeconds")); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	return 5 * time.Minute
}

var (
	aModelCacheOnce sync.Once
	aModelCachePtr  *ttlCache
)

// 懒构造：包变量初始化早于 flag.Parse，--aModelTTLSeconds 只能在首次
// 使用（首个请求，已在 main 启动之后）时才读得到。
func aModelCache() *ttlCache {
	aModelCacheOnce.Do(func() { aModelCachePtr = newTTLCache(aModelTTL()) })
	return aModelCachePtr
}

// upstreamPort mirrors main.go's listen port so the proxy always reaches the
// co-located main server. the --ipPortMain / ipPortMain value is authoritative, then PORT,
// then the -port flag. Without this, --ipPortMain 127.0.0.1:13000 would leave
// the proxy dialling the default port while main listens elsewhere.
func upstreamPort() string {
	if a := ipPortMainValue(); a != "" {
		if i := strings.LastIndex(a, ":"); i >= 0 {
			return a[i+1:]
		}
		return a
	}
	// if p := os.Getenv("PORT"); p != "" {
	// 	return p
	// }
	return strconv.Itoa(*common.Port)
}

// resolveUser mirrors middleware.TokenAuth's key parsing, then uses the
// read-only model.GetTokenByKey (no status/quota side effects, unlike
// ValidateUserToken). Returns (0, 0, nil) if the key is absent or unknown.
// The token itself is returned so :3001 can enforce its IP allowlist
// (94b) with the real client IP before proxying to :3000.
func resolveUser(c *gin.Context) (userId, tokenId int, tok *model.Token) {
	key := strings.TrimSpace(c.GetHeader("Authorization"))
	if len(key) >= 7 && (strings.HasPrefix(key, "Bearer ") || strings.HasPrefix(key, "bearer ")) {
		key = strings.TrimSpace(key[7:])
	}
	if key == "" {
		key = strings.TrimSpace(c.GetHeader("x-api-key"))
	}
	key = strings.TrimPrefix(key, "sk-")
	if i := strings.Index(key, "-"); i >= 0 {
		key = key[:i]
	}
	if key == "" {
		return 0, 0, nil
	}
	tok, err := model.GetTokenByKey(key, false)
	if err != nil || tok == nil {
		return 0, 0, nil
	}
	return tok.UserId, tok.Id, tok
}

func parseConvHeader(c *gin.Context) int64 {
	id, err := strconv.ParseInt(strings.TrimSpace(c.GetHeader("X-Sen-Id")), 10, 64)
	if err != nil || id <= 0 {
		return 0
	}
	return id
}

// detectIsKey: the historyhub web UI marks its own requests with
// X-Historyhub-UI -> isKey=0 (web Q&A); everything else (external API-key
// clients) -> isKey=1 (N302).
func detectIsKey(c *gin.Context) int {
	if strings.TrimSpace(c.GetHeader("X-Historyhub-UI")) != "" {
		return 0
	}
	return 1
}

// resolveAModel returns "channel/model" (e.g. gd-llm/GD-LLM) by looking up a
// channel serving this model for the user's group. This is representative — it
// is the same resolver new-api uses, but not necessarily the exact channel a
// given load-balanced request hit (new-api exposes no such header).
func resolveAModel(userId int, modelName string) string {
	modelName = strings.TrimSpace(modelName)
	if modelName == "" {
		return ""
	}
	group, _ := model.GetUserGroup(userId, false)
	if group == "" {
		group = "default"
	}
	cacheKey := group + "\x00" + modelName
	if v, ok := aModelCache().get(cacheKey); ok {
		return v
	}
	res := modelName
	if ch, err := model.GetChannel(group, modelName, 0, "/v1/chat/completions"); err == nil && ch != nil && strings.TrimSpace(ch.Name) != "" {
		res = strings.TrimSpace(ch.Name) + "/" + modelName
	}
	aModelCache().set(cacheKey, res)
	return res
}

// forward reverse-proxies the request to the main server on upstreamPort().
// For an OpenAI chat/completions call it also records the turn as a q/a pair
// (two-phase, N106) and de-dups the system prompt + attachments. Everything
// else (models, embeddings, keys API, ...) is forwarded transparently so :3001
// can serve as a drop-in base_url. Every proxied request is logged (N204).
func forward(c *gin.Context, record bool) {
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "failed to read request body"}})
		return
	}

	isChat := record &&
		c.Request.Method == http.MethodPost &&
		strings.HasSuffix(c.Request.URL.Path, "/chat/completions")

	var req chatReq
	userId, tokenId := 0, 0
	var tok, ptok *model.Token
	isKey := 1
	syspId := 0
	syspMd5 := ""
	qText := ""
	qFil := ""
	convId := int64(0)

	if isChat {
		_ = common.Unmarshal(raw, &req)
		userId, tokenId, tok = resolveUser(c)
		if userId > 0 && tok != nil && !tokenIPAllowed(c.ClientIP(), tok) {
			c.JSON(http.StatusForbidden, gin.H{"error": gin.H{"message": "您的 IP 不在密钥允许访问的列表中"}})
			return
		}
		if userId > 0 {
			isKey = detectIsKey(c)
			syspId, syspMd5, qText, qFil = extractRequest(c, req)
			convId = parseConvHeader(c)
			if !senExists(userId, convId) {
				convId = 0
			}
			if convId == 0 {
				conv, _ := createSen(userId, tokenId, isKey, deriveTitle(qText))
				if conv != nil {
					convId = conv.Id
				}
			}
		}
	} else {
		// Still resolve the user so passthrough requests can be logged.
		userId, _, ptok = resolveUser(c)
		if userId > 0 && ptok != nil && !tokenIPAllowed(c.ClientIP(), ptok) {
			c.JSON(http.StatusForbidden, gin.H{"error": gin.H{"message": "您的 IP 不在密钥允许访问的列表中"}})
			return
		}
	}

	// Build and send the upstream request using the ORIGINAL raw body so
	// multipart / vision / unknown fields are forwarded byte-for-byte.
	start := time.Now()
	target := "http://127.0.0.1:" + upstreamPort() + c.Request.URL.Path
	if c.Request.URL.RawQuery != "" {
		target += "?" + c.Request.URL.RawQuery
	}
	upReq, err := http.NewRequestWithContext(c.Request.Context(), c.Request.Method, target, bytes.NewReader(raw))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": "failed to build upstream request"}})
		return
	}
	copyHeaders(upReq.Header, c.Request.Header)
	applyForwardedFor(upReq.Header, c)

	resp, err := client().Do(upReq)

	// Phase 1 (N106): the request has now been sent to the upstream — record
	// the request side of the turn before we start streaming the answer back.
	msgId := int64(0)
	if isChat && userId > 0 && convId > 0 {
		msgId = insertMsgQ(userId, convId, syspId, strings.TrimSpace(qText), qFil, start.UnixMilli())
	}

	if err != nil {
		errMsg := "[报错Error] 请求失败: " + err.Error()
		if msgId > 0 {
			updateMsgA(userId, msgId, resolveAModel(userId, req.Model), errMsg, "", 0, 0, 0, 0)
		}
		if userId > 0 {
			// N204: log the request even when the upstream is unreachable.
			logHTTP(httpLogEntry{
				userId:     userId,
				method:     c.Request.Method,
				path:       c.Request.URL.Path,
				status:     http.StatusBadGateway,
				durationMS: time.Since(start).Milliseconds(),
				reqHeaders: c.Request.Header,
				reqBody:    raw,
				syspFile:   syspFileOf(syspId, syspMd5),
				note:       "upstream unreachable: " + err.Error(),
			})
		}
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": "upstream unreachable: " + err.Error()}})
		return
	}
	defer resp.Body.Close()

	aText := ""
	aFil := ""
	ttftMS := int64(0)
	var respBody []byte
	logResp := resp // N204 记录默认按首次响应；空流重试成功后指向重试响应
	isSSE := strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")
	if isSSE && resp.StatusCode == http.StatusOK && isChat && req.Stream {
		// 90: 探测后再决定怎么回 —— 见 sseProbe 注释
		probe := &sseProbe{w: c.Writer, body: resp.Body, start: start, header: resp.Header, senId: convId}
		if probe.run() {
			aText, respBody, ttftMS = probe.content(), probe.raw.Bytes(), probe.ttftMS
		} else {
			// 空流：渠道不支持流式，主服务中转把 JSON 答案吞成了 usage+[DONE]。
			// 客户端尚未收到任何字节，改用 stream:false 原题重发一次，同一轮
			// 消息行(msgId)复用，答案以普通 JSON 返回，由前端按非流式渲染。
			_ = resp.Body.Close()
			retried := false
			if raw2, ok := patchStreamFalse(raw); ok {
				up2, e2 := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, target, bytes.NewReader(raw2))
				if e2 == nil {
					copyHeaders(up2.Header, c.Request.Header)
					applyForwardedFor(up2.Header, c)
					if resp2, err2 := client().Do(up2); err2 == nil {
						defer resp2.Body.Close()
						body2, _ := io.ReadAll(resp2.Body)
						dst := c.Writer.Header()
						copyHeaders(dst, resp2.Header)
						if convId > 0 {
							dst.Set("X-Sen-Id", strconv.FormatInt(convId, 10))
						}
						c.Writer.WriteHeader(resp2.StatusCode)
						_, _ = c.Writer.Write(body2)
						respBody, logResp = body2, resp2
						if resp2.StatusCode == http.StatusOK {
							aText, aFil = extractAssistantAndFiles(body2)
						} else {
							aText = "[报错Error] " + extractError(body2, resp2.StatusCode)
						}
						retried = true
					}
				}
			}
			if !retried {
				// 重试没能发出：按修复前的行为把(空)SSE 原样回给客户端
				dst := c.Writer.Header()
				copyHeaders(dst, resp.Header)
				if convId > 0 {
					dst.Set("X-Sen-Id", strconv.FormatInt(convId, 10))
				}
				c.Writer.WriteHeader(resp.StatusCode)
				_, _ = c.Writer.Write(probe.raw.Bytes())
				respBody = probe.raw.Bytes()
			}
		}
	} else if isSSE && resp.StatusCode == http.StatusOK {
		// 先回显上游头并盖章会话 id，再逐行直通(拷头时会剔除 hop-by-hop 头)。
		dst := c.Writer.Header()
		copyHeaders(dst, resp.Header)
		if isChat && convId > 0 {
			dst.Set("X-Sen-Id", strconv.FormatInt(convId, 10))
		}
		c.Writer.WriteHeader(resp.StatusCode)
		aText, respBody, ttftMS = streamAndTee(c.Writer, resp.Body, start)
	} else {
		dst := c.Writer.Header()
		copyHeaders(dst, resp.Header)
		if isChat && convId > 0 {
			dst.Set("X-Sen-Id", strconv.FormatInt(convId, 10))
		}
		c.Writer.WriteHeader(resp.StatusCode)
		respBody, _ = io.ReadAll(resp.Body)
		_, _ = c.Writer.Write(respBody)
		if isChat {
			if resp.StatusCode == http.StatusOK {
				aText, aFil = extractAssistantAndFiles(respBody)
			} else {
				aText = "[报错Error] " + extractError(respBody, resp.StatusCode)
			}
		}
	}
	promptTok, completionTok := extractUsage(respBody)

	// Phase 2 (N106): the response has been returned to the user — fill in the
	// answer side of the turn (incl. token usage).
	if msgId > 0 {
		updateMsgA(userId, msgId, resolveAModel(userId, req.Model), strings.TrimSpace(aText), aFil, promptTok, completionTok, ttftMS, time.Since(start).Milliseconds())
		touchSen(userId, convId)
	}
	// N204: now that the user has been replied to, write the complete
	// request/response block to the per-day log.
	if userId > 0 {
		logHTTP(httpLogEntry{
			userId:        userId,
			method:        c.Request.Method,
			path:          c.Request.URL.Path,
			status:        logResp.StatusCode,
			durationMS:    time.Since(start).Milliseconds(),
			reqHeaders:    c.Request.Header,
			reqBody:       raw,
			syspFile:      syspFileOf(syspId, syspMd5),
			respHeaders:   logResp.Header,
			respBody:      respBody,
			promptTok:     promptTok,
			completionTok: completionTok,
		})
	}
}

// syspFileOf turns a recorded system prompt into its hybfil file reference for
// the log; empty when nothing was recorded.
func syspFileOf(syspId int, syspMd5 string) string {
	if syspId <= 0 || syspMd5 == "" {
		return ""
	}
	return syspMd5 + ".sysp"
}

// extractRequest pulls the de-duped system-prompt id (+ its md5 file stem for
// log masking), the (trimmed) last user question, and the comma-separated
// attachment ids out of a chat request.
func extractRequest(c *gin.Context, req chatReq) (syspId int, syspMd5, q, qFil string) {
	var syspParts []string
	var lastUser *chatMessage
	for i := range req.Messages {
		m := &req.Messages[i]
		if m.Role == "system" {
			if s := stringifyContent(m.Content); s != "" {
				syspParts = append(syspParts, s)
			}
		}
		if m.Role == "user" {
			lastUser = m
		}
	}
	if len(syspParts) > 0 {
		syspId, syspMd5 = dedupSysp(agentTag(c), strings.Join(syspParts, "\n"))
	}
	if lastUser != nil {
		q = stringifyContent(lastUser.Content)
		qFil = parseAttachments(lastUser.Content)
	} else if n := len(req.Messages); n > 0 {
		// No explicit user turn — fall back to the trailing message text.
		q = stringifyContent(req.Messages[n-1].Content)
		qFil = parseAttachments(req.Messages[n-1].Content)
	}
	return syspId, syspMd5, q, qFil
}

// agentTag derives a short tag (<=16) for the system-prompt table from the
// User-Agent header (e.g. codex / zcode / OpenClaw).
func agentTag(c *gin.Context) string {
	ua := strings.TrimSpace(c.GetHeader("User-Agent"))
	if ua == "" {
		return ""
	}
	// First token of the UA, lower-cased.
	if i := strings.IndexAny(ua, " /;"); i > 0 {
		ua = ua[:i]
	}
	return strings.ToLower(truncate(ua, 16))
}

// parseAttachments scans an OpenAI vision content for data: image URLs, saves
// each, and returns the comma-separated httpFile ids ("" if none). Remote
// http(s) URLs are intentionally not downloaded.
func parseAttachments(content interface{}) string {
	arr, ok := content.([]interface{})
	if !ok {
		return ""
	}
	var ids []string
	for _, e := range arr {
		m, ok := e.(map[string]interface{})
		if !ok {
			continue
		}
		if ty, _ := m["type"].(string); ty == "image_url" {
			if iu, _ := m["image_url"].(map[string]interface{}); iu != nil {
				if url, _ := iu["url"].(string); url != "" {
					if ext, data, ok := decodeDataURL(url); ok {
						if id := saveFile("upload"+ext, data); id > 0 {
							ids = append(ids, strconv.FormatInt(id, 10))
						}
					}
				}
			}
		}
	}
	return strings.Join(ids, ",")
}

// decodeDataURL parses a "data:image/png;base64,...." string.
func decodeDataURL(s string) (ext string, data []byte, ok bool) {
	const pfx = "data:"
	if !strings.HasPrefix(s, pfx) {
		return "", nil, false
	}
	comma := strings.Index(s, ",")
	if comma < 0 {
		return "", nil, false
	}
	head := s[len(pfx):comma]
	b64 := s[comma+1:]
	mime := head
	if i := strings.Index(head, ";"); i >= 0 {
		mime = head[:i]
	}
	ext = mimeToExt(mime)
	var err error
	if data, err = base64.StdEncoding.DecodeString(b64); err == nil {
		return ext, data, true
	}
	if data, err = base64.RawStdEncoding.DecodeString(b64); err == nil {
		return ext, data, true
	}
	return ext, nil, false
}

func mimeToExt(mime string) string {
	switch strings.ToLower(strings.TrimSpace(mime)) {
	case "image/png":
		return ".png"
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	case "image/bmp":
		return ".bmp"
	case "application/pdf":
		return ".pdf"
	default:
		return ".bin"
	}
}

// ---------------------------------------------------------------------------
// 90: SSE 探测 —— 解决"渠道不支持流式"时的空流。
//
// 客户端请求 stream:true 而上游渠道只会回普通 JSON 时，主服务中转(openai 兼容
// 通道的 OaiStreamHandler)扫不到任何 data: 行，会把 JSON 答案整个丢掉、只给
// :3001 回一条 usage chunk + [DONE] 的"空 SSE"。内容在中转层已丢失，无法从
// 字节流恢复，只能在 :3001 重发一次。sseProbe 的作用是把"发现是空流"的时机
// 提前到向客户端写出任何字节之前：
//   - 未提交(未写响应头)前，chunk 一律先攒着；
//   - 一旦出现"带内容"的 chunk(delta/message 的正文、思考、工具调用，或错误
//     对象)，立即提交响应头、按原样补发攒下的行，之后逐行直通；
//   - 若流直到 [DONE]/EOF 都没有任何内容 chunk，run() 返回 false，且客户端
//     没收到任何字节 —— forward() 此时改用 stream:false 原题重发，把真正的
//     JSON 答案拿回来。代价：空流那次上游照常计费(基本只有 prompt tokens)，
//     属于"模型不支持流式"场景下换可用性的最小代价；消息行复用、仍只记一轮。
// ---------------------------------------------------------------------------

type sseProbe struct {
	w      gin.ResponseWriter
	body   io.Reader
	start  time.Time
	header http.Header
	senId  int64

	held      []string     // 未提交前攒下的行(含空行/注释行)，提交时按原样补发
	raw       bytes.Buffer // 整条流原始字节(N204 日志 / 回退补发用)
	cbuf      strings.Builder
	ttftMS    int64
	firstAt   time.Time
	committed bool
}

func (p *sseProbe) content() string { return p.cbuf.String() }

func (p *sseProbe) commitHeader() {
	if p.committed {
		return
	}
	dst := p.w.Header()
	copyHeaders(dst, p.header)
	if p.senId > 0 {
		dst.Set("X-Sen-Id", strconv.FormatInt(p.senId, 10))
	}
	p.w.WriteHeader(http.StatusOK)
	p.committed = true
}

// run 消费整条 SSE 流：见到首条内容 chunk 即提交并转为直通；返回 false 表示
// 流从头到尾没有内容 chunk(空流)，此时尚未向客户端写过任何字节。
func (p *sseProbe) run() bool {
	reader := bufio.NewReaderSize(p.body, 64*1024)
	const prefix = "data: "
	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			p.raw.WriteString(line)
			if p.committed {
				_, _ = p.w.Write([]byte(line))
				p.w.Flush()
			} else {
				p.held = append(p.held, line)
			}
			trimmed := strings.TrimRight(line, "\r\n")
			if strings.HasPrefix(trimmed, prefix) {
				if p.firstAt.IsZero() {
					p.firstAt = time.Now()
					p.ttftMS = p.firstAt.Sub(p.start).Milliseconds()
				}
				payload := strings.TrimPrefix(trimmed, prefix)
				if payload == "[DONE]" {
					if !p.committed {
						return false // 流结束仍无内容 chunk：空流
					}
				} else {
					content, bears := sseDeltaOf(payload)
					p.cbuf.WriteString(content)
					if bears && !p.committed {
						p.commitHeader()
						for _, h := range p.held {
							_, _ = p.w.Write([]byte(h))
						}
						p.held = nil
						p.w.Flush()
					}
				}
			}
		}
		if err != nil {
			break
		}
	}
	return p.committed
}

// sseDeltaOf 解析一条 data: 载荷：返回其中的正文增量(delta.content，兼容
// 少见的中转层 message.content，供入库)，以及该 chunk 是否"带内容"——正文/
// 思考/工具调用/错误对象都算；纯 role 首块、usage 尾块、finish 空块不算。
func sseDeltaOf(payload string) (content string, bears bool) {
	var v struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
		Choices []struct {
			Delta struct {
				Content   string     `json:"content"`
				Reasoning string     `json:"reasoning_content"`
				Reason    string     `json:"reasoning"`
				ToolCalls []struct{} `json:"tool_calls"`
			} `json:"delta"`
			Message struct {
				Content   string `json:"content"`
				Reasoning string `json:"reasoning_content"`
				Reason    string `json:"reasoning"`
			} `json:"message"`
		} `json:"choices"`
	}
	if common.Unmarshal([]byte(payload), &v) != nil {
		return "", false
	}
	if v.Error != nil {
		return "", true
	}
	for i := range v.Choices {
		c := &v.Choices[i]
		if c.Delta.Content != "" {
			content += c.Delta.Content
		} else if c.Message.Content != "" {
			content += c.Message.Content
		}
		if c.Delta.Content != "" || c.Message.Content != "" ||
			c.Delta.Reasoning != "" || c.Delta.Reason != "" ||
			c.Message.Reasoning != "" || c.Message.Reason != "" ||
			len(c.Delta.ToolCalls) > 0 {
			bears = true
		}
	}
	return content, bears
}

// patchStreamFalse 把请求 JSON 顶层的 "stream" 改为 false，其余字段字节原样
// 保留(map[string]json.RawMessage，不走结构体重序列化，vision/自定义参数不丢)。
func patchStreamFalse(raw []byte) ([]byte, bool) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || len(m) == 0 {
		return nil, false
	}
	m["stream"] = json.RawMessage("false")
	out, err := json.Marshal(m)
	if err != nil {
		return nil, false
	}
	return out, true
}

// streamAndTee copies an SSE stream to the client while accumulating the joined
// assistant content (from choices[].delta.content, for the DB record) and the
// raw SSE bytes (for the N204 response log, which merges data: lines). Returns
// (content, rawBytes, ttftMS) — ttftMS is the latency from start to the first
// data: chunk (0 when the stream carried none).
func streamAndTee(w gin.ResponseWriter, body io.Reader, start time.Time) (content string, raw []byte, ttftMS int64) {
	reader := bufio.NewReaderSize(body, 64*1024)
	var cbuf strings.Builder
	var buf bytes.Buffer
	var firstAt time.Time
	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			buf.WriteString(line)
			_, _ = w.Write([]byte(line))
			w.Flush()
			trimmed := strings.TrimRight(line, "\r\n")
			const prefix = "data: "
			if strings.HasPrefix(trimmed, prefix) {
				if firstAt.IsZero() {
					firstAt = time.Now()
					ttftMS = firstAt.Sub(start).Milliseconds()
				}
				payload := strings.TrimPrefix(trimmed, prefix)
				if payload != "[DONE]" {
					var delta struct {
						Choices []struct {
							Delta struct {
								Content string `json:"content"`
							} `json:"delta"`
						} `json:"choices"`
					}
					if common.Unmarshal([]byte(payload), &delta) == nil && len(delta.Choices) > 0 {
						cbuf.WriteString(delta.Choices[0].Delta.Content)
					}
				}
			}
		}
		if err != nil {
			break
		}
	}
	return cbuf.String(), buf.Bytes(), ttftMS
}

// extractAssistantAndFiles parses a non-streaming chat completion response into
// the assistant text and any response attachment ids.
func extractAssistantAndFiles(buf []byte) (string, string) {
	var resp struct {
		Choices []struct {
			Message struct {
				Content interface{} `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if common.Unmarshal(buf, &resp) != nil || len(resp.Choices) == 0 {
		return "", ""
	}
	content := resp.Choices[0].Message.Content
	return stringifyContent(content), parseAttachments(content)
}

// extractError pulls a human-readable message out of an OpenAI-style error body.
func extractError(buf []byte, status int) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if common.Unmarshal(buf, &e) == nil {
		if e.Error.Message != "" {
			return e.Error.Message
		}
		if e.Message != "" {
			return e.Message
		}
	}
	return "HTTP " + strconv.Itoa(status)
}

// extractUsage pulls prompt_tokens / completion_tokens out of a chat completion
// response. Each SSE `data:` payload is properly JSON-parsed (the old raw byte
// scan broke when the usage object contained nested objects — the first `}`
// closed a child and the slice stopped being valid JSON, yielding 0/0). Falls
// back to parsing the whole body for non-streaming responses. Returns (0, 0)
// when absent.
func extractUsage(buf []byte) (prompt, completion int) {
	type usageObj struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	}
	for _, raw := range strings.Split(string(buf), "\n") {
		line := strings.TrimRight(raw, "\r")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var v struct {
			Usage *usageObj `json:"usage"`
		}
		if common.Unmarshal([]byte(payload), &v) == nil && v.Usage != nil {
			return v.Usage.PromptTokens, v.Usage.CompletionTokens
		}
	}
	var w struct {
		Usage *usageObj `json:"usage"`
	}
	if common.Unmarshal(buf, &w) == nil && w.Usage != nil {
		return w.Usage.PromptTokens, w.Usage.CompletionTokens
	}
	return 0, 0
}

// stringifyContent flattens an OpenAI message content (string or array of
// {type,text} parts, as used by vision models) into plain text.
func stringifyContent(v interface{}) string {
	switch t := v.(type) {
	case string:
		return t
	case []interface{}:
		var sb strings.Builder
		for _, e := range t {
			if m, ok := e.(map[string]interface{}); ok {
				if ty, _ := m["type"].(string); ty == "text" {
					if s, _ := m["text"].(string); s != "" {
						sb.WriteString(s)
					}
				}
			}
		}
		return sb.String()
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", v)
	}
}

func deriveTitle(q string) string {
	s := strings.TrimSpace(q)
	if s == "" {
		return "New chat"
	}
	r := []rune(s)
	if len(r) > 40 {
		r = append(r[:40], []rune("…")...)
	}
	return string(r)
}

func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		switch strings.ToLower(k) {
		case "host", "content-length", "transfer-encoding", "connection":
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// applyForwardedFor 把 :3001 看到的真实来源 IP 追加到 X-Forwarded-For 末尾。
// 主服务默认信任回环代理(middleware.ConfigureTrustedProxies 的缺省值),会从右往左
// 取第一个不受信条目 —— 即我们追加的这个 —— 因此其密钥 IP 白名单按 agent 的真实
// IP 判定,而不是 127.0.0.1(94b)。
func applyForwardedFor(dst http.Header, c *gin.Context) {
	if prior := dst.Get("X-Forwarded-For"); prior != "" {
		dst.Set("X-Forwarded-For", prior+", "+c.ClientIP())
	} else {
		dst.Set("X-Forwarded-For", c.ClientIP())
	}
}
