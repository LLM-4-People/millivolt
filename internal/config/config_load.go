package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"reflect"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

func yamlKeySet(present map[string]any, key string) bool {
	_, ok := present[key]
	return ok
}

// mergeOverlay copies Schema keys that appear in present from src onto c.
// Omitted keys keep Default(). Maps merge by key; slices and scalars replace.
func (c *Config) mergeOverlay(src *Config, present map[string]any) {
	for _, f := range Schema() {
		if !yamlKeySet(present, f.Key) {
			continue
		}
		dst := c.fieldRV(f.Key)
		from := src.fieldRV(f.Key)
		if !dst.IsValid() || !from.IsValid() || !dst.CanSet() {
			continue
		}
		if dst.Kind() == reflect.Map {
			if dst.IsNil() {
				dst.Set(reflect.MakeMap(dst.Type()))
			}
			iter := from.MapRange()
			for iter.Next() {
				dst.SetMapIndex(iter.Key(), iter.Value())
			}
			continue
		}
		dst.Set(from)
	}
}

// userFile is LoadFile's strict decode target: every Config key plus the
// upstream_timeout text probe. Config declares UpstreamTimeout yaml:"-"
// (a YAML integer would decode as nanoseconds on time.Duration), so the
// probe field makes the key known to the strict decoder while LoadFile still
// parses the scalar as a duration string. Overlay rejects a bare integer.
type userFile struct {
	Config          `yaml:",inline"`
	UpstreamTimeout *string `yaml:"upstream_timeout"`
}

const (
	skippedExtraDocument = "(extra yaml document)"
	skippedUnparseable   = "(unparseable yaml)"
	invalidConfigSuffix  = ".invalid"
)

// configParseError is a YAML syntax/shape failure. LoadFileRepair moves the
// original aside and writes Default(), except for an ambiguous duplicate key,
// which fails closed without rewriting the source. Other load errors fail
// closed too.
type configParseError struct {
	Path string
	Err  error
}

func (e *configParseError) Error() string {
	return fmt.Sprintf("parse config %s: %s", e.Path, e.Err)
}

func (e *configParseError) Unwrap() error { return e.Err }

// duplicateYAMLKeyError marks an ambiguous mapping: the same top-level key is
// defined twice. It is deliberately distinguishable from generic YAML syntax
// errors so LoadFileRepair refuses to rewrite the document instead of replacing
// it with defaults.
type duplicateYAMLKeyError struct {
	Key       string
	Line      int
	FirstLine int
}

func (e *duplicateYAMLKeyError) Error() string {
	return fmt.Sprintf("duplicate key %q at line %d (first defined at line %d)", e.Key, e.Line, e.FirstLine)
}

func parseConfig(path string, err error) error {
	return &configParseError{Path: path, Err: err}
}

// configValueError is a recognized key whose value is invalid. It is not
// repairable debris: dropping it would silently change policy, so boot and
// reload must fail closed instead of replacing it with a default.
type configValueError struct {
	key string
	err error
}

func (e *configValueError) Error() string {
	if e.key == "" {
		return fmt.Sprintf("invalid config value: %v", e.err)
	}
	return fmt.Sprintf("config key %q: %v", e.key, e.err)
}
func (e *configValueError) Unwrap() error { return e.err }

// failClosedYAMLKey lists keys whose absence changes a security or bind
// boundary. An invalid value must never be silently dropped.
func failClosedYAMLKey(key string) bool {
	switch key {
	case "allowed_base_urls", "listen":
		return true
	default:
		return false
	}
}

// LoadFile is the lenient library/test read. It reads the YAML config at path
// (may be empty) and merges recognized, valid keys over the built-in defaults;
// a missing file yields the defaults. Unknown keys, extra YAML documents and an
// invalid type or range for an ordinary key are omitted. `allowed_base_urls`
// and `listen` are the exceptions: their invalid values describe a security or
// bind boundary, so they fail the read instead of being dropped. Production
// does not call it: boot uses LoadFileRepair, reload uses LoadFileStrict, and
// Settings, backup and the healthcheck use their strict reads. A duplicate
// top-level key is ambiguous and returns a parse error naming the key and line.
// This path does not rewrite the file.
func LoadFile(path string) (*Config, error) {
	cfg, _, err := loadYAMLFile(path)
	return cfg, err
}

// LoadFileStrict is the reload admission path. Unlike LoadFile it does not
// salvage a recognized key's invalid value: any unparseable document, unknown
// nested field, extra document, invalid value or cross-field validation
// failure fails the reload so the running policy is preserved. Unknown
// top-level keys remain debris and are reported, never applied. Boot repair
// remains LoadFileRepair's job.
func LoadFileStrict(path string) (*Config, []string, error) {
	if path == "" {
		return Default(), nil, nil
	}
	b, err := ReadDocument(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read config: %w", err)
	}
	return loadYAMLBytesFailClosed(path, b, false)
}

