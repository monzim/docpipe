package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/monzim/docpipe/internal/config"
)

func TestSwaggerHandler_DefaultsOnly(t *testing.T) {
	h, err := NewSwaggerHandler(nil)
	if err != nil {
		t.Fatalf("NewSwaggerHandler: %v", err)
	}
	rec := httptest.NewRecorder()
	h.Spec(rec, httptest.NewRequest(http.MethodGet, "/swagger/openapi.yaml", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d want 200", rec.Code)
	}
	body := rec.Body.String()
	// The YAML defaults must still be in the served spec.
	if !strings.Contains(body, "https://docpipe.services.monzim.com") {
		t.Errorf("default production URL missing from served spec")
	}
	if !strings.Contains(body, "http://localhost:8080") {
		t.Errorf("default localhost URL missing from served spec")
	}
}

func TestSwaggerHandler_EnvServersPrepended(t *testing.T) {
	extras := []config.SwaggerServer{
		{URL: "https://staging.docpipe.example.com", Description: "Staging"},
		{URL: "https://canary.docpipe.example.com", Description: "Canary"},
	}
	h, err := NewSwaggerHandler(extras)
	if err != nil {
		t.Fatalf("NewSwaggerHandler: %v", err)
	}

	// Parse the served bytes back to a structured form and check ordering.
	var doc map[string]any
	if err := yaml.Unmarshal(h.specBytes, &doc); err != nil {
		t.Fatalf("yaml.Unmarshal served spec: %v", err)
	}
	rawServers, ok := doc["servers"].([]any)
	if !ok {
		t.Fatalf("servers key missing or wrong type: %T", doc["servers"])
	}
	if len(rawServers) < 4 {
		t.Fatalf("expected ≥4 servers (2 extras + 2 defaults), got %d", len(rawServers))
	}

	first := rawServers[0].(map[string]any)
	if first["url"] != "https://staging.docpipe.example.com" {
		t.Errorf("first server should be the first env extra; got %v", first["url"])
	}
	second := rawServers[1].(map[string]any)
	if second["url"] != "https://canary.docpipe.example.com" {
		t.Errorf("second server should be the second env extra; got %v", second["url"])
	}
	// Defaults follow.
	defaultsFound := 0
	for _, s := range rawServers[2:] {
		m := s.(map[string]any)
		if m["url"] == "https://docpipe.services.monzim.com" || m["url"] == "http://localhost:8080" {
			defaultsFound++
		}
	}
	if defaultsFound != 2 {
		t.Errorf("expected both YAML defaults preserved after env extras, found %d", defaultsFound)
	}
}

func TestSwaggerHandler_UIServesHTML(t *testing.T) {
	h, err := NewSwaggerHandler(nil)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.UI(rec, httptest.NewRequest(http.MethodGet, "/swagger/", nil))
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Errorf("Content-Type = %q want text/html...", got)
	}
	if !strings.Contains(rec.Body.String(), "/swagger/openapi.yaml") {
		t.Errorf("HTML should reference the spec URL")
	}
}

func TestSwaggerHandler_DescriptionOmittedWhenEmpty(t *testing.T) {
	extras := []config.SwaggerServer{
		{URL: "https://nodescription.example.com"}, // no description
	}
	h, err := NewSwaggerHandler(extras)
	if err != nil {
		t.Fatal(err)
	}
	body := string(h.specBytes)
	if !strings.Contains(body, "https://nodescription.example.com") {
		t.Fatal("URL missing")
	}
	// The description key should NOT appear for the bare-URL entry. We can't
	// grep "description:" because YAML defaults have descriptions — instead,
	// check structurally.
	var doc map[string]any
	if err := yaml.Unmarshal(h.specBytes, &doc); err != nil {
		t.Fatal(err)
	}
	servers := doc["servers"].([]any)
	first := servers[0].(map[string]any)
	if _, has := first["description"]; has {
		t.Errorf("first server (env-supplied, no description) should not have description key; got %v", first)
	}
}
