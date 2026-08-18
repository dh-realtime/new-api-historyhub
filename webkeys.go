package historyhub

// 94: 简化版 API 密钥管理(网页端)。
//
// 复用主站 model.Token 表与函数(GenerateKey/Insert/DeleteTokenById 等,零新表)。
// 约定:
//   - 登录成功即 ensureDefaultToken:该用户名下没有名为 "default" 的密钥时自动
//     创建一个(永不过期/不限额度/不限模型),default 不可删除 —— 让 openclaw/
//     hermes/codex 等 agent 不登录网页也能直接用 sk- 密钥走 :3001 调模型;
//   - 网页对话专用的 "historyhub-web" 密钥是系统内部令牌,列表不展示、也不可删;
//   - 其余密钥可新建/删除。密钥列表只回脱敏形式,复制时再按 id 取完整密钥。

import (
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"

	"github.com/gin-gonic/gin"
)

const hybDefaultTokenName = "default"

// ensureDefaultToken 幂等:存在同名(未软删)default 即跳过,失败只记日志不阻断登录。
func ensureDefaultToken(userId int) {
	var cnt int64
	if err := model.DB.Model(&model.Token{}).Where("user_id = ? AND name = ?", userId, hybDefaultTokenName).Count(&cnt).Error; err != nil {
		common.SysLog("ensureDefaultToken count failed: " + err.Error())
		return
	}
	if cnt > 0 {
		return
	}
	key, err := common.GenerateKey()
	if err != nil {
		common.SysLog("ensureDefaultToken genkey failed: " + err.Error())
		return
	}
	nowTS := common.GetTimestamp()
	t := model.Token{
		UserId:         userId,
		Name:           hybDefaultTokenName,
		Key:            key,
		Status:         common.TokenStatusEnabled,
		CreatedTime:    nowTS,
		AccessedTime:   nowTS,
		ExpiredTime:    -1,
		UnlimitedQuota: true,
	}
	if err := t.Insert(); err != nil {
		common.SysLog("ensureDefaultToken insert failed: " + err.Error())
	}
}

type hybKeyView struct {
	Id             int      `json:"id"`
	Name           string   `json:"name"`
	Masked         string   `json:"masked"` // "sk-" + 前4****后4
	CreatedAt      int64    `json:"created_at"`
	UsedQuota      int      `json:"used_quota"`
	IsDefault      bool     `json:"is_default"`
	UnlimitedQuota bool     `json:"unlimited_quota"`
	RemainQuota    int      `json:"remain_quota"`
	ModelLimitsOn  bool     `json:"model_limits_enabled"`
	ModelLimits    []string `json:"model_limits"`
	AllowIps       []string `json:"allow_ips"`
}

func keyViewOf(t *model.Token) hybKeyView {
	v := hybKeyView{
		Id:             t.Id,
		Name:           t.Name,
		Masked:         "sk-" + model.MaskTokenKey(t.Key),
		CreatedAt:      t.CreatedTime,
		UsedQuota:      t.UsedQuota,
		IsDefault:      t.Name == hybDefaultTokenName,
		UnlimitedQuota: t.UnlimitedQuota,
		RemainQuota:    t.RemainQuota,
		ModelLimitsOn:  t.ModelLimitsEnabled,
		ModelLimits:    t.GetModelLimits(),
		AllowIps:       t.GetIpLimits(),
	}
	if v.ModelLimits == nil {
		v.ModelLimits = []string{}
	}
	if v.AllowIps == nil {
		v.AllowIps = []string{}
	}
	return v
}

func hybListKeysHandler(c *gin.Context) {
	uid := c.GetInt("uid")
	tokens, err := model.GetAllUserTokens(uid, 0, 100)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": "读取密钥列表失败"}})
		return
	}
	out := make([]hybKeyView, 0, len(tokens))
	for _, t := range tokens {
		if t == nil || t.Name == webTokenName {
			continue
		}
		out = append(out, keyViewOf(t))
	}
	// default 永远排第一,其余按创建时间倒序(新的在前)。
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].IsDefault != out[j].IsDefault {
			return out[i].IsDefault
		}
		return out[i].Id > out[j].Id
	})
	c.JSON(http.StatusOK, gin.H{"data": out})
}

