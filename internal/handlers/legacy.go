package handlers

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/monzim/docpipe/internal/httpx"
	"github.com/monzim/docpipe/internal/render"
)

// LegacyConvertHandler implements the v1-compatible POST /api/html-to-pdf.
//
// The CASCK job portal posts `{"base64_html": "..."}` and expects a PDF
// named `admit_card.pdf`. We translate to v2 defaults and emit a Sunset/
// Deprecation header pair so consumers know to migrate.
//
// Slated for removal once the portal is migrated to /v1/convert/html-to-pdf.
type LegacyConvertHandler struct {
	b   *render.Browser
	log *slog.Logger

	// sunsetDate is sent in the Sunset header. We pick a date ~90 days from
	// process start; in practice you'd set this from a build flag tied to
	// the portal's migration schedule.
	sunsetDate string
}

// NewLegacyConvertHandler binds the renderer and computes the sunset date.
func NewLegacyConvertHandler(b *render.Browser, log *slog.Logger) *LegacyConvertHandler {
	return &LegacyConvertHandler{
		b:          b,
		log:        log,
		sunsetDate: time.Now().AddDate(0, 0, 90).UTC().Format(http.TimeFormat),
	}
}

type legacyRequest struct {
	Base64HTML string `json:"base64_html"`
}

// Handle implements POST /api/html-to-pdf.
func (h *LegacyConvertHandler) Handle(w http.ResponseWriter, r *http.Request) {
	// Per-call deprecation warning. Includes key name so the team can chase
	// remaining v1 callers by identity rather than guessing.
	h.log.Warn("legacy_endpoint_used",
		"key", httpx.APIKeyName(r.Context()),
		"remote", r.RemoteAddr,
		"path", r.URL.Path,
	)

	if !strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
		httpx.WriteError(w, r, httpx.CodeUnsupportedMedia,
			"Content-Type must be application/json", nil)
		return
	}

	var req legacyRequest
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&req); err != nil {
		if httpx.IsBodyTooLarge(err) {
			httpx.WriteError(w, r, httpx.CodePayloadTooLarge, "request body too large", nil)
			return
		}
		httpx.WriteError(w, r, httpx.CodeInvalidRequest, "invalid JSON: "+err.Error(), nil)
		return
	}
	if strings.TrimSpace(req.Base64HTML) == "" {
		httpx.WriteError(w, r, httpx.CodeInvalidRequest,
			"`base64_html` is required", nil)
		return
	}

	// Forward via the v2 surface, translating the field name.
	// v1 used `base64_html`; v2 uses `html_base64`.
	v2 := convertRequest{
		HTMLBase64: req.Base64HTML,
		Filename:   "admit_card.pdf",
	}
	html, code, err := resolveHTML(&v2)
	if err != nil {
		httpx.WriteError(w, r, code, err.Error(), nil)
		return
	}

	opts, code, err := buildOptions(nil, h.b)
	if err != nil {
		httpx.WriteError(w, r, code, err.Error(), nil)
		return
	}
	// v1 defaults: wait 10s before printing (closest analogue is networkidle
	// with a long ceiling, since v1 used a hard Sleep(10s)).
	opts.WaitStrategy = render.WaitNetworkIdle
	opts.WaitTimeout = 10 * time.Second

	start := time.Now()
	pdf, err := h.b.Render(r.Context(), html, opts)
	dur := time.Since(start)
	if err != nil {
		writeRenderError(w, r, err)
		return
	}

	// Deprecation signal. RFC 8594 (Sunset) + draft-ietf-httpapi-deprecation-header.
	w.Header().Set("Deprecation", "true")
	w.Header().Set("Sunset", h.sunsetDate)
	w.Header().Set("Link", `</v1/convert/html-to-pdf>; rel="successor-version"`)

	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(pdf)))
	w.Header().Set("Content-Disposition", `attachment; filename="admit_card.pdf"`)
	w.Header().Set("X-Render-Duration-Ms", fmt.Sprintf("%d", dur.Milliseconds()))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(pdf)
}
