// Package webassets owns the static files embedded into the binary —
// the dashboard HTML and the OpenAPI spec.
//
// The package exists so `go:embed` directives sit next to the assets,
// keeping handlers/ free of raw markup.
package webassets

import _ "embed"

//go:embed dashboard.html
var dashboard []byte

//go:embed openapi.yaml
var openapi []byte

// Dashboard returns the embedded dashboard HTML bytes.
func Dashboard() []byte { return dashboard }

// OpenAPI returns the embedded OpenAPI 3.1 spec YAML bytes.
func OpenAPI() []byte { return openapi }