// LoadBytes is the backup-restore admission path for an in-memory YAML
// document. It fails closed on invalid recognized values like boot, while
// unknown debris is reported and omitted.
func LoadBytes(b []byte) (*Config, []string, error) {
	return loadYAMLBytesFailClosed("backup", b, true)
}

// LoadFileRepair is the boot-only repair path: it loads under the fail-closed
// policy, then persists a cleaned document when unknown keys, unknown nested
// fields, or extra YAML documents were dropped. A recognized key with an
// invalid value is not debris and returns an error without rewriting the
// source. Unparseable files are moved aside to path+invalidConfigSuffix and
// replaced with Default(). A duplicate top-level key is ambiguous instead of
// debris: it is returned as a parse error and the source is left untouched.
// Healthcheck, print-only and reload paths must not call this.
func LoadFileRepair(path string) (*Config, []string, error) {
	if path == "" {
		cfg := Default()
		return cfg, nil, cfg.Validate()
	}
	b, err := ReadDocument(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Default(), nil, nil
		}
		return nil, nil, fmt.Errorf("read config: %w", err)
	}
	cfg, skipped, err := loadYAMLBytesFailClosed(path, b, true)
	if err != nil {
		var valueErr *configValueError
		if errors.As(err, &valueErr) {
			return nil, nil, err
		}
		var parseErr *configParseError
		if !errors.As(err, &parseErr) {
			return nil, nil, err
		}
		// A duplicate key is ambiguous, not debris: leave the document
		// untouched so the first value is never replaced by a default.
		var duplicate *duplicateYAMLKeyError
		if errors.As(err, &duplicate) {
			return nil, nil, err
		}
		invalidPath := path + invalidConfigSuffix
		_ = os.Remove(invalidPath)
		if rerr := os.Rename(path, invalidPath); rerr != nil {
			return nil, nil, fmt.Errorf("repair config %s: %w", path, rerr)
		}
		cfg = Default()
		if werr := WriteFile(path, cfg); werr != nil {
			return nil, nil, fmt.Errorf("repair config %s: %w", path, werr)
		}
		return cfg, []string{skippedUnparseable}, nil
	}
	if len(skipped) > 0 {
		if err := WriteFile(path, cfg); err != nil {
			return cfg, skipped, fmt.Errorf("repair config %s: %w", path, err)
		}
	}
	return cfg, skipped, nil
}

// ReadDocument is the one bounded read owner for a configuration document.
// Boot, reload, inspect and the restore rollback capture all use it. It
// enforces the ConfigDocumentMaxBytes bound on the input bytes before parsing,
// matching the rendered-document bound that Config.validateDocumentSize
// enforces through Validate after parsing.
//
// The pre-open stat rejects a FIFO, device or directory before os.Open, which
// would block indefinitely on a FIFO with no writer. The post-open stat repeats
// the regular-file and size checks for the stat-to-open race, and the bounded
// LimitReader keeps the size guarantee even if the file grows after the check.
func ReadDocument(path string) ([]byte, error) {
	pre, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !pre.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: not a regular file", path)
	}
	if pre.Size() > int64(ConfigDocumentMaxBytes) {
		return nil, configFileTooLarge(path, pre.Size())
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: not a regular file", path)
	}
	if info.Size() > int64(ConfigDocumentMaxBytes) {
		return nil, configFileTooLarge(path, info.Size())
	}
	b, err := io.ReadAll(io.LimitReader(f, int64(ConfigDocumentMaxBytes)+1))
	if err != nil {
		return nil, err
	}
	if len(b) > ConfigDocumentMaxBytes {
		return nil, configFileTooLarge(path, int64(len(b)))
	}
	return b, nil
}

func configFileTooLarge(path string, size int64) error {
	return fmt.Errorf("%s: file is %d bytes, above the %s (%d byte) config document limit",
		path, size, FormatByteSize(ConfigDocumentMaxBytes), ConfigDocumentMaxBytes)
}

func loadYAMLFile(path string) (*Config, []string, error) {
	def := Default()
	if path == "" {
		return def, nil, def.Validate()
	}
	b, err := ReadDocument(path)
	if err != nil {
		if os.IsNotExist(err) {
			return def, nil, nil
		}
		return nil, nil, fmt.Errorf("read config: %w", err)
	}
	return loadYAMLBytes(path, b)
}

