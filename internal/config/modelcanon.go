package config

import (
	"fmt"
	"regexp"
	"strings"
)

// Model-name canonicalization: clients and providers spell the SAME model
// differently (glm-5.3 vs glm-5-3, moonshotai/kimi-k3:nube vs kimi-k3), so
// every surface that GROUPS models would show one model several times. The
// mechanism is a fully data-driven ORDERED rule list (`model_rules`, editable
// in Settings ▸ Models - add/edit/remove/reorder with a live preview);
// NOTHING about the rules is hardcoded: the shipped pipeline in
// DefaultModelRules is Default() data the operator can change or delete
// wholesale (an empty list groups by the exact stored spelling).
// ApplyModelRules is the single semantic owner of rule application; it runs
// at the lowest grouping choke point - the dashboard's contrib mapping
// (internal/web fromRecord/scanContrib) - so the explorer model dimension,
// scope filters, rail counts, and the debug/checklist menus all group
// canonically - and the request-log leaf DISPLAYS the canonical name too
// (log.js modelCell), with the original spelling kept one hover away and
// in the drawer's Parameters. The STORED spelling stays raw everywhere;
// debug session matching and purge/export filters match it exactly
// (destructive surfaces never lie).
//
// JS MIRROR: explorer.js `canonicalModel` applies the identical rule
// semantics (the client's log-scope mirror recordMatchesDim needs it);
// pinned by matching case tables (TestCanonicalModel + the ui_check mirror
// test) - keep them in lockstep.

// Rule modes - the complete vocabulary. `exact` compares the whole string
// (a plain merge/rewrite); `pattern` is a regex rewrite (all occurrences,
// `$1`-style capture refs, RE2-compatible - compiled/validated at load so
// the JS mirror can never see a pattern Go rejects); `lower` folds case
// (regex cannot rewrite case in a portable replacement). Rules apply ONCE
// in list order - a single pipeline pass, never iterated to fixpoint.
const (
	ModelRuleExact   = "exact"
	ModelRulePattern = "pattern"
	ModelRuleLower   = "lower"
)

// ModelRule is one rewrite step in the canonicalization pipeline. A
// Disabled rule is kept but never executes - the operator can park a rule
// (or an unfinished draft) and A/B the pipeline without deleting it, the
// same affordance Grafana transformations and Stripe rules give.
type ModelRule struct {
	Mode     string `yaml:"mode" json:"mode"`
	From     string `yaml:"from" json:"from"`
	To       string `yaml:"to" json:"to"`
	Disabled bool   `yaml:"disabled,omitempty" json:"disabled,omitempty"`
}

// ModelCanon is the effective rule set, built from the live Config by
// Config.ModelCanon(). It rides the /metrics/bootstrap payload
// (`model_canon.rules`) so the client mirror and the server fold always
// agree; each side compiles the patterns once (CompileModelRules in Go,
// applyModelCanon in JS) and applies them per folded record.
type ModelCanon struct {
	Rules []ModelRule `json:"rules"`
}

// ModelRulesMax bounds the pipeline length (internal guardrail, same shape
// as the explorer's 64-filter cap): the list executes per folded record
// spelling, and an unbounded chain cannot be memoized meaningfully.
const ModelRulesMax = 64

// DefaultModelRules is the SHIPPED pipeline - pure data, no code semantics:
// fold case, then strip an OpenRouter-style `vendor/` namespace, strip a
// trailing `:tag` deployment suffix, strip a trailing architecture/quant
// suffix (`-fp4`, `-nvfp4`, `-bf16`, `-int8`, GGUF quants like `-q4_k_m` -
// the same model served at a different precision groups with its base),
// and unify `.` with `-` between digits (a dated snapshot suffix like
// `-0813` stays distinct). Put an `exact` rule ABOVE the list to merge
// before normalization, BELOW to merge after.
func DefaultModelRules() []ModelRule {
	return []ModelRule{
		{Mode: ModelRuleLower},
		{Mode: ModelRulePattern, From: `^[a-z0-9][a-z0-9._-]*/`, To: ""},
		{Mode: ModelRulePattern, From: `:[a-z0-9._-]+$`, To: ""},
		{Mode: ModelRulePattern, From: `-(?:[a-z]{0,2}fp\d+|bf\d+|int\d+|nf\d+|[a-z]?q\d+(?:_[0-9a-z]+)*)$`, To: ""},
		{Mode: ModelRulePattern, From: `(\d)\.(\d)`, To: "$1-$2"},
	}
}

