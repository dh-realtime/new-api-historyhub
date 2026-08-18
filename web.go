package historyhub

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// The web chat frontend: plain html/css/js under 0/web (no bundler, no
// framework), embedded into the binary so new-api.exe stays self-contained
// (02.md E01). Layout and interactions follow the open-webui chat page:
// left session sidebar, right conversation area, multimodal in/out.
//
//go:embed web
var webEmbed embed.FS

var webFS, _ = fs.Sub(webEmbed, "web")

var webTypes = map[string]string{
	".html":  "text/html; charset=utf-8",
	".css":   "text/css; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".svg":   "image/svg+xml",
	".png":   "image/png",
	".ico":   "image/x-icon",
	".json":  "application/json",
	".woff2": "font/woff2",
}

func webContentType(name string) string {
	if i := strings.LastIndex(name, "."); i >= 0 {
		if t, ok := webTypes[strings.ToLower(name[i:])]; ok {
			return t
		}
	}
	return "application/octet-stream"
}

func serveWebIndex(c *gin.Context) {
	b, err := fs.ReadFile(webFS, "index.html")
	if err != nil {
		c.String(http.StatusNotFound, "frontend missing")
		return
	}
	c.Data(http.StatusOK, "text/html; charset=utf-8", b)
}

// serveWebAsset serves /web/<file> from the embedded frontend. Path traversal
// is impossible: the name is looked up in the embed FS, never on disk.
func serveWebAsset(c *gin.Context) {
	name := strings.TrimPrefix(c.Param("rest"), "/")
	if name == "" || strings.Contains(name, "..") {
		c.String(http.StatusNotFound, "not found")
		return
	}
	b, err := fs.ReadFile(webFS, name)
	if err != nil {
		c.String(http.StatusNotFound, "not found")
		return
	}
	c.Header("Cache-Control", "no-cache")
	c.Data(http.StatusOK, webContentType(name), b)
}
