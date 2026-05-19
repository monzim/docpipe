package handlers

import (
	"strings"
	"testing"

	"github.com/monzim/docpipe/internal/httpx"
)

func TestResolveHTML(t *testing.T) {
	cases := []struct {
		name     string
		req      convertRequest
		wantHTML string
		wantCode string
	}{
		{"inline", convertRequest{HTML: "<p>hi</p>"}, "<p>hi</p>", ""},
		{"base64", convertRequest{HTMLBase64: "PHA+aGk8L3A+"}, "<p>hi</p>", ""},
		{"neither", convertRequest{}, "", httpx.CodeInvalidRequest},
		{"both", convertRequest{HTML: "<p/>", HTMLBase64: "PHA+aGk8L3A+"}, "", httpx.CodeInvalidRequest},
		{"bad base64", convertRequest{HTMLBase64: "!!!"}, "", httpx.CodeInvalidBase64},
		{"whitespace only is treated as missing", convertRequest{HTML: "   "}, "", httpx.CodeInvalidRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, code, err := resolveHTML(&tc.req)
			if tc.wantCode == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if got != tc.wantHTML {
					t.Errorf("html: got %q want %q", got, tc.wantHTML)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error code %s, got nil", tc.wantCode)
			}
			if code != tc.wantCode {
				t.Errorf("code: got %s want %s", code, tc.wantCode)
			}
		})
	}
}

func TestBuildOptions_NilRequest(t *testing.T) {
	o, code, err := buildOptions(nil, nil)
	if err != nil || code != "" {
		t.Fatalf("buildOptions(nil): err=%v code=%s", err, code)
	}
	if !o.PrintBackground {
		t.Error("print_background should default to true")
	}
}

func TestBuildOptions_Margins(t *testing.T) {
	mr := &requestOptions{
		Margin: &marginRequest{Top: "10mm", Right: "0.5in", Bottom: "1cm", Left: "10mm"},
	}
	o, _, err := buildOptions(mr, nil)
	if err != nil {
		t.Fatal(err)
	}
	if o.Margin.Right != 0.5 {
		t.Errorf("right: got %v want 0.5", o.Margin.Right)
	}
}

func TestBuildOptions_BadMargin(t *testing.T) {
	mr := &requestOptions{Margin: &marginRequest{Top: "ten miles"}}
	_, code, err := buildOptions(mr, nil)
	if err == nil {
		t.Fatal("expected error on bad margin")
	}
	if code != httpx.CodeInvalidRequest {
		t.Errorf("code: got %s want %s", code, httpx.CodeInvalidRequest)
	}
}

func TestWantsJSON(t *testing.T) {
	cases := map[string]bool{
		"":                                false,
		"*/*":                             false,
		"application/pdf":                 false,
		"application/json":                true,
		"application/json; charset=utf-8": true,
		"text/html, application/json,*/*": true,
	}
	for accept, want := range cases {
		if got := wantsJSON(accept); got != want {
			t.Errorf("wantsJSON(%q) = %v, want %v", accept, got, want)
		}
	}
}

func TestSanitizeFilename(t *testing.T) {
	cases := map[string]string{
		"":                   "document.pdf",
		"   ":                "document.pdf",
		"normal.pdf":         "normal.pdf",
		`bad/../path.pdf`:    "bad..path.pdf",
		"weird\"name\nx.pdf": "weirdnamex.pdf",
		`C:\Users\evil.pdf`:  "C:Usersevil.pdf",
	}
	for in, want := range cases {
		if got := sanitizeFilename(in); got != want {
			t.Errorf("sanitizeFilename(%q) = %q want %q", in, got, want)
		}
	}
	// Sanity: never returns anything containing /, \, ", \r, \n
	for in := range cases {
		got := sanitizeFilename(in)
		for _, bad := range []string{"/", "\\", "\"", "\r", "\n"} {
			if strings.Contains(got, bad) {
				t.Errorf("sanitizeFilename(%q) leaked %q", in, bad)
			}
		}
	}
}