func hybCreateKeyHandler(c *gin.Context) {
	uid := c.GetInt("uid")
	var body struct {
		Name string `json:"name"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "请求参数错误"}})
		return
	}
	name := []rune(strings.TrimSpace(body.Name))
	if len(name) == 0 {
		name = []rune("新密钥")
	}
	if len(name) > 50 {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "名称最长 50 个字符"}})
		return
	}
	maxTokens := operation_setting.GetMaxUserTokens()
	if cnt, err := model.CountUserTokens(uid); err == nil && int(cnt) >= maxTokens {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "已达到密钥数量上限(" + strconv.Itoa(maxTokens) + "个)"}})
		return
	}
	key, err := common.GenerateKey()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": "生成密钥失败"}})
		return
	}
	nowTS := common.GetTimestamp()
	t := model.Token{
		UserId:         uid,
		Name:           string(name),
		Key:            key,
		Status:         common.TokenStatusEnabled,
		CreatedTime:    nowTS,
		AccessedTime:   nowTS,
		ExpiredTime:    -1,
		UnlimitedQuota: true,
	}
	if err := t.Insert(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": "保存密钥失败"}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"id": t.Id, "key": "sk-" + t.Key})
}

// hybRevealKeyHandler 按 id 取完整密钥(复制按钮用)。只允许本人。
func hybRevealKeyHandler(c *gin.Context) {
	uid := c.GetInt("uid")
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "密钥 id 无效"}})
		return
	}
	t, err := model.GetTokenByIds(id, uid)
	if err != nil || t == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"message": "密钥不存在"}})
		return
	}
	if t.Name == webTokenName {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "系统内部密钥不对外提供"}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"id": t.Id, "name": t.Name, "key": "sk-" + t.Key})
}

func hybDeleteKeyHandler(c *gin.Context) {
	uid := c.GetInt("uid")
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "密钥 id 无效"}})
		return
	}
	t, err := model.GetTokenByIds(id, uid)
	if err != nil || t == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"message": "密钥不存在"}})
		return
	}
	if t.Name == hybDefaultTokenName {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "默认密钥不可删除"}})
		return
	}
	if t.Name == webTokenName {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "系统内部密钥不可删除"}})
		return
	}
	if err := model.DeleteTokenById(id, uid); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": "删除失败"}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// hybUpdateKeyHandler: 94b 编辑密钥的 额度/模型限制/IP 白名单(与主站同字段同校验,
// 其余功能一律不做)。default 与 historyhub-web 保持无限/不限/不限,不接受编辑。
// 额度按美元金额传入(remain_usd,内部 × QuotaPerUnit 换算配额);模型限制为勾选的
// 模型名数组;IP 白名单支持 IP 与 CIDR 网段。保存走 model.Token.Update()(自动失效缓存)。
func hybUpdateKeyHandler(c *gin.Context) {
	uid := c.GetInt("uid")
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "密钥 id 无效"}})
		return
	}
	var body struct {
		UnlimitedQuota *bool    `json:"unlimited_quota"`
		RemainUSD      *float64 `json:"remain_usd"`
		ModelLimitsOn  *bool    `json:"model_limits_enabled"`
		ModelLimits    []string `json:"model_limits"`
		AllowIps       []string `json:"allow_ips"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "请求参数错误"}})
		return
	}
	t, err := model.GetTokenByIds(id, uid)
	if err != nil || t == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"message": "密钥不存在"}})
		return
	}
	if t.Name == hybDefaultTokenName || t.Name == webTokenName {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "默认密钥保持 不限额度/不限模型/不限 IP，不可编辑"}})
		return
	}

	if body.UnlimitedQuota != nil {
		t.UnlimitedQuota = *body.UnlimitedQuota
		t.RemainQuota = 0
		if !t.UnlimitedQuota {
			if body.RemainUSD == nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "请填写限定额度（美元）"}})
				return
			}
			if *body.RemainUSD < 0 {
				c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "额度不能为负数"}})
				return
			}
			// 与主站 AddToken 相同的上限(10 亿刀);浮点换算四舍五入。
			quota := common.QuotaFromFloat(*body.RemainUSD * common.QuotaPerUnit)
			maxQuota := common.QuotaFromFloat(1000000000 * common.QuotaPerUnit)
			if quota > maxQuota {
				c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "额度超出上限"}})
				return
			}
			t.RemainQuota = quota
		}
	}
	if body.ModelLimitsOn != nil {
		t.ModelLimitsEnabled = *body.ModelLimitsOn
		t.ModelLimits = ""
		if t.ModelLimitsEnabled {
			seen := make(map[string]bool, len(body.ModelLimits))
			kept := make([]string, 0, len(body.ModelLimits))
			for _, m := range body.ModelLimits {
				m = strings.TrimSpace(m)
				if m == "" || len(m) > 190 || seen[m] {
					continue
				}
				seen[m] = true
				kept = append(kept, m)
			}
			if len(kept) == 0 {
				c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "开启了模型限制但没有勾选任何模型"}})
				return
			}
			if len(kept) > 100 {
				c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "最多勾选 100 个模型"}})
				return
			}
			t.ModelLimits = strings.Join(kept, ",")
		}
	}
	if body.AllowIps != nil {
		kept := make([]string, 0, len(body.AllowIps))
		for _, s := range body.AllowIps {
			s = strings.TrimSpace(s)
			if s == "" {
				continue
			}
			if net.ParseIP(s) == nil {
				if _, _, err := net.ParseCIDR(s); err != nil {
					c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "IP 白名单格式不对：" + s + "（支持 IP 如 1.2.3.4 或网段如 10.0.0.0/8）"}})
					return
				}
			}
			kept = append(kept, s)
		}
		if len(kept) > 100 {
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "IP 白名单最多 100 条"}})
			return
		}
		// 主站 GetIpLimits 按换行拆分(逗号会被剔除),必须用 \n 连接。
		ips := strings.Join(kept, "\n")
		t.AllowIps = &ips
	}

	if err := t.Update(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": "保存失败"}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": keyViewOf(t)})
}

// tokenIPAllowed: 与主站 TokenAuth 相同语义的 IP 白名单判定(复用 GetIpLimits +
// IsIpInCIDRList),在 :3001 入口对 agent 直连提前拦截。
func tokenIPAllowed(clientIP string, t *model.Token) bool {
	limits := t.GetIpLimits()
	if len(limits) == 0 {
		return true
	}
	ip := net.ParseIP(clientIP)
	if ip == nil {
		return false
	}
	return common.IsIpInCIDRList(ip, limits)
}
