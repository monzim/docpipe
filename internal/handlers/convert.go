// Package handlers exposes the HTTP handlers for DocPipe.
//
// Each handler is a method on a small struct that holds its dependencies.
// Handlers never reach for globals; their inputs are passed via New*Handler.
package handlers

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/monzim/docpipe/internal/httpx"
	"github.com/monzim/docpipe/internal/render"
)

// convertRequest mirrors spec §6 request body.
//
// HTML must be base64-encoded — raw HTML in JSON is escape-fragile (newlines,
// quotes, backslashes in inline styles all break naive callers). Base64 is one
// well-defined encoding to keep the wire format unambiguous.
type convertRequest struct {
	HTMLBase64 string          `json:"html_base64"`
	Filename   string          `json:"filename,omitempty"`
	Options    *requestOptions `json:"options,omitempty"`
}

type requestOptions struct {
	Format              string         `json:"format,omitempty"`
	Landscape           bool           `json:"landscape,omitempty"`
	Scale               float64        `json:"scale,omitempty"`
	PrintBackground     *bool          `json:"print_background,omitempty"`
	PreferCSSPageSize   bool           `json:"prefer_css_page_size,omitempty"`
	Margin              *marginRequest `json:"margin,omitempty"`
	HeaderTemplate      string         `json:"header_template,omitempty"`
	FooterTemplate      string         `json:"footer_template,omitempty"`
	DisplayHeaderFooter bool           `json:"display_header_footer,omitempty"`
	PageRanges          string         `json:"page_ranges,omitempty"`
	Wait                *waitRequest   `json:"wait,omitempty"`
}

type marginRequest struct {
	Top    string `json:"top,omitempty"`
	Right  string `json:"right,omitempty"`
	Bottom string `json:"bottom,omitempty"`
	Left   string `json:"left,omitempty"`
}

type waitRequest struct {
	Strategy  string  `json:"strategy,omitempty"`
	TimeoutMS int64   `json:"timeout_ms,omitempty"`
	Selector  *string `json:"selector,omitempty"`
}

type convertJSONResponse struct {
	Filename   string `json:"filename"`
	SizeBytes  int    `json:"size_bytes"`
	PDFBase64  string `json:"pdf_base64"`
	DurationMS int64  `json:"duration_ms"`
	RequestID  string `json:"request_id,omitempty"`
}

// ConvertHandler wraps the renderer dependency.
type ConvertHandler struct {
	b *render.Browser
}

// NewConvertHandler returns a handler bound to the given browser.
func NewConvertHandler(b *render.Browser) *ConvertHandler {
	return &ConvertHandler{b: b}
}

// Handle implements POST /v1/convert/html-to-pdf.
//
// Response negotiation:
//   - Accept: application/pdf (default) → raw PDF bytes
//   - Accept: application/json          → base64-wrapped envelope per spec §6
func (h *ConvertHandler) Handle(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
		httpx.WriteError(w, r, httpx.CodeUnsupportedMedia,
			"Content-Type must be application/json", nil)
		return
	}

	var req convertRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		if httpx.IsBodyTooLarge(err) {
			httpx.WriteError(w, r, httpx.CodePayloadTooLarge, "request body too large", nil)
			return
		}
		httpx.WriteError(w, r, httpx.CodeInvalidRequest, "invalid JSON: "+err.Error(), nil)
		return
	}

	html, code, err := resolveHTML(&req)
	if err != nil {
		httpx.WriteError(w, r, code, err.Error(), nil)
		return
	}

	opts, code, err := buildOptions(req.Options, h.b)
	if err != nil {
		httpx.WriteError(w, r, code, err.Error(), nil)
		return
	}

	filename := req.Filename
	if filename == "" {
		filename = "document.pdf"
	}
	filename = sanitizeFilename(filename)

	start := time.Now()
	pdf, err := h.b.Render(r.Context(), html, opts)
	dur := time.Since(start)
	if err != nil {
		writeRenderError(w, r, err)
		return
	}

	if wantsJSON(r.Header.Get("Accept")) {
		httpx.WriteJSON(w, http.StatusOK, convertJSONResponse{
			Filename:   filename,
			SizeBytes:  len(pdf),
			PDFBase64:  base64.StdEncoding.EncodeToString(pdf),
			DurationMS: dur.Milliseconds(),
			RequestID:  httpx.RequestID(r.Context()),
		})
		return
	}

	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(pdf)))
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, filename))
	w.Header().Set("X-Render-Duration-Ms", fmt.Sprintf("%d", dur.Milliseconds()))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(pdf)
}

