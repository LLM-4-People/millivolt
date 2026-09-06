package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// Map exports every schema field as a JSON-friendly value: durations are Go
// duration strings ("5s", "2m"), never nanoseconds. This is what GET
// /admin/config returns and what the dashboard round-trips through POST.
func (c *Config) Map() map[string]any {
	out := make(map[string]any, len(Schema()))
	for _, f := range Schema() {
		out[f.Key] = exportValue(f, c.fieldValue(f.Key))
	}
	return out
}

// Apply overlays values onto c. Unknown keys are rejected (deny by default).
// Types are coerced from JSON (float64 integers, duration strings). Call
// Validate after Apply; this only does per-field type conversion.
func (c *Config) Apply(values map[string]any) error {
	if values == nil {
		return fmt.Errorf("values is required")
	}
	for k, v := range values {
		f := FieldByKey(k)
		if f == nil {
			return fmt.Errorf("unknown config key %q", k)
		}
		if f.Category == "storm" && v == nil {
			return fmt.Errorf("%s: null is not a valid setting", k)
		}
		if err := c.setField(*f, v); err != nil {
			return fmt.Errorf("%s: %w", k, err)
		}
	}
	return nil
}

// StartupBoundChanges returns yaml keys that differ between cur and next and
// cannot hot-reload. Driven by Schema().HotReload - the same flag the
// Settings UI uses to badge "restart".
func StartupBoundChanges(cur, next *Config) []string {
	if cur == nil || next == nil {
		return nil
	}
	cm, nm := cur.Map(), next.Map()
	var out []string
	for _, f := range Schema() {
		if f.HotReload {
			continue
		}
		if !reflect.DeepEqual(cm[f.Key], nm[f.Key]) {
			out = append(out, f.Key)
		}
	}
	return out
}

// FormatDuration renders a duration the way proxy.yaml writes it: 0s, 500ms,
// 5s, 2m, 3h. Coarsest exact unit wins so generated YAML stays readable.
func FormatDuration(d time.Duration) string {
	if d == 0 {
		return "0s"
	}
	neg := d < 0
	if neg {
		d = -d
	}
	var s string
	switch {
	case d%time.Hour == 0:
		s = fmt.Sprintf("%dh", d/time.Hour)
	case d%time.Minute == 0:
		s = fmt.Sprintf("%dm", d/time.Minute)
	case d%time.Second == 0:
		s = fmt.Sprintf("%ds", d/time.Second)
	case d%time.Millisecond == 0:
		s = fmt.Sprintf("%dms", d/time.Millisecond)
	case d%time.Microsecond == 0:
		s = fmt.Sprintf("%dus", d/time.Microsecond)
	default:
		s = d.String()
	}
	if neg {
		return "-" + s
	}
	return s
}

func exportValue(f Field, v any) any {
	switch f.Kind {
	case KindDuration:
		if d, ok := v.(time.Duration); ok {
			return FormatDuration(d)
		}
		return FormatDuration(0)
	case KindStrings:
		ss, _ := v.([]string)
		out := make([]string, len(ss))
		copy(out, ss)
		return out
	case KindProviders:
		m, ok := v.(map[string]ProviderOverride)
		if !ok || m == nil {
			return map[string]ProviderOverride{}
		}
		out := make(map[string]ProviderOverride, len(m))
		for k, pv := range m {
			nv := ProviderOverride{
				CostKeys:   append([]string(nil), pv.CostKeys...),
				UsageKeys:  cloneStrMap(pv.UsageKeys),
				ModelsPath: pv.ModelsPath,
				ModelsKeys: cloneStrMap(pv.ModelsKeys),
				Headers:    cloneStrMap(pv.Headers),
			}
			out[k] = nv
		}
		return out
	case KindAliases:
		m, ok := v.(map[string]string)
		if !ok || m == nil {
			return map[string]string{}
		}
		out := make(map[string]string, len(m))
		for k, pv := range m {
			out[k] = pv
		}
		return out
	default:
		return v
	}
}

func (c *Config) fieldValue(key string) any {
	rv := c.fieldRV(key)
	if !rv.IsValid() {
		return nil
	}
	return rv.Interface()
}

func (c *Config) fieldRV(key string) reflect.Value {
	v := reflect.ValueOf(c).Elem()
	if key == "upstream_timeout" {
		return v.FieldByName("UpstreamTimeout")
	}
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		tag := strings.Split(t.Field(i).Tag.Get("yaml"), ",")[0]
		if tag == key {
			return v.Field(i)
		}
	}
	return reflect.Value{}
}

func (c *Config) setField(f Field, v any) error {
	rv := c.fieldRV(f.Key)
	if !rv.IsValid() || !rv.CanSet() {
		return fmt.Errorf("internal: no settable field for %q", f.Key)
	}
	switch f.Kind {
	case KindString:
		s, err := asString(v)
		if err != nil {
			return err
		}
		rv.SetString(s)
	case KindInt:
		n, err := asInt(v)
		if err != nil {
			return err
		}
		rv.SetInt(int64(n))
	case KindBytes:
		n, err := asInt64(v)
		if err != nil {
			return err
		}
		rv.SetInt(n)
	case KindBool:
		b, err := asBool(v)
		if err != nil {
			return err
		}
		rv.SetBool(b)
	case KindDuration:
		d, err := asDuration(v)
		if err != nil {
			return err
		}
		rv.SetInt(int64(d))
	case KindStrings:
		ss, err := asStrings(v)
		if err != nil {
			return err
		}
		rv.Set(reflect.ValueOf(ss))
	case KindProviders:
		p, err := asProviders(v)
		if err != nil {
			return err
		}
		rv.Set(reflect.ValueOf(p))
	case KindAliases:
		a, err := asAliases(v)
		if err != nil {
			return err
		}
		rv.Set(reflect.ValueOf(a))
	case KindModelRules:
		rs, err := asModelRules(v)
		if err != nil {
			return err
		}
		rv.Set(reflect.ValueOf(rs))
	default:
		return fmt.Errorf("unsupported kind %q", f.Kind)
	}
	return nil
}

