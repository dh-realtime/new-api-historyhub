package historyhub

import (
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"

	"github.com/gin-gonic/gin"
)

// init runs at program start because main.go blank-imports this package.
// Starting the listener in its own goroutine keeps it out of the way of
// main()'s own initialization; handlers only touch new-api globals
// (model.*, common.Port) at request time, by which point main has finished.
func init() {
	go start()
}

func start() {
	// init() runs before main(), so wait for the main process to finish
	// initializing its database; otherwise model.GetTokenByKey would hit a nil
	// *gorm.DB on requests arriving during startup.
	for i := 0; i < 600; i++ {
		if model.DB != nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	addr := hybAddr()
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())
	// 94b: 与主服务同一套受信代理配置。gin 默认信任所有代理,客户端可伪造
	// X-Forwarded-For 绕过密钥 IP 白名单;收紧后 c.ClientIP() 才是真实来源。
	if err := middleware.ConfigureTrustedProxies(r); err != nil {
		common.SysError("historyhub: configure trusted proxies failed: " + err.Error())
	}
	registerRoutes(r)
	common.SysLog("historyhub: sen-history service listening on " + addr)
	srv := &http.Server{Addr: addr, Handler: r}
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		common.SysError("historyhub: server stopped: " + err.Error())
	}
}

// authMiddleware resolves the API key to a user id and stores it in the
// context. Anything under /history requires a valid new-api API key.
func authMiddleware(c *gin.Context) {
	uid, _, _ := resolveUser(c)
	if uid == 0 {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": gin.H{"message": "invalid or missing api key"}})
		return
	}
	c.Set("uid", uid)
	c.Next()
}

func registerRoutes(r *gin.Engine) {
	// Web chat frontend (native html/css/js under 0/web, go:embed'ed in
	// web.go). The old single-file ui.html UI is gone.
	r.GET("/", serveWebIndex)
	r.GET("/history", serveWebIndex)
	r.GET("/web/*rest", serveWebAsset)

	// Browser-session API used by the 0/web frontend (username+password
	// login, sessions, uploads, and a chat shim that re-enters the recording
	// proxy path). See webauth.go.
	r.POST("/hybapi/login", hybLoginHandler)
	hyb := r.Group("/hybapi", webAuthMiddleware)
	{
		hyb.POST("/logout", hybLogoutHandler)
		hyb.GET("/me", hybMeHandler)
		hyb.GET("/models", hybModelsHandler)
		hyb.GET("/sen", hybListSenHandler)
		hyb.GET("/sen/:id/messages", hybMessagesHandler)
		hyb.PATCH("/sen/:id", hybRenameSenHandler)
		hyb.PUT("/sen/:id", hybRenameSenHandler)
		hyb.DELETE("/sen/:id", hybDeleteSenHandler)
		hyb.POST("/upload", hybUploadHandler)
		hyb.GET("/file/:id", hybFileHandler)
		hyb.POST("/chat/completions", hybChatHandler)

		// 93: 修改密码;94: 简化版 API 密钥管理;95: 只读钱包。
		hyb.POST("/password", hybPasswordHandler)
		hyb.GET("/keys", hybListKeysHandler)
		hyb.POST("/keys", hybCreateKeyHandler)
		hyb.GET("/keys/:id", hybRevealKeyHandler)
		hyb.PUT("/keys/:id", hybUpdateKeyHandler)
		hyb.PATCH("/keys/:id", hybUpdateKeyHandler)
		hyb.DELETE("/keys/:id", hybDeleteKeyHandler)
		hyb.GET("/wallet", hybWalletHandler)
	}

	api := r.Group("/history", authMiddleware)
	{
		api.GET("/sen", listSenHandler)
		api.POST("/sen", createSenHandler)
		api.GET("/sen/:id/messages", getMessagesHandler)
		api.DELETE("/sen/:id", deleteSenHandler)
		api.PATCH("/sen/:id", renameSenHandler)
		api.PUT("/sen/:id", renameSenHandler)

		// Serve saved attachments (q_fil / a_fil) from hybfil/.
		api.GET("/file/:id", serveFileHandler)
	}

	// Recorded chat relay — drop-in replacement for the main /v1/chat/completions.
	// Point a client's base_url at :3001 and sessions are kept automatically.
	r.POST("/v1/chat/completions", func(c *gin.Context) { forward(c, true) })

	// Transparent passthrough for every other path (models, embeddings, keys
	// API, ...). Lets :3001 act as a full base_url without recording.
	r.NoRoute(func(c *gin.Context) { forward(c, false) })
}

func listSenHandler(c *gin.Context) {
	convs, _ := listSen(c.GetInt("uid"))
	c.JSON(http.StatusOK, gin.H{"data": convs})
}

func createSenHandler(c *gin.Context) {
	var body struct {
		Title string `json:"title"`
	}
	_ = c.ShouldBindJSON(&body)
	if body.Title == "" {
		body.Title = "New chat"
	}
	// Manual creation via this API is a web/UI action -> isKey follows the
	// marker header (0 from the historyhub UI, 1 from an external caller).
	conv, err := createSen(c.GetInt("uid"), 0, detectIsKey(c), body.Title)
	if err != nil || conv == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": "failed to create session"}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": conv})
}

func getMessagesHandler(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "invalid id"}})
		return
	}
	msgs, err := getMessages(c.GetInt("uid"), id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"message": "session not found"}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": msgs})
}

func deleteSenHandler(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "invalid id"}})
		return
	}
	if err := deleteSen(c.GetInt("uid"), id); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"message": "session not found"}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": true})
}

func renameSenHandler(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "invalid id"}})
		return
	}
	var body struct {
		Title string `json:"title"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Title == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "title required"}})
		return
	}
	if err := renameSen(c.GetInt("uid"), id, body.Title); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"message": "session not found"}})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": true})
}

// serveFileHandler streams a registered attachment back to the client. Auth is
// any valid new-api key (files aren't re-scoped per user in httpFile.db).
func serveFileHandler(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "invalid id"}})
		return
	}
	hf, ok := httpFileById(id)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"message": "file not found"}})
		return
	}
	full := filepath.Join(fileDir(), hf.FPath)
	f, err := os.Open(full)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"message": "file missing on disk"}})
		return
	}
	defer f.Close()
	c.Header("Content-Disposition", `attachment; filename="`+hf.FNam+`"`)
	http.ServeContent(c.Writer, c.Request, hf.FNam, time.Time{}, f)
}
