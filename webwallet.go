package historyhub

// 95(用户消息第二个 94): 网页端钱包 —— 只读展示,无充值。
//   941: 每个模型已消耗的 tokens(输入/输出)与已花费金额;
//   942: 剩余金额若单独用在某模型上,≈还能用多少 tokens、≈还能对话多少次。
//
// 数据源全部只读:
//   - 剩余/累计:user.Quota / user.UsedQuota(主站 users 表行);
//   - 逐模型消耗:主站 logs 表(type=2 消费日志)按 model_name 聚合
//     SUM(quota)/SUM(prompt_tokens)/SUM(completion_tokens)/COUNT;
//   - 牌价:setting/ratio_setting 的 GetModelRatioOrPrice(倍率或按次价格)。
//
// 估算口径(界面标明"估算"):
//   - 有消耗记录:按该模型历史平均单价(总配额/总 tokens)与平均每次消耗;
//   - 无记录:按典型对话规模 输入1K+输出0.5K tokens 结合牌价;
//   - 按次计费模型只估次数,不估 tokens;
//   - 查不到牌价的模型不做估算(前端显示 —)。
// 需要估算的候选模型列表由前端 ?models=a,b,c 传入(即 /hybapi/models 已返回、
// 用户实际可用的模型),与聊天模型下拉完全一致。

import (
	"net/http"
	"sort"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/ratio_setting"

	"github.com/gin-gonic/gin"
)

// 典型单次对话规模(无消耗记录模型的估算基准)。
const (
	walletTypicalPromptTok     = 1000.0
	walletTypicalCompletionTok = 500.0
)

type walletModelRow struct {
	Model            string  `json:"model"`
	AModel           string  `json:"a_model"` // 渠道/模型 展示名,与聊天下拉一致
	Used             bool    `json:"used"`
	Calls            int64   `json:"calls"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	Quota            int64   `json:"quota"`      // 已花费(配额)
	EstTokens        float64 `json:"est_tokens"` // 0=不适用
	EstCalls         float64 `json:"est_calls"`  // 0=不适用
	ByCall           bool    `json:"by_call"`    // 按次计费 → est_tokens 不适用
	Basis            string  `json:"basis"`      // history|typical|bycall|none
}

// estFromQuota: 给定剩余配额与"每一 token 的平均配额/每一次的平均配额"。
func estFromQuota(remain float64, perToken, perCall float64) (tokens, calls float64) {
	if perToken > 0 {
		tokens = remain / perToken
	}
	if perCall > 0 {
		calls = remain / perCall
	}
	return
}

func hybWalletHandler(c *gin.Context) {
	uid := c.GetInt("uid")
	user, err := model.GetUserById(uid, true)
	if err != nil || user == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": gin.H{"message": "用户不存在"}})
		return
	}

	// 逐模型消耗聚合(只读)。
	var aggs []struct {
		ModelName        string `json:"model_name"`
		Calls            int64  `json:"calls"`
		Quota            int64  `json:"quota"`
		PromptTokens     int64  `json:"prompt_tokens"`
		CompletionTokens int64  `json:"completion_tokens"`
	}
	if err := model.DB.Model(&model.Log{}).
		Select("model_name, COUNT(*) as calls, SUM(quota) as quota, SUM(prompt_tokens) as prompt_tokens, SUM(completion_tokens) as completion_tokens").
		Where("user_id = ? AND type = ?", uid, model.LogTypeConsume).
		Group("model_name").
		Scan(&aggs).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": "聚合消费记录失败"}})
		return
	}

	remain := float64(user.Quota)
	rows := make([]walletModelRow, 0, len(aggs)+8)
	byModel := make(map[string]walletModelRow, len(aggs)+8)

	for _, a := range aggs {
		name := strings.TrimSpace(a.ModelName)
		if name == "" {
			continue
		}
		row := walletModelRow{
			Model:            name,
			AModel:           resolveAModel(uid, name),
			Used:             true,
			Calls:            a.Calls,
			PromptTokens:     a.PromptTokens,
			CompletionTokens: a.CompletionTokens,
			Quota:            a.Quota,
			Basis:            "history",
		}
		totalTok := float64(a.PromptTokens + a.CompletionTokens)
		perTok, perCall := 0.0, 0.0
		if totalTok > 0 {
			perTok = float64(a.Quota) / totalTok
		}
		if a.Calls > 0 {
			perCall = float64(a.Quota) / float64(a.Calls)
		}
		row.EstTokens, row.EstCalls = estFromQuota(remain, perTok, perCall)
		if perTok == 0 && perCall == 0 {
			row.Basis = "none" // 全零账单估不出
		}
		rows = append(rows, row)
		byModel[name] = row
	}

	// 前端传来的可用模型(逗号分隔),补齐"用过/没用过"的估算行。
	if raw := strings.TrimSpace(c.Query("models")); raw != "" {
		for _, name := range strings.Split(raw, ",") {
			name = strings.TrimSpace(name)
			if name == "" || len(name) > 190 {
				continue
			}
			if _, ok := byModel[name]; ok {
				continue // 已有消耗记录,历史口径优先
			}
			if len(byModel) >= 300 {
				break
			}
			row := walletModelRow{Model: name, AModel: resolveAModel(uid, name), Basis: "none"}
			if val, usePrice, exist := ratio_setting.GetModelRatioOrPrice(name); exist {
				if usePrice {
					// 按次计费:每次固定配额 = 价格$ × QuotaPerUnit。
					row.ByCall = true
					row.Basis = "bycall"
					row.EstCalls = remain / (val * common.QuotaPerUnit)
				} else {
					cr := ratio_setting.GetCompletionRatio(name)
					perCall := walletTypicalPromptTok*val + walletTypicalCompletionTok*val*cr
					perTok := perCall / (walletTypicalPromptTok + walletTypicalCompletionTok)
					row.EstTokens, row.EstCalls = estFromQuota(remain, perTok, perCall)
					row.Basis = "typical"
				}
			}
			rows = append(rows, row)
			byModel[name] = row
		}
	}

	// 已花费的排前面(按花费降序),其后未用的按名称排。
	sortWalletRows(rows)

	c.JSON(http.StatusOK, gin.H{
		"remain_quota": user.Quota,
		"used_quota":   user.UsedQuota,
		"per_unit":     common.QuotaPerUnit,
		"models":       rows,
	})
}

func sortWalletRows(rows []walletModelRow) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.Used != b.Used {
			return a.Used // 用过的排前面
		}
		if a.Quota != b.Quota {
			return a.Quota > b.Quota // 花费多在前;未用的同按此稳定排序
		}
		return a.Model < b.Model
	})
}
