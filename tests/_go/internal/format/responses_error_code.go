package format

// Round 6 B1 regression: response.failed.response.error.code is a closed
// ResponseErrorCode enum. The envelope copied any internal code verbatim
// (truncated, empty_completion, translation_error, "500"), producing a wire the
// Responses schema rejects. The free-form error event and the record keep the
// specific code.

import (
	"encoding/json"
	"testing"

	"github.com/LLM-4-People/millivolt/internal/format/ir"
)

func TestResponsesFailedErrorCodeUsesEnum(t *testing.T) {
	enum := map[string]bool{
		"server_error": true, "rate_limit_exceeded": true, "invalid_prompt": true,
		"data_residency_mismatch": true, "bio_policy": true, "misalignment_policy_violation": true,
		"vector_store_timeout": true, "invalid_image": true, "invalid_image_format": true,
		"invalid_base64_image": true, "invalid_image_url": true, "image_too_large": true,
		"image_too_small": true, "image_parse_error": true, "image_content_policy_violation": true,
		"invalid_image_mode": true, "image_file_too_large": true, "unsupported_image_media_type": true,
		"empty_image_file": true, "failed_to_download_image": true, "image_file_not_found": true,
	}
	for _, code := range []string{
		"truncated", "empty_completion", "reasoning_only", "translation_error",
		"overloaded", "500", "api_error", "provider_error", "invalid_api_key",
	} {
		t.Run(code, func(t *testing.T) {
			env, err := NewResponsesEnvelope(ResponsesEnvelope{
				ID: "resp_1", Model: "m", Status: "failed",
				Error: &ir.Error{Code: code, Message: "failed"},
			})
			if err != nil {
				t.Fatal(err)
			}
			failure := env["error"].(map[string]any)
			stored := failure["code"].(string)
			if stored != "server_error" {
				t.Fatalf("failed envelope code = %q, want server_error for internal %q", stored, code)
			}

			payload, _, err := ResponsesSyntheticEvent("response.failed", 2, "resp_1", "m", code, "failed")
			if err != nil {
				t.Fatal(err)
			}
			var event struct {
				Response struct {
					Error struct {
						Code string `json:"code"`
					} `json:"error"`
				} `json:"response"`
			}
			if err := json.Unmarshal(payload, &event); err != nil {
				t.Fatal(err)
			}
			if event.Response.Error.Code != "server_error" {
				t.Fatalf("synthetic failed code = %q, want server_error", event.Response.Error.Code)
			}

			errorPayload, _, err := ResponsesSyntheticEvent("error", 1, "resp_1", "m", code, "failed")
			if err != nil {
				t.Fatal(err)
			}
			var errorEvent struct {
				Code string `json:"code"`
			}
			if err := json.Unmarshal(errorPayload, &errorEvent); err != nil {
				t.Fatal(err)
			}
			if errorEvent.Code != code {
				t.Fatalf("error event code = %q, want the specific %q", errorEvent.Code, code)
			}
		})
	}

	t.Run("rate limit synonym", func(t *testing.T) {
		env, err := NewResponsesEnvelope(ResponsesEnvelope{
			ID: "resp_1", Model: "m", Status: "failed",
			Error: &ir.Error{Code: "rate_limit_error", Message: "slow down"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := env["error"].(map[string]any)["code"]; got != "rate_limit_exceeded" {
			t.Fatalf("rate_limit_error mapped to %v, want rate_limit_exceeded", got)
		}
	})

	for code := range enum {
		env, err := NewResponsesEnvelope(ResponsesEnvelope{
			ID: "resp_1", Model: "m", Status: "failed",
			Error: &ir.Error{Code: code, Message: "failed"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := env["error"].(map[string]any)["code"]; got != code {
			t.Fatalf("known enum %q was rewritten to %v", code, got)
		}
	}
}
