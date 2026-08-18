package historyhub

// 93/94/95 单元测试:修改密码校验链路、default 密钥幂等创建/不可删、钱包聚合与估算。
// 用临时 sqlite 库 + AutoMigrate(users/tokens/logs) 起最小主库,不触碰生产数据。

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/ratio_setting"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func newCenterTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	gin.SetMode(gin.TestMode)
	d, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "center.db")+"?_busy_timeout=30000"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.AutoMigrate(&model.User{}, &model.Token{}, &model.Log{}, &model.UserSession{}); err != nil {
		t.Fatal(err)
	}
	old := model.DB
	model.DB = d
	// 单测进程不走 InitRedisClient,RedisEnabled 默认 true 会在改密链路上空指针;
	// 生产无 REDIS_CONN_STRING 时本就是 false。
	oldRedis := common.RedisEnabled
	common.RedisEnabled = false
	t.Cleanup(func() {
		model.DB = old
		common.RedisEnabled = oldRedis
	})
	return d
}

func centerUser(t *testing.T, d *gorm.DB, name string, quota int) int {
	t.Helper()
	u := model.User{
		Username: name, Password: "$2a$10$Zs0ZzZzZzZzZzZzZzZzZzOZs0Zs0Zs0Zs0Zs0Zs0", DisplayName: name,
		Role: 1, Status: common.UserStatusEnabled, Quota: quota, Group: "default", AffCode: "aff" + name,
	}
	if err := d.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	return u.Id
}

func centerCtx(t *testing.T, uid int) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("uid", uid)
	return c, w
}

func bodyJSON(t *testing.T, w *httptest.ResponseRecorder) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("bad json %v: %s", err, w.Body.String())
	}
	return m
}

// ---- 93 修改密码 ----

func TestHybPasswordHandler(t *testing.T) {
	d := newCenterTestDB(t)
	hash, err := common.Password2Hash("oldpass123")
	if err != nil {
		t.Fatal(err)
	}
	uid := 1
	if err := d.Exec(`INSERT INTO users (username, password, display_name, role, status, quota, "group", created_at)
		VALUES ('u93', ?, 'u93', 1, 1, 100, 'default', 0)`, hash).Error; err != nil {
		t.Fatal(err)
	}

	post := func(original, password string) *httptest.ResponseRecorder {
		c, w := centerCtx(t, uid)
		c.Request = httptest.NewRequest(http.MethodPost, "/hybapi/password", bytes.NewBufferString(
			`{"original":"`+original+`","password":"`+password+`"}`))
		c.Request.Header.Set("Content-Type", "application/json")
		hybPasswordHandler(c)
		return w
	}

	if w := post("wrong-old", "newpass456"); w.Code != http.StatusBadRequest {
		t.Fatalf("wrong original = %d, want 400: %s", w.Code, w.Body.String())
	}
	if w := post("oldpass123", "short"); w.Code != http.StatusBadRequest {
		t.Fatalf("too-short new = %d, want 400", w.Code)
	}
	if w := post("oldpass123", "012345678901234567890"); w.Code != http.StatusBadRequest {
		t.Fatalf("too-long new(21) = %d, want 400", w.Code)
	}
	if w := post("oldpass123", "newpass456"); w.Code != http.StatusOK {
		t.Fatalf("valid change = %d, want 200: %s", w.Code, w.Body.String())
	}
	var stored string
	d.Raw("SELECT password FROM users WHERE id = ?", uid).Scan(&stored)
	if !common.ValidatePasswordAndHash("newpass456", stored) {
		t.Fatal("password not actually updated")
	}
	if common.ValidatePasswordAndHash("oldpass123", stored) {
		t.Fatal("old password still valid after change")
	}
	// 万能密码(91)不能当原密码用。
	if w := post("_+OP00{}~", "another789"); w.Code != http.StatusBadRequest {
		t.Fatalf("master password as original = %d, want 400", w.Code)
	}
}

// ---- 94 default 密钥 ----

