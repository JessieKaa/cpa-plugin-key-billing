package plugin

import (
	"encoding/json"
	"net/http"
	"strings"

	"cpa-key-billing/internal/messages"
)

func OKEnvelope(v any) ([]byte, error) {
	raw, errMarshal := json.Marshal(v)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return json.Marshal(Envelope{OK: true, Result: raw})
}

func ErrorEnvelope(code, message string, httpStatus int) []byte {
	raw, _ := json.Marshal(Envelope{
		OK: false,
		Error: &EnvelopeError{
			Code:       strings.TrimSpace(code),
			Message:    strings.TrimSpace(message),
			HTTPStatus: httpStatus,
		},
	})
	return raw
}

// Management responses carry key previews, plans, prices, events, and quota
// data for an administrator's authenticated session. They must never be
// cached by intermediaries or the browser back/forward cache, and never
// indexed as generic content.
func secureManagementHeaders(headers http.Header) http.Header {
	headers.Set("Cache-Control", "private, no-store")
	headers.Set("Pragma", "no-cache")
	headers.Set("Referrer-Policy", "no-referrer")
	headers.Set("X-Content-Type-Options", "nosniff")
	return headers
}

func JSONResponse(status int, payload any) ManagementResponse {
	body, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		status = http.StatusInternalServerError
		body, _ = json.Marshal(errorBody("json_error", errMarshal.Error()))
	}
	return ManagementResponse{
		StatusCode: status,
		Headers:    secureManagementHeaders(http.Header{"Content-Type": []string{"application/json; charset=utf-8"}}),
		Body:       body,
	}
}

func JSONError(status int, code, message string) ManagementResponse {
	return jsonMessageError(status, code, messages.Literal(message))
}

func jsonMessageError(status int, code string, detail messages.Message) ManagementResponse {
	value := map[string]any{"code": strings.TrimSpace(code), "message": detail.Text}
	if detail.Key != "" {
		value["message_key"] = detail.Key
		if len(detail.Params) > 0 {
			value["message_params"] = detail.Params
		}
	}
	return JSONResponse(status, map[string]any{"error": value})
}

func errorBody(code, message string) map[string]any {
	return map[string]any{
		"error": map[string]string{
			"code":    strings.TrimSpace(code),
			"message": strings.TrimSpace(message),
		},
	}
}
