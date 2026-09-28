package plugin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode"

	"cpa-key-billing/internal/billing"
	"cpa-key-billing/internal/uibuild"
)

func TestUIIncludesBothLanguagesWithoutExternalTranslationResources(t *testing.T) {
	uiHTML, err := uibuild.Build(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`<html lang="en">`, "const BILLING_MESSAGES =", "billing-language-change", `data-page-action`} {
		if !bytes.Contains(uiHTML, []byte(want)) {
			t.Fatalf("missing %q", want)
		}
	}
	if bytes.Contains(uiHTML, []byte("// BILLING_I18N")) {
		t.Fatal("unexpanded translation marker")
	}
}

func TestManagementValidationIncludesTranslationMetadata(t *testing.T) {
	err := (billing.Plan{}).Validate()
	if err == nil {
		t.Fatal("expected invalid plan")
	}
	response := errorResponse(err)
	var body struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Key     string `json:"message_key"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body, &body); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusBadRequest || body.Error.Code != "invalid" || body.Error.Key == "" || body.Error.Message == "" {
		t.Fatalf("response = %s", response.Body)
	}
}

func TestProtocolRejectionsUseEnglishWithoutUITranslationMetadata(t *testing.T) {
	for _, format := range []string{"openai", "claude", "gemini"} {
		responses := []RequestInterceptResponse{
			concurrencyLimitResponse(format, billing.SlotDecision{Active: 2, Limit: 2}),
			quotaExhaustedResponse(format, billing.Decision{PlanName: "Test plan"}, time.Now()),
			modelForbiddenResponse(format, billing.RoutingDecision{Model: "test-model"}),
			routingConfigurationResponse(format, "Routing rule no longer exists"),
			priceRefusal(format, "model_price_error", "Model test-model has no configured price"),
		}
		for _, response := range responses {
			var payload struct {
				Error map[string]any `json:"error"`
			}
			if err := json.Unmarshal(response.ResponseBody, &payload); err != nil {
				t.Fatal(err)
			}
			message, _ := payload.Error["message"].(string)
			if message == "" || strings.IndexFunc(message, func(r rune) bool { return unicode.Is(unicode.Han, r) }) >= 0 {
				t.Fatalf("%s response must use English: %s", format, response.ResponseBody)
			}
			if payload.Error["message_key"] != nil || payload.Error["message_params"] != nil {
				t.Fatalf("UI metadata must not leak into the client protocol: %s", response.ResponseBody)
			}
		}
	}
	if strings.IndexFunc(PluginName+noRoutedCredentialMessage, func(r rune) bool { return unicode.Is(unicode.Han, r) }) >= 0 {
		t.Fatal("registration and scheduler diagnostics must use English")
	}
}
