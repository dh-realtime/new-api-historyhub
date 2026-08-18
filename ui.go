package historyhub

import (
	_ "embed"
	"net/http"

	"github.com/gin-gonic/gin"
)

//go:embed ui.html
var uiHTML []byte

func serveUI(c *gin.Context) {
	c.Data(http.StatusOK, "text/html; charset=utf-8", uiHTML)
}