func asString(v any) (string, error) {
	switch t := v.(type) {
	case string:
		return t, nil
	case nil:
		return "", nil
	default:
		return "", fmt.Errorf("want string, got %T", v)
	}
}

func asBool(v any) (bool, error) {
	switch t := v.(type) {
	case bool:
		return t, nil
	case string:
		b, err := strconv.ParseBool(strings.TrimSpace(t))
		if err != nil {
			return false, fmt.Errorf("want bool")
		}
		return b, nil
	default:
		return false, fmt.Errorf("want bool, got %T", v)
	}
}

func asInt(v any) (int, error) {
	n, err := asInt64(v)
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

func asInt64(v any) (int64, error) {
	switch t := v.(type) {
	case int:
		return int64(t), nil
	case int64:
		return t, nil
	case float64:
		n := int64(t)
		if float64(n) != t {
			return 0, fmt.Errorf("want integer, got %v", t)
		}
		return n, nil
	case json.Number:
		return t.Int64()
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return 0, fmt.Errorf("want integer")
		}
		return strconv.ParseInt(s, 10, 64)
	default:
		return 0, fmt.Errorf("want integer, got %T", v)
	}
}

func asDuration(v any) (time.Duration, error) {
	switch t := v.(type) {
	case time.Duration:
		return t, nil
	case string:
		s := strings.TrimSpace(t)
		if s == "" || s == "0" {
			return 0, nil
		}
		d, err := time.ParseDuration(s)
		if err != nil {
			return 0, fmt.Errorf("want duration (e.g. 500ms, 5s, 2m, 1h)")
		}
		return d, nil
	default:
		return 0, fmt.Errorf("want duration string, got %T", v)
	}
}

func asStrings(v any) ([]string, error) {
	if v == nil {
		return []string{}, nil
	}
	switch t := v.(type) {
	case []string:
		// Explicit list entries must reach Validate unchanged. Dropping an
		// empty allowlist entry can turn malformed input into unrestricted access.
		return append([]string{}, t...), nil
	case []any:
		out := make([]string, 0, len(t))
		for i, item := range t {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("item %d: want string, got %T", i, item)
			}
			out = append(out, s)
		}
		return out, nil
	case string:
		var out []string
		for _, line := range strings.Split(t, "\n") {
			line = strings.TrimSpace(line)
			if line != "" {
				out = append(out, line)
			}
		}
		return out, nil
	default:
		return nil, fmt.Errorf("want list of string, got %T", v)
	}
}

func asProviders(v any) (map[string]ProviderOverride, error) {
	if v == nil {
		return nil, nil
	}
	if m, ok := v.(map[string]ProviderOverride); ok {
		return m, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("want object")
	}
	if string(b) == "null" || string(b) == "{}" {
		return map[string]ProviderOverride{}, nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var m map[string]ProviderOverride
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("want {label: {cost_keys, usage_keys, models_path, models_keys, headers}}: %w", err)
	}
	if m == nil {
		m = map[string]ProviderOverride{}
	}
	for label := range m {
		if strings.TrimSpace(label) == "" {
			return nil, fmt.Errorf("provider label must be non-empty")
		}
	}
	return m, nil
}

// asModelRules coerces the Settings POST shape of model_rules (ordered
// list of {mode, from, to}). Field-level shape is checked here so a
// malformed rule fails the Apply with a clear error; semantic validation
// (mode vocabulary, pattern compilability, cap) runs in Validate - the same
// gate the YAML path passes through.
func asModelRules(v any) ([]ModelRule, error) {
	if v == nil {
		return nil, nil
	}
	if rs, ok := v.([]ModelRule); ok {
		return rs, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("want list")
	}
	if string(b) == "null" || string(b) == "[]" {
		return []ModelRule{}, nil
	}
	var rs []ModelRule
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rs); err != nil {
		return nil, fmt.Errorf("want [{mode, from, to}]: %w", err)
	}
	return rs, nil
}

// asAliases coerces the JSON shape of provider_aliases (map of old label →
// canonical label). Both sides are trimmed; empty strings are rejected here so
// a malformed pair fails the Apply with a clear error instead of silently
// splitting the label again.
func asAliases(v any) (map[string]string, error) {
	if v == nil {
		return nil, nil
	}
	if m, ok := v.(map[string]string); ok {
		return m, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("want object")
	}
	if string(b) == "null" || string(b) == "{}" {
		return map[string]string{}, nil
	}
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("want {old label: canonical label}: %w", err)
	}
	if m == nil {
		m = map[string]string{}
	}
	for from, to := range m {
		if strings.TrimSpace(from) == "" {
			return nil, fmt.Errorf("alias label must be non-empty")
		}
		if strings.TrimSpace(to) == "" {
			return nil, fmt.Errorf("%s: target label must be non-empty", from)
		}
	}
	return m, nil
}
