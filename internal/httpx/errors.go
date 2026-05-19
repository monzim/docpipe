// Error envelope. Spec §11 — every 4xx/5xx returns this JSON shape.
package httpx

import (
	"encoding/json"
	"net/http"
)

// Error codes. Keep in sync with spec §11.
const (
	CodeInvalidRequest   = "invalid_request"
	CodeInvalidBase64    = "invalid_base64"
	CodeUnauthorized     = "unauthorized"
	CodeForbidden        = "forbidden"
	CodePayloadTooLarge  = "payload_too_large"
	CodeRateLimited      = "rate_limited"
	CodeRenderTimeout    = "render_timeout"
	CodeRenderFailed     = "render_failed"
	CodeServiceUnavail   = "service_unavailable"
	CodeInternalError    = "internal_error"
	CodeMethodNotAllowed = "method_not_allowed"
	CodeUnsupportedMedia = "unsupported_media_type"
)

// errorEnvelope matches the JSON shape from spec §11.
type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	RequestID string         `json:"request_id,omitempty"`
	Details   map[string]any `json:"details,omitempty"`
}

// WriteError emits a uniform JSON error response.
//
// The HTTP status is inferred from the code. Details may be nil.
func WriteError(w http.ResponseWriter, r *http.Request, code, msg string, details map[string]any) {
	status := statusForCode(code)
	env := errorEnvelope{
		Error: errorBody{
			Code:      code,
			Message:   msg,
			RequestID: RequestID(r.Context()),
			Details:   details,
		},
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(env)
}

func statusForCode(code string) int {
	switch code {
	case CodeInvalidRequest, CodeInvalidBase64:
		return http.StatusBadRequest
	case CodeUnauthorized:
		return http.StatusUnauthorized
	case CodeForbidden:
		return http.StatusForbidden
	case CodeMethodNotAllowed:
		return http.StatusMethodNotAllowed
	case CodeUnsupportedMedia:
		return http.StatusUnsupportedMediaType
	case CodePayloadTooLarge:
		return http.StatusRequestEntityTooLarge
	case CodeRateLimited:
		return http.StatusTooManyRequests
	case CodeRenderTimeout:
		return http.StatusGatewayTimeout
	case CodeServiceUnavail:
		return http.StatusServiceUnavailable
	case CodeRenderFailed, CodeInternalError:
		return http.StatusInternalServerError
	default:
		return http.StatusInternalServerError
	}
}
