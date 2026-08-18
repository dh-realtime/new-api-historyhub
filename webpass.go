package historyhub

// 93: 网页端修改密码(对应主站 /profile 的"更改密码")。
//
// 校验与写库全部复用主站已有函数:common.ValidatePasswordAndHash 比对原密码,
// model.User.Update(true) 负责 bcrypt 加密、按需递增 auth_version 并吊销主站
// 旧会话、刷新用户缓存 —— 与主站改密行为一致(N06/N04:只改 users 表行,不动表结构)。
// 91 的万能密码只用于登录,此处不接受:改密码必须提供账号本身的当前密码。
// 本网页会话存 hybdb,与密码无关,改密后无需重新登录。

import (
	"net/http"
	"strconv"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"

	"github.com/gin-gonic/gin"
)

func hybPasswordHandler(c *gin.Context) {
	uid := c.GetInt("uid")
	var body struct {
		Original string `json:"original"`
		Password string `json:"password"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Original == "" || body.Password == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "请填写原密码和新密码"}})
		return
	}
	current, err := model.GetUserById(uid, true)
	if err != nil || current == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": gin.H{"message": "用户不存在"}})
		return
	}
	if current.Password == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "当前账号未设置密码，请联系管理员"}})
		return
	}
	if !common.ValidatePasswordAndHash(body.Original, current.Password) {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "原密码不正确"}})
		return
	}
	// 与主站同一校验规则(User.Password 的 validate 标签: 8~20 位)。
	if utf8Len(body.Password) < 8 || utf8Len(body.Password) > 20 {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "新密码长度需为 8~20 个字符"}})
		return
	}
	upd := model.User{Id: uid, Password: body.Password}
	if err := upd.Update(true); err != nil {
		common.SysLog("web password change failed for uid " + strconv.Itoa(uid) + ": " + err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": "修改失败，请稍后重试"}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func utf8Len(s string) int { return len([]rune(s)) }