// resolveHTML decodes the required html_base64 field. Empty input is a
// 400; a base64 decode error is a 400 with a more specific code so callers
// can distinguish "you forgot the field" from "your encoding is wrong".
func resolveHTML(req *convertRequest) (string, string, error) {
	if strings.TrimSpace(req.HTMLBase64) == "" {
		return "", httpx.CodeInvalidRequest,
			errors.New("`html_base64` is required (base64-encoded HTML)")
	}
	decoded, err := base64.StdEncoding.DecodeString(req.HTMLBase64)
	if err != nil {
		return "", httpx.CodeInvalidBase64,
			fmt.Errorf("html_base64: %w", err)
	}
	return string(decoded), "", nil
}

// buildOptions maps the JSON request options onto render.Options and applies
// defaults from the browser's configured timeout.
func buildOptions(in *requestOptions, _ *render.Browser) (render.Options, string, error) {
	o := render.Options{
		PrintBackground: true,
	}
	if in == nil {
		return o, "", nil
	}

	o.Format = in.Format
	o.Landscape = in.Landscape
	o.Scale = in.Scale
	if in.PrintBackground != nil {
		o.PrintBackground = *in.PrintBackground
	}
	o.PreferCSSPageSize = in.PreferCSSPageSize
	o.HeaderTemplate = in.HeaderTemplate
	o.FooterTemplate = in.FooterTemplate
	o.DisplayHeaderFooter = in.DisplayHeaderFooter
	o.PageRanges = in.PageRanges

	if in.Margin != nil {
		for _, p := range []struct {
			val  string
			dest *float64
			name string
		}{
			{in.Margin.Top, &o.Margin.Top, "top"},
			{in.Margin.Right, &o.Margin.Right, "right"},
			{in.Margin.Bottom, &o.Margin.Bottom, "bottom"},
			{in.Margin.Left, &o.Margin.Left, "left"},
		} {
			v, err := render.ParseMargin(p.val)
			if err != nil {
				return o, httpx.CodeInvalidRequest,
					fmt.Errorf("margin.%s: %w", p.name, err)
			}
			*p.dest = v
		}
	}

	if in.Wait != nil {
		o.WaitStrategy = render.WaitStrategy(in.Wait.Strategy)
		if in.Wait.TimeoutMS > 0 {
			o.WaitTimeout = time.Duration(in.Wait.TimeoutMS) * time.Millisecond
		}
		if in.Wait.Selector != nil {
			o.WaitSelector = *in.Wait.Selector
		}
	}
	return o, "", nil
}

// writeRenderError translates a render.Browser error into the right code.
func writeRenderError(w http.ResponseWriter, r *http.Request, err error) {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "timeout"), strings.Contains(msg, "deadline"):
		httpx.WriteError(w, r, httpx.CodeRenderTimeout, "render exceeded timeout", nil)
	case strings.Contains(msg, "unhealthy"), strings.Contains(msg, "closed"):
		httpx.WriteError(w, r, httpx.CodeServiceUnavail, "renderer unavailable", nil)
	default:
		httpx.WriteError(w, r, httpx.CodeRenderFailed, "render failed: "+msg, nil)
	}
}

// wantsJSON returns true if the Accept header asks for JSON specifically.
// We default to PDF when Accept is */* or absent.
func wantsJSON(accept string) bool {
	accept = strings.ToLower(accept)
	if accept == "" {
		return false
	}
	for part := range strings.SplitSeq(accept, ",") {
		mt := strings.TrimSpace(strings.SplitN(part, ";", 2)[0])
		if mt == "application/json" {
			return true
		}
	}
	return false
}

// sanitizeFilename strips path traversal and quoting that would break the
// Content-Disposition header.
func sanitizeFilename(name string) string {
	name = strings.ReplaceAll(name, "\\", "")
	name = strings.ReplaceAll(name, "/", "")
	name = strings.ReplaceAll(name, "\"", "")
	name = strings.ReplaceAll(name, "\r", "")
	name = strings.ReplaceAll(name, "\n", "")
	name = strings.TrimSpace(name)
	if name == "" {
		return "document.pdf"
	}
	return name
}
