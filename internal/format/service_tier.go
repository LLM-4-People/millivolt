package format

import "github.com/LLM-4-People/millivolt/internal/format/ir"

// Service-tier vocabularies per target wire. One owner keeps the three
// encoders from copying a value the target does not accept: an unrepresentable
// tier is omitted with a warning rather than sent and rejected upstream.
var (
	chatServiceTiers = map[string]bool{
		"auto": true, "default": true, "flex": true, "scale": true, "priority": true, "fast": true,
	}
	responsesServiceTiers = map[string]bool{
		"auto": true, "default": true, "flex": true, "scale": true, "fast": true, "priority": true, "ultrafast": true,
	}
	anthropicServiceTiers = map[string]bool{
		"auto": true, "standard_only": true,
	}
)

// mapServiceTier maps one source service tier onto the target vocabulary.
// Auto passes everywhere; Chat/Responses "default" and Anthropic
// "standard_only" are the same documented behavior and map to each other with
// an approximation warning. A value with no target spelling is omitted with a
// dropped warning, never forwarded blindly.
func mapServiceTier(target ir.Protocol, value string) (string, ir.Warnings) {
	if value == "" {
		return "", nil
	}
	var vocabulary map[string]bool
	switch target {
	case ir.Chat:
		vocabulary = chatServiceTiers
	case ir.Responses:
		vocabulary = responsesServiceTiers
	case ir.Anthropic:
		vocabulary = anthropicServiceTiers
	default:
		return "", ir.Warnings{{Kind: ir.WarningDropped, Field: "service_tier",
			Detail: "the target has no service-tier vocabulary"}}
	}
	if vocabulary[value] {
		return value, nil
	}
	switch {
	case value == "default" && target == ir.Anthropic:
		return "standard_only", ir.Warnings{{Kind: ir.WarningApproximated, Field: "service_tier",
			Detail: "mapped default to standard_only"}}
	case value == "standard_only" && (target == ir.Chat || target == ir.Responses):
		return "default", ir.Warnings{{Kind: ir.WarningApproximated, Field: "service_tier",
			Detail: "mapped standard_only to default"}}
	}
	return "", ir.Warnings{{Kind: ir.WarningDropped, Field: "service_tier",
		Detail: "the target has no equivalent tier"}}
}
