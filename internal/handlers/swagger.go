package handlers

import (
	"fmt"
	"net/http"

	"gopkg.in/yaml.v3"

	"github.com/monzim/docpipe/internal/config"
	"github.com/monzim/docpipe/internal/webassets"
)

// swaggerHTML is the minimal Swagger UI shell.
//
// We intentionally load swagger-ui-dist from a CDN here — the dashboard
// avoids CDNs because it's user-facing and gated public, but the OpenAPI
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

// SwaggerHandler holds the OpenAPI spec bytes that get served at
// /swagger/openapi.yaml. The bytes are computed once at startup with
// extra servers from DOCPIPE_SWAGGER_SERVERS prepended to the YAML's
// defaults, then cached — every request returns the same bytes.
type SwaggerHandler struct {
	specBytes []byte
}

// NewSwaggerHandler reads the embedded OpenAPI YAML, prepends the env-driven
// servers to whatever the YAML already lists, and caches the result.
//
// Order in the served spec: [env servers..., YAML defaults...]. So the
// Swagger UI's "Servers" dropdown shows env-supplied URLs first — that
// matches the operator intent (env represents the live deployment;
// YAML defaults are documentation).
func NewSwaggerHandler(extraServers []config.SwaggerServer) (*SwaggerHandler, error) {
	raw := webassets.OpenAPI()

	// Parse into a yaml.Node so we can replace just the `servers` block
	// without disturbing the rest of the document's key order.
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse embedded openapi: %w", err)
	}

	// Top-level Document → Mapping. Walk pairs to find "servers".
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("openapi spec has unexpected top-level shape")
	}
	root := doc.Content[0]

	var serversValue *yaml.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "servers" {
			serversValue = root.Content[i+1]
			break
		}
	}
	if serversValue == nil {
		return nil, fmt.Errorf("openapi spec missing `servers` block")
	}

	// Pull existing entries (the YAML defaults) so we keep them after env extras.
	defaults, err := readServers(serversValue)
	if err != nil {
		return nil, fmt.Errorf("read default servers: %w", err)
	}

	final := make([]config.SwaggerServer, 0, len(extraServers)+len(defaults))
	final = append(final, extraServers...)
	final = append(final, defaults...)

	// Replace the value node in place.
	*serversValue = buildServersNode(final)

	out, err := yaml.Marshal(&doc)
	if err != nil {
		return nil, fmt.Errorf("re-marshal openapi: %w", err)
	}
	return &SwaggerHandler{specBytes: out}, nil
}

// UI serves the small HTML shell that loads swagger-ui-dist and points it at
// /swagger/openapi.yaml.
func (h *SwaggerHandler) UI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(swaggerHTML))
}

// Spec serves the (cached) OpenAPI YAML with servers injected at startup.
func (h *SwaggerHandler) Spec(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(h.specBytes)
}

// readServers extracts existing `servers:` entries from a yaml.Node.
// Each entry is a mapping with `url` and optional `description`.
func readServers(n *yaml.Node) ([]config.SwaggerServer, error) {
	if n.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("servers must be a sequence, got kind=%d", n.Kind)
	}
	var out []config.SwaggerServer
	for _, item := range n.Content {
		if item.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("server entry must be a mapping")
		}
		var s config.SwaggerServer
		for i := 0; i+1 < len(item.Content); i += 2 {
			switch item.Content[i].Value {
			case "url":
				s.URL = item.Content[i+1].Value
			case "description":
				s.Description = item.Content[i+1].Value
			}
		}
		out = append(out, s)
	}
	return out, nil
}

// buildServersNode produces a yaml.Node representing the full `servers:`
// sequence. Each item is `{url: ..., description: ...}` (description omitted
// when empty so the YAML stays clean).
func buildServersNode(servers []config.SwaggerServer) yaml.Node {
	seq := yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	for _, s := range servers {
		item := yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
		item.Content = append(item.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "url"},
			&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s.URL},
		)
		if s.Description != "" {
			item.Content = append(item.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "description"},
				&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: s.Description},
			)
		}
		seq.Content = append(seq.Content, &item)
	}
	return seq
}