// ValidateModelRules is the load-boundary gate for the whole list (called by
// Validate, i.e. by YAML load AND the Settings POST): mode shape per rule,
// pattern compilability (a bad regex fails with the pattern named, never
// silently mis-fires at fold time), and the length cap. A DISABLED rule
// still requires a valid mode (structural) but skips its body checks -
// parking a broken/unfinished rule is exactly what the flag is for.
func ValidateModelRules(rules []ModelRule) error {
	if len(rules) > ModelRulesMax {
		return fmt.Errorf("model_rules: %d rules exceeds the cap of %d", len(rules), ModelRulesMax)
	}
	for i, r := range rules {
		switch r.Mode {
		case ModelRuleLower:
			if !r.Disabled && (r.From != "" || r.To != "") {
				return fmt.Errorf("model_rules[%d]: a lower rule takes no from/to", i)
			}
		case ModelRuleExact:
			if !r.Disabled && strings.TrimSpace(r.From) == "" {
				return fmt.Errorf("model_rules[%d]: exact rule needs a from spelling", i)
			}
			// The comparison is against the TRIMMED stored spelling, so a
			// padded from is a rule that can never fire - reject it at the
			// boundary instead of letting it die silently at fold time.
			if !r.Disabled && r.From != strings.TrimSpace(r.From) {
				return fmt.Errorf("model_rules[%d]: exact rule from must not have leading/trailing spaces (it is compared whole)", i)
			}
		case ModelRulePattern:
			if !r.Disabled && strings.TrimSpace(r.From) == "" {
				return fmt.Errorf("model_rules[%d]: pattern rule needs a from pattern", i)
			}
			if !r.Disabled {
				if _, err := regexp.Compile(r.From); err != nil {
					return fmt.Errorf("model_rules[%d]: invalid pattern %q: %v", i, r.From, err)
				}
			}
		default:
			return fmt.Errorf("model_rules[%d]: mode must be one of exact, pattern, lower (got %q)", i, r.Mode)
		}
	}
	return nil
}

// ModelRuleExec is one compiled pipeline step. Patterns are compiled ONCE
// per request (CompileModelRules) - never per folded record.
type ModelRuleExec struct {
	lower     bool
	exact     bool
	exactFrom string
	exactTo   string
	re        *regexp.Regexp
	reTo      string
}

// CompileModelRules prepares a validated rule list for application.
// Disabled rules are SKIPPED here (the single execution choke point), so a
// parked rule costs nothing at fold time. The remaining rules must already
// have passed ValidateModelRules (the only caller path: config load gates
// validation before the web fold ever sees the list); a compile failure
// here is unreachable and panics loudly rather than silently mis-grouping.
func CompileModelRules(rules []ModelRule) []ModelRuleExec {
	exec := make([]ModelRuleExec, 0, len(rules))
	for _, r := range rules {
		if r.Disabled {
			continue
		}
		switch r.Mode {
		case ModelRuleLower:
			exec = append(exec, ModelRuleExec{lower: true})
		case ModelRuleExact:
			exec = append(exec, ModelRuleExec{exact: true, exactFrom: r.From, exactTo: r.To})
		case ModelRulePattern:
			exec = append(exec, ModelRuleExec{re: regexp.MustCompile(r.From), reTo: r.To})
		}
	}
	return exec
}

// ApplyModelRules maps one raw spelling through the compiled pipeline.
// Each rule applies once, in list order; an all-stripped result falls back
// to the trimmed original (a model name never canonicalizes to empty), and
// an empty pipeline is the identity.
func ApplyModelRules(exec []ModelRuleExec, name string) string {
	s := strings.TrimSpace(name)
	for _, r := range exec {
		switch {
		case r.lower:
			s = strings.ToLower(s)
		case r.exact:
			if s == r.exactFrom {
				s = r.exactTo
			}
		default:
			s = r.re.ReplaceAllString(s, r.reTo)
		}
		if s == "" {
			return strings.TrimSpace(name)
		}
	}
	return s
}

// ModelCanon derives the effective rule set from the config (a copy - the
// payload may outlive a config swap). Read per request by the dashboard
// aggregates and per bootstrap payload by the client mirror.
func (c *Config) ModelCanon() ModelCanon {
	rules := make([]ModelRule, len(c.ModelRules))
	copy(rules, c.ModelRules)
	return ModelCanon{Rules: rules}
}