func loadYAMLBytes(path string, b []byte) (*Config, []string, error) {
	def := Default()
	dec := yaml.NewDecoder(bytes.NewReader(b))
	var first yaml.Node
	if err := dec.Decode(&first); err != nil && !errors.Is(err, io.EOF) {
		return nil, nil, parseConfig(path, err)
	}
	var skipped []string
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		skipped = append(skipped, skippedExtraDocument)
	}
	mapping, err := yamlDocumentMapping(&first)
	if err != nil {
		return nil, nil, parseConfig(path, err)
	}
	present, nodes, err := yamlMappingValues(mapping)
	if err != nil {
		return nil, nil, parseConfig(path, err)
	}
	if len(present) == 0 {
		return def, skipped, nil
	}
	keys := slices.Sorted(maps.Keys(present))
	var accepted []string
	for _, key := range keys {
		if FieldByKey(key) == nil {
			skipped = append(skipped, key)
			continue
		}
		loose, err := overlayYAMLKey(def.Clone(), key, nodes[key], present[key])
		if err != nil {
			if failClosedYAMLKey(key) {
				return nil, nil, &configValueError{key: key, err: err}
			}
			skipped = append(skipped, key)
			continue
		}
		if loose {
			skipped = append(skipped, key+" (unknown fields)")
		}
		accepted = append(accepted, key)
	}
	cfg, dropped, err := keepYAMLKeys(def, present, nodes, accepted)
	if err != nil {
		return nil, nil, err
	}
	skipped = append(skipped, dropped...)
	return cfg, skipped, nil
}

// loadYAMLBytesFailClosed parses one document for the applying paths (boot
// repair, reload, backup restore). A recognized key's invalid value is always
// an error. repairDebris additionally allows boot to drop and rewrite unknown
// top-level keys, unknown nested fields and extra documents; reload refuses
// the latter two so only an explicit boot repair can clean a document.
func loadYAMLBytesFailClosed(path string, b []byte, repairDebris bool) (*Config, []string, error) {
	def := Default()
	dec := yaml.NewDecoder(bytes.NewReader(b))
	var first yaml.Node
	if err := dec.Decode(&first); err != nil && !errors.Is(err, io.EOF) {
		return nil, nil, parseConfig(path, err)
	}
	var skipped []string
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if !repairDebris {
			return nil, nil, parseConfig(path, errors.New("multiple YAML documents"))
		}
		skipped = append(skipped, skippedExtraDocument)
	}
	mapping, err := yamlDocumentMapping(&first)
	if err != nil {
		return nil, nil, parseConfig(path, err)
	}
	present, nodes, err := yamlMappingValues(mapping)
	if err != nil {
		return nil, nil, parseConfig(path, err)
	}
	if len(present) == 0 {
		return def, skipped, nil
	}
	keys := slices.Sorted(maps.Keys(present))
	accepted := make([]string, 0, len(keys))
	for _, key := range keys {
		if FieldByKey(key) == nil {
			// Unknown top-level keys are obsolete debris, never policy: both
			// paths report them without applying them.
			skipped = append(skipped, key)
			continue
		}
		loose, err := overlayYAMLKey(def.Clone(), key, nodes[key], present[key])
		if err != nil {
			return nil, nil, &configValueError{key: key, err: err}
		}
		if loose {
			if !repairDebris {
				return nil, nil, parseConfig(path, fmt.Errorf("config key %q has unknown fields", key))
			}
			skipped = append(skipped, key+" (unknown fields)")
		}
		accepted = append(accepted, key)
	}
	cfg, err := mergeYAMLKeys(def, present, nodes, accepted)
	if err != nil {
		return nil, nil, &configValueError{err: err}
	}
	return cfg, skipped, nil
}

func yamlResolved(n *yaml.Node) *yaml.Node {
	for n != nil && n.Kind == yaml.AliasNode && n.Alias != nil {
		n = n.Alias
	}
	return n
}

func yamlDocumentMapping(n *yaml.Node) (*yaml.Node, error) {
	n = yamlResolved(n)
	if n == nil || n.Kind == 0 {
		return nil, nil
	}
	if n.Kind == yaml.DocumentNode {
		if len(n.Content) == 0 {
			return nil, nil
		}
		n = yamlResolved(n.Content[0])
	}
	if n == nil || n.Kind == 0 || (n.Kind == yaml.ScalarNode && n.ShortTag() == "!!null") {
		return nil, nil
	}
	if n.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("document must be a YAML mapping")
	}
	return n, nil
}