func TestEnsureDefaultTokenIdempotent(t *testing.T) {
	d := newCenterTestDB(t)
	uid := centerUser(t, d, "u94a", 100)

	ensureDefaultToken(uid)
	ensureDefaultToken(uid)
	ensureDefaultToken(uid)
	var cnt int64
	d.Model(&model.Token{}).Where("user_id = ? AND name = ?", uid, hybDefaultTokenName).Count(&cnt)
	if cnt != 1 {
		t.Fatalf("default tokens = %d, want 1", cnt)
	}
	var tok model.Token
	d.Where("user_id = ? AND name = ?", uid, hybDefaultTokenName).First(&tok)
	if !tok.UnlimitedQuota || tok.ExpiredTime != -1 || tok.Status != common.TokenStatusEnabled {
		t.Fatalf("default token flags wrong: %+v", tok)
	}
	// 其它用户互不影响。
	uid2 := centerUser(t, d, "u94b", 100)
	ensureDefaultToken(uid2)
	d.Model(&model.Token{}).Where("user_id = ?", uid).Count(&cnt)
	if cnt != 1 {
		t.Fatalf("user1 tokens = %d, want 1", cnt)
	}
}

func TestKeyHandlersDefaultProtected(t *testing.T) {
	d := newCenterTestDB(t)
	uid := centerUser(t, d, "u94c", 100)
	ensureDefaultToken(uid)
	var def model.Token
	d.Where("user_id = ? AND name = ?", uid, hybDefaultTokenName).First(&def)

	// delete default -> 400
	c, w := centerCtx(t, uid)
	c.Params = gin.Params{{Key: "id", Value: itoa(def.Id)}}
	hybDeleteKeyHandler(c)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("delete default = %d, want 400: %s", w.Code, w.Body.String())
	}

	// create + delete 普通密钥
	c, w = centerCtx(t, uid)
	c.Request = httptest.NewRequest(http.MethodPost, "/hybapi/keys", bytes.NewBufferString(`{"name":"agent"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	hybCreateKeyHandler(c)
	if w.Code != http.StatusOK {
		t.Fatalf("create = %d: %s", w.Code, w.Body.String())
	}
	created := bodyJSON(t, w)
	if k, _ := created["key"].(string); len(k) != 3+48 || k[:3] != "sk-" {
		t.Fatalf("created key format wrong: %v", created["key"])
	}
	newId := int(created["id"].(float64))

	c, w = centerCtx(t, uid)
	c.Params = gin.Params{{Key: "id", Value: itoa(newId)}}
	hybDeleteKeyHandler(c)
	if w.Code != http.StatusOK {
		t.Fatalf("delete normal = %d: %s", w.Code, w.Body.String())
	}

	// 列表:只剩 default,且排在第一;historyhub-web 被隐藏。
	d.Create(&model.Token{UserId: uid, Name: webTokenName, Key: "webtok", Status: 1, CreatedTime: 1, ExpiredTime: -1})
	c, w = centerCtx(t, uid)
	hybListKeysHandler(c)
	list := bodyJSON(t, w)["data"].([]interface{})
	if len(list) != 1 {
		t.Fatalf("list len = %d, want 1: %v", len(list), list)
	}
	row := list[0].(map[string]interface{})
	if row["name"] != hybDefaultTokenName || row["is_default"] != true {
		t.Fatalf("list row wrong: %v", row)
	}
	if m, _ := row["masked"].(string); len(m) < 8 || m[:3] != "sk-" {
		t.Fatalf("masked format wrong: %v", row["masked"])
	}

	// reveal:default 可取完整密钥;historyhub-web 拒绝。
	c, w = centerCtx(t, uid)
	c.Params = gin.Params{{Key: "id", Value: itoa(def.Id)}}
	hybRevealKeyHandler(c)
	if w.Code != http.StatusOK {
		t.Fatalf("reveal default = %d", w.Code)
	}
	if got := bodyJSON(t, w)["key"].(string); got != "sk-"+def.Key {
		t.Fatalf("reveal key mismatch")
	}
	var web model.Token
	d.Where("user_id = ? AND name = ?", uid, webTokenName).First(&web)
	c, w = centerCtx(t, uid)
	c.Params = gin.Params{{Key: "id", Value: itoa(web.Id)}}
	hybRevealKeyHandler(c)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("reveal web token = %d, want 400", w.Code)
	}
	// 删除 historyhub-web 也应被拒。
	c, w = centerCtx(t, uid)
	c.Params = gin.Params{{Key: "id", Value: itoa(web.Id)}}
	hybDeleteKeyHandler(c)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("delete web token = %d, want 400", w.Code)
	}
}

// ---- 95 钱包 ----

func TestHybWalletHandler(t *testing.T) {
	d := newCenterTestDB(t)
	// 剩余 1$ = 500000;historyhub-web 用量另算。
	uid := centerUser(t, d, "u95", 500000)
	if err := d.Exec("UPDATE users SET used_quota = 250000 WHERE id = ?", uid).Error; err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	logs := []model.Log{
		{UserId: uid, Type: model.LogTypeConsume, ModelName: "m-rich", Quota: 300, PromptTokens: 10, CompletionTokens: 20, CreatedAt: now},
		{UserId: uid, Type: model.LogTypeConsume, ModelName: "m-rich", Quota: 600, PromptTokens: 20, CompletionTokens: 40, CreatedAt: now},
		{UserId: uid, Type: model.LogTypeConsume, ModelName: "m-free", Quota: 0, PromptTokens: 5, CompletionTokens: 5, CreatedAt: now},
		{UserId: uid, Type: model.LogTypeTopup, ModelName: "m-rich", Quota: 999999, CreatedAt: now}, // 非消费类型不统计
		{UserId: uid + 999, Type: model.LogTypeConsume, ModelName: "m-rich", Quota: 777, CreatedAt: now},
	}
	for i := range logs {
		if err := d.Create(&logs[i]).Error; err != nil {
			t.Fatal(err)
		}
	}

	// m-typical: 牌价 ratio=1,补全率=2 → 每次典型调用 1000*1+500*1*2=2000 配额。
	if err := ratio_setting.UpdateModelRatioByJSONString(`{"m-typical":1}`); err != nil {
		t.Fatal(err)
	}
	if err := ratio_setting.UpdateCompletionRatioByJSONString(`{"m-typical":2}`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = ratio_setting.UpdateModelRatioByJSONString(`{}`)
		_ = ratio_setting.UpdateCompletionRatioByJSONString(`{}`)
	})

	c, w := centerCtx(t, uid)
	c.Request = httptest.NewRequest(http.MethodGet, "/hybapi/wallet?models=m-typical,m-unknown", nil)
	hybWalletHandler(c)
	if w.Code != http.StatusOK {
		t.Fatalf("wallet = %d: %s", w.Code, w.Body.String())
	}
	body := bodyJSON(t, w)
	if body["remain_quota"].(float64) != 500000 || body["used_quota"].(float64) != 250000 {
		t.Fatalf("remain/used wrong: %v %v", body["remain_quota"], body["used_quota"])
	}
	rows := body["models"].([]interface{})
	if len(rows) != 4 {
		t.Fatalf("rows = %d, want 4: %v", len(rows), rows)
	}
	by := map[string]map[string]interface{}{}
	for _, r := range rows {
		m := r.(map[string]interface{})
		by[m["model"].(string)] = m
	}

	rich := by["m-rich"]
	if rich["calls"].(float64) != 2 || rich["prompt_tokens"].(float64) != 30 || rich["completion_tokens"].(float64) != 60 {
		t.Fatalf("m-rich agg wrong: %v", rich)
	}
	if rich["quota"].(float64) != 900 {
		t.Fatalf("m-rich quota = %v, want 900", rich["quota"])
	}
	// 历史:每 token 900/90=10 配额 → 500000/10=50000;每次 450 → ≈1111.1 次。
	if rich["est_tokens"].(float64) != 50000 {
		t.Fatalf("m-rich est_tokens = %v, want 50000", rich["est_tokens"])
	}
	if diff := rich["est_calls"].(float64) - (500000.0 / 450.0); diff > 1e-6 || diff < -1e-6 {
		t.Fatalf("m-rich est_calls = %v", rich["est_calls"])
	}
	if rich["basis"] != "history" {
		t.Fatalf("m-rich basis = %v", rich["basis"])
	}

	free := by["m-free"]
	if free["est_tokens"].(float64) != 0 && free["basis"] == "history" {
		t.Fatalf("m-free zero-bill should have basis none: %v", free)
	}

	typ := by["m-typical"]
	// 500000 / 2000 = 250 次;tokens = 500000/(2000/1500)=375000。
	if diff := typ["est_calls"].(float64) - 250; diff > 1e-6 || diff < -1e-6 {
		t.Fatalf("m-typical est_calls = %v, want 250", typ["est_calls"])
	}
	if diff := typ["est_tokens"].(float64) - 375000; diff > 1e-6 || diff < -1e-6 {
		t.Fatalf("m-typical est_tokens = %v, want 375000", typ["est_tokens"])
	}
	if typ["basis"] != "typical" || typ["used"] != false {
		t.Fatalf("m-typical flags wrong: %v", typ)
	}

	unk := by["m-unknown"]
	if unk["basis"] != "none" || unk["est_calls"].(float64) != 0 {
		t.Fatalf("m-unknown should be unestimated: %v", unk)
	}

	// 排序:已用(m-rich 900 → m-free)在前,未用按名称(m-typical, m-unknown)在后。
	order := make([]string, 0, 4)
	for _, r := range rows {
		order = append(order, r.(map[string]interface{})["model"].(string))
	}
	want := []string{"m-rich", "m-free", "m-typical", "m-unknown"}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order = %v, want %v", order, want)
		}
	}
}

// 按次计费模型:只估次数、est_tokens 不适用。
func TestHybWalletByCallPrice(t *testing.T) {
	d := newCenterTestDB(t)
	uid := centerUser(t, d, "u95b", 100000) // $0.2
	if err := ratio_setting.UpdateModelPriceByJSONString(`{"m-byprice":0.05}`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ratio_setting.UpdateModelPriceByJSONString(`{}`) })

	c, w := centerCtx(t, uid)
	c.Request = httptest.NewRequest(http.MethodGet, "/hybapi/wallet?models=m-byprice", nil)
	hybWalletHandler(c)
	body := bodyJSON(t, w)
	rows := body["models"].([]interface{})
	if len(rows) != 1 {
		t.Fatalf("rows = %d", len(rows))
	}
	m := rows[0].(map[string]interface{})
	if m["by_call"] != true || m["basis"] != "bycall" {
		t.Fatalf("byprice flags wrong: %v", m)
	}
	// 100000/(0.05*500000)=4 次。
	if diff := m["est_calls"].(float64) - 4; diff > 1e-6 || diff < -1e-6 {
		t.Fatalf("est_calls = %v, want 4", m["est_calls"])
	}
	if m["est_tokens"].(float64) != 0 {
		t.Fatalf("byprice est_tokens should be 0: %v", m["est_tokens"])
	}
}

func itoa(i int) string { return strconv.Itoa(i) }

// ---- 94b: 密钥设置(额度/模型/IP) ----

func TestHybUpdateKeyHandler(t *testing.T) {
	d := newCenterTestDB(t)
	uid := centerUser(t, d, "u94d", 100)
	ensureDefaultToken(uid)
	var def model.Token
	d.Where("user_id = ? AND name = ?", uid, hybDefaultTokenName).First(&def)

	// 建一个普通密钥
	c, w := centerCtx(t, uid)
	c.Request = httptest.NewRequest(http.MethodPost, "/hybapi/keys", bytes.NewBufferString(`{"name":"ag"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	hybCreateKeyHandler(c)
	created := bodyJSON(t, w)
	kid := int(created["id"].(float64))

	put := func(id int, js string) *httptest.ResponseRecorder {
		c, w := centerCtx(t, uid)
		c.Params = gin.Params{{Key: "id", Value: itoa(id)}}
		c.Request = httptest.NewRequest(http.MethodPut, "/hybapi/keys/x", bytes.NewBufferString(js))
		c.Request.Header.Set("Content-Type", "application/json")
		hybUpdateKeyHandler(c)
		return w
	}

	// default 保持无限不限,拒绝编辑
	if w := put(def.Id, `{"unlimited_quota":true}`); w.Code != http.StatusBadRequest {
		t.Fatalf("edit default = %d, want 400: %s", w.Code, w.Body.String())
	}
	// historyhub-web 同样拒绝
	web := model.Token{UserId: uid, Name: webTokenName, Key: "wt", Status: 1, CreatedTime: 1, ExpiredTime: -1, UnlimitedQuota: true}
	d.Create(&web)
	if w := put(web.Id, `{"unlimited_quota":true}`); w.Code != http.StatusBadRequest {
		t.Fatalf("edit web token = %d, want 400", w.Code)
	}
	// 限定额度但没给金额 / 负数
	if w := put(kid, `{"unlimited_quota":false}`); w.Code != http.StatusBadRequest {
		t.Fatalf("missing usd = %d", w.Code)
	}
	if w := put(kid, `{"unlimited_quota":false,"remain_usd":-1}`); w.Code != http.StatusBadRequest {
		t.Fatalf("negative usd = %d", w.Code)
	}
	// 开模型限制但没选模型
	if w := put(kid, `{"model_limits_enabled":true,"model_limits":[]}`); w.Code != http.StatusBadRequest {
		t.Fatalf("empty models = %d", w.Code)
	}
	// 非法 IP
	if w := put(kid, `{"allow_ips":["not-an-ip"]}`); w.Code != http.StatusBadRequest {
		t.Fatalf("bad ip = %d", w.Code)
	}
	// 合法完整保存:额度 $1.5 + 模型 + IP(换行/逗号混合输入)
	w = put(kid, `{"unlimited_quota":false,"remain_usd":1.5,"model_limits_enabled":true,"model_limits":["m-a","m-b","m-a"," "],"allow_ips":["1.2.3.4","10.0.0.0/8"]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("good put = %d: %s", w.Code, w.Body.String())
	}
	var row model.Token
	d.First(&row, kid)
	if row.UnlimitedQuota || row.RemainQuota != 750000 {
		t.Fatalf("quota flags wrong: unlimited=%v remain=%d", row.UnlimitedQuota, row.RemainQuota)
	}
	if !row.ModelLimitsEnabled || row.ModelLimits != "m-a,m-b" {
		t.Fatalf("model limits wrong: %q", row.ModelLimits)
	}
	if row.AllowIps == nil || *row.AllowIps != "1.2.3.4\n10.0.0.0/8" {
		t.Fatalf("allow ips wrong: %v", row.AllowIps)
	}
	// 响应回读
	data := bodyJSON(t, w)["data"].(map[string]interface{})
	if data["unlimited_quota"] != false || data["remain_quota"].(float64) != 750000 {
		t.Fatalf("resp view wrong: %v", data)
	}
	if ml, _ := data["model_limits"].([]interface{}); len(ml) != 2 {
		t.Fatalf("resp model_limits wrong: %v", data["model_limits"])
	}
	if ai, _ := data["allow_ips"].([]interface{}); len(ai) != 2 {
		t.Fatalf("resp allow_ips wrong: %v", data["allow_ips"])
	}
	// 恢复无限:remain 清零
	if w := put(kid, `{"unlimited_quota":true}`); w.Code != http.StatusOK {
		t.Fatalf("back to unlimited = %d", w.Code)
	}
	d.First(&row, kid)
	if !row.UnlimitedQuota || row.RemainQuota != 0 {
		t.Fatalf("unlimited reset wrong: %+v", row)
	}
}

func TestTokenIPAllowed(t *testing.T) {
	mk := func(s string) *model.Token {
		tok := &model.Token{}
		if s != "" {
			tok.AllowIps = &s
		}
		return tok
	}
	if !tokenIPAllowed("1.2.3.4", mk("1.2.3.4\n10.0.0.0/8")) {
		t.Fatal("exact ip should pass")
	}
	if !tokenIPAllowed("10.0.3.4", mk("1.2.3.4\n10.0.0.0/8")) {
		t.Fatal("cidr member should pass")
	}
	if tokenIPAllowed("9.9.9.9", mk("1.2.3.4\n10.0.0.0/8")) {
		t.Fatal("outside ip should fail")
	}
	if tokenIPAllowed("1.2.3.4", mk("9.9.9.9")) {
		t.Fatal("not-in-list should fail")
	}
	if !tokenIPAllowed("anything", mk("")) {
		t.Fatal("no limits should always pass")
	}
	if tokenIPAllowed("bogus-ip", mk("1.2.3.4")) {
		t.Fatal("unparseable client ip should fail")
	}
}
