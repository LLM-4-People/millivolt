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

// anthropicUsageServiceTier folds Anthropic's Usage.service_tier and
// Usage.speed onto one canonical IR tier. priority wins over fast, which wins
// over an explicit standard; standard has no Chat/Responses spelling and is
// approximated to default. batch describes the Batch API, which has no
// cross-protocol tier. Both fields describe one serving decision, so an
// explicit standard pair produces at most one approximation warning.
func anthropicUsageServiceTier(serviceTier, speed string) (string, ir.Warnings) {
	if serviceTier == "batch" {
		return "", ir.Warnings{{Kind: ir.WarningDropped, Field: "service_tier",
			Detail: "Anthropic batch usage has no cross-protocol tier"}}
	}
	if serviceTier == "priority" {
		return "priority", nil
	}
	if speed == "fast" {
		return "fast", nil
	}
	if serviceTier == "standard" || speed == "standard" {
		return "default", ir.Warnings{{Kind: ir.WarningApproximated, Field: "service_tier",
			Detail: "mapped Anthropic standard to default"}}
	}
	if serviceTier != "" {
		return "", ir.Warnings{{Kind: ir.WarningDropped, Field: "service_tier",
			Detail: "the usage tier has no cross-protocol equivalent"}}
	}
	if speed != "" {
		return "", ir.Warnings{{Kind: ir.WarningDropped, Field: "service_tier",
			Detail: "the usage speed has no cross-protocol equivalent"}}
	}
	return "", nil
}

// anthropicResponseUsageTier maps one canonical IR tier back onto the
// Anthropic response usage vocabulary (service_tier standard/priority/batch,
// speed standard/fast). The request vocabulary (auto/standard_only) is not a
// response usage value, so a native round trip renders it as standard.
func anthropicResponseUsageTier(tier string) (serviceTier, speed string, warns ir.Warnings) {
	switch tier {
	case "":
		return "", "", nil
	case "default", "standard", "standard_only":
		return "standard", "", nil
	case "priority":
		return "priority", "", nil
	case "batch":
		return "batch", "", nil
	case "fast":
		return "", "fast", nil
	default:
		return "", "", ir.Warnings{{Kind: ir.WarningDropped, Field: "service_tier",
			Detail: "Anthropic response usage has no equivalent tier"}}
	}
}
