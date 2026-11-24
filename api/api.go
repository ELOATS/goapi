// Package api 内嵌 OpenAPI 规范，并提供规范文件和 Swagger UI 的 HTTP 处理器。
package api

import (
	_ "embed"
	"net/http"
)

//go:embed openapi.yaml
var Spec []byte

// SpecHandler 返回 openapi.yaml。
func SpecHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(Spec)
}

// Swagger UI 从 CDN 加载。内网或离线环境请把 swagger-ui-dist 下载下来自己托管，并修改这两个地址。
const uiHTML = `<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>goapi API 文档</title>
  <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5/swagger-ui.css">
</head>
<body>
  <div id="swagger-ui"></div>
  <script src="https://unpkg.com/swagger-ui-dist@5/swagger-ui-bundle.js" crossorigin></script>
  <script>
    window.ui = SwaggerUIBundle({
      url: "openapi.yaml",          // 相对于 /api/docs，即 /api/openapi.yaml
      dom_id: "#swagger-ui",
      deepLinking: true,
      persistAuthorization: true,   // 刷新页面后保留填写的 token
    });
  </script>
</body>
</html>
`

// UIHandler 返回 Swagger UI 页面，点右上角 Authorize 填入 token 后可以直接调用接口。
func UIHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write([]byte(uiHTML))
}
