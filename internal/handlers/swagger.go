package handlers

import (
	"net/http"

	"github.com/monzim/docpipe/internal/webassets"
)

// swaggerHTML is the minimal Swagger UI shell.
//
// We intentionally load swagger-ui-dist from a CDN here — the spec calls for
// "no external CDN if you can avoid it" on the dashboard, but the OpenAPI
// surface is dev-facing and gated by DOCPIPE_ENABLE_SWAGGER. The cost of
// vendoring swagger-ui-dist (~1MB) into the binary isn't worth it for a
// dev tool that's off by default in production.
const swaggerHTML = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <title>DocPipe — API</title>
  <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5/swagger-ui.css">
  <style>body{margin:0}</style>
</head>
<body>
<div id="swagger-ui"></div>
<script src="https://unpkg.com/swagger-ui-dist@5/swagger-ui-bundle.js"></script>
<script>
  window.onload = () => {
    window.ui = SwaggerUIBundle({
      url: "/swagger/openapi.yaml",
      dom_id: "#swagger-ui",
      deepLinking: true,
      docExpansion: "list",
    });
  };
</script>
</body>
</html>`

// SwaggerUI serves a tiny HTML shell that loads swagger-ui-dist from a CDN
// and points it at /swagger/openapi.yaml. Gated by config in main.go.
func SwaggerUI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(swaggerHTML))
}

// SwaggerSpec serves the embedded OpenAPI YAML.
func SwaggerSpec(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(webassets.OpenAPI())
}