func yamlMappingValues(n *yaml.Node) (map[string]any, map[string]*yaml.Node, error) {
	present := map[string]any{}
	nodes := map[string]*yaml.Node{}
	if n == nil {
		return present, nodes, nil
	}
	seen := make(map[string]int, len(n.Content)/2)
	for i := 0; i+1 < len(n.Content); i += 2 {
		keyNode := yamlResolved(n.Content[i])
		valNode := yamlResolved(n.Content[i+1])
		if keyNode == nil || keyNode.Kind != yaml.ScalarNode {
			return nil, nil, fmt.Errorf("document must be a YAML mapping")
		}
		key := keyNode.Value
		if firstLine, duplicate := seen[key]; duplicate {
			return nil, nil, &duplicateYAMLKeyError{Key: key, Line: keyNode.Line, FirstLine: firstLine}
		}
		seen[key] = keyNode.Line
		var v any
		if valNode == nil {
			present[key] = nil
			nodes[key] = n.Content[i+1]
			continue
		}
		if err := valNode.Decode(&v); err != nil {
			return nil, nil, err
		}
		present[key] = v
		nodes[key] = valNode
	}
	return present, nodes, nil
}

// keepYAMLKeys merges the accepted yaml keys onto def and returns the kept
// config plus the keys the loader must drop. Two-phase contract: a file whose
// whole accepted key set validates loads in a single merge, the common case
// for every written file; the greedy per-key salvage loop only runs when
// whole-set validation fails, so conflicting files keep the existing repair
// semantics unchanged.
func keepYAMLKeys(def *Config, present map[string]any, nodes map[string]*yaml.Node, accepted []string) (*Config, []string, error) {
	if merged, err := mergeYAMLKeys(def, present, nodes, accepted); err == nil {
		return merged, nil, nil
	} else {
		validationErr := err
		kept := make([]string, 0, len(accepted))
		cfg := def.Clone()
		try := func(key string) bool {
			trial := append(append([]string{}, kept...), key)
			merged, err := mergeYAMLKeys(def, present, nodes, trial)
			if err != nil {
				return false
			}
			kept = trial
			cfg = merged
			return true
		}
		var deferred []string
		for _, key := range accepted {
			if !try(key) {
				deferred = append(deferred, key)
			}
		}
		var skipped []string
		for _, key := range deferred {
			if !try(key) {
				skipped = append(skipped, key)
			}
		}
		for _, key := range skipped {
			if failClosedYAMLKey(key) {
				return nil, nil, &configValueError{key: key, err: validationErr}
			}
		}
		return cfg, skipped, nil
	}
}

func mergeYAMLKeys(base *Config, present map[string]any, nodes map[string]*yaml.Node, keys []string) (*Config, error) {
	cfg := base.Clone()
	for _, key := range keys {
		if _, err := overlayYAMLKey(cfg, key, nodes[key], present[key]); err != nil {
			return nil, err
		}
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func decodeUserOverlay(raw []byte, strict bool) (*userFile, error) {
	var user userFile
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(strict)
	if err := dec.Decode(&user); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if user.UpstreamTimeout != nil {
		d, err := parseYAMLDuration(*user.UpstreamTimeout)
		if err != nil {
			return nil, err
		}
		user.Config.UpstreamTimeout = d
	}
	return &user, nil
}

func overlayYAMLKey(dst *Config, key string, valueNode *yaml.Node, value any) (loose bool, err error) {
	field := FieldByKey(key)
	if field == nil {
		return false, errUnknownKey(key)
	}
	if err := checkYAMLType(*field, value); err != nil {
		return false, err
	}
	if valueNode == nil {
		return false, fmt.Errorf("%s: missing yaml node", key)
	}
	raw, err := yaml.Marshal(&yaml.Node{
		Kind: yaml.MappingNode,
		Tag:  "!!map",
		Content: []*yaml.Node{
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
			valueNode,
		},
	})
	if err != nil {
		return false, err
	}
	user, err := decodeUserOverlay(raw, true)
	if err != nil {
		user, err = decodeUserOverlay(raw, false)
		if err != nil {
			return false, err
		}
		loose = true
	}
	dst.mergeOverlay(&user.Config, map[string]any{key: value})
	return loose, nil
}

// parseYAMLDuration accepts Go duration strings ("5m", "0s"). Settings typed
// zeros go through asDuration, not this YAML probe.
func parseYAMLDuration(s string) (time.Duration, error) {
	return time.ParseDuration(strings.TrimSpace(s))
}

func checkQueueRetryAfter(d time.Duration) error {
	if d < 0 || d > QueueRetryAfterMax || d%time.Second != 0 {
		return fmt.Errorf("queue_retry_after: must be a whole-second duration %s..%s, got %s", FormatDuration(0), FormatDuration(QueueRetryAfterMax), FormatDuration(d))
	}
	return nil
}

// RetryAfterSeconds is the integer-seconds Retry-After value for proxy-owned
// 429s. HTTP Retry-After is delta-seconds; this is the only conversion.
func (c *Config) RetryAfterSeconds() int {
	if c == nil || c.QueueRetryAfter <= 0 {
		return 0
	}
	return int(c.QueueRetryAfter / time.Second)
}
