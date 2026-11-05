package main

// Config commands. The schema, kinds and bounds come from internal/config;
// mvctl only transports a revision-checked values map and renders the result.

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/mcp"
)

func cmdConfig(ctx *Context) (Result, error) {
	sub := "get"
	if len(ctx.Args) > 0 {
		sub = ctx.Args[0]
	}
	switch sub {
	case "get":
		return configGet(ctx)
	case "keys":
		return configKeys(ctx)
	case "diff":
		return configDiff(ctx)
	case "reload":
		return configReload(ctx)
	case "set":
		return configSet(ctx)
	default:
		return exitUsage(fmt.Errorf("unknown config subcommand %q", sub))
	}
}

func configGet(ctx *Context) (Result, error) {
	if len(ctx.Args) > 2 {
		return exitUsage(errors.New("usage: mvctl config get [KEY]"))
	}
	view, err := fetchConfig(ctx)
	if err != nil {
		return Result{Code: exitFailure}, err
	}
	reveal := ctx.Flags.Bool("reveal", false)
	doc := redactConfigDocument(view.Raw, reveal)
	values := documentMap(doc, "values")
	if len(ctx.Args) == 2 {
		key := ctx.Args[1]
		value, ok := values[key]
		if !ok {
			return exitUsage(fmt.Errorf("config key %q not found; run 'mvctl config keys'", key))
		}
		return Result{
			Pretty: sanitizeCell(key) + " = " + sanitizeCell(displayValue(value)),
			Data:   map[string]any{key: value},
		}, nil
	}
	keys := sortedKeys(values)
	rows := make([][]string, 0, len(keys))
	for _, key := range keys {
		rows = append(rows, []string{key, truncate(displayValue(values[key]), 80)})
	}
	pretty := heading("config") + "\n" + table([]string{"key", "value"}, rows)
	pretty += "\n\n" + kv([][2]string{
		{"revision", view.Document.Revision},
		{"path", view.Document.Path},
		{"writable", boolText(view.Document.Writable)},
		{"restart required", strings.Join(view.Document.RestartRequired, ", ")},
	})
	if !reveal {
		pretty += "\n" + dim("Credential-class provider header values are redacted; pass --reveal to print them.")
	}
	return Result{Pretty: pretty, Data: doc}, nil
}

func configKeys(ctx *Context) (Result, error) {
	if len(ctx.Args) > 2 {
		return exitUsage(errors.New("usage: mvctl config keys [SUBSTRING]"))
	}
	if err := ctx.rejectFlagsExcept(); err != nil {
		return exitUsage(err)
	}
	view, err := fetchConfig(ctx)
	if err != nil {
		return Result{Code: exitFailure}, err
	}
	needle := ""
	if len(ctx.Args) == 2 {
		needle = strings.ToLower(ctx.Args[1])
	}
	keys := make([]string, 0, len(view.Fields))
	for _, field := range view.Fields {
		if needle == "" || strings.Contains(strings.ToLower(field.Key), needle) {
			keys = append(keys, field.Key)
		}
	}
	sort.Strings(keys)
	return Result{Pretty: sanitizeText(strings.Join(keys, "\n")), Data: map[string]any{"keys": keys}}, nil
}

func configDiff(ctx *Context) (Result, error) {
	if len(ctx.Args) != 1 {
		return exitUsage(errors.New("usage: mvctl config diff"))
	}
	view, err := fetchConfig(ctx)
	if err != nil {
		return Result{Code: exitFailure}, err
	}
	reveal := ctx.Flags.Bool("reveal", false)
	doc := redactConfigDocument(view.Raw, reveal)
	values := documentMap(doc, "values")
	defaults := documentMap(doc, "defaults")
	type change struct {
		Key     string `json:"key"`
		Default any    `json:"default"`
		Value   any    `json:"value"`
	}
	changes := make([]change, 0)
	for _, key := range sortedKeys(values) {
		if reflect.DeepEqual(values[key], defaults[key]) {
			continue
		}
		changes = append(changes, change{Key: key, Default: defaults[key], Value: values[key]})
	}
	pretty := heading("config diff (saved file vs defaults)") + "\n"
	if len(changes) == 0 {
		pretty += dim("no changes from defaults")
	} else {
		rows := make([][]string, 0, len(changes))
		for _, item := range changes {
			rows = append(rows, []string{item.Key, truncate(displayValue(item.Default), 40), truncate(displayValue(item.Value), 40)})
		}
		pretty += table([]string{"key", "default", "saved"}, rows)
	}
	return Result{Pretty: pretty, Data: map[string]any{"changed": changes}}, nil
}

func configReload(ctx *Context) (Result, error) {
	if len(ctx.Args) != 1 {
		return exitUsage(errors.New("usage: mvctl config reload [--yes]"))
	}
	if err := ctx.rejectFlagsExcept(); err != nil {
		return exitUsage(err)
	}
	if result, proceed, err := guarded(ctx, "reload the live configuration", nil); !proceed {
		return result, err
	}
	reqCtx, cancel := ctx.bounded()
	defer cancel()
	var out map[string]any
	if _, err := ctx.C.PostJSON(reqCtx, mcp.RouteReload, nil, map[string]any{}, &out, nil); err != nil {
		return Result{Code: exitFailure}, err
	}
	return Result{Pretty: green("config reloaded") + "\n" + prettyJSON(out), Data: out}, nil
}

func configSet(ctx *Context) (Result, error) {
	if len(ctx.Args) < 2 {
		return exitUsage(errors.New("usage: mvctl config set KEY=VALUE [KEY=VALUE ...] [--yes]"))
	}
	view, err := fetchConfig(ctx)
	if err != nil {
		return Result{Code: exitFailure}, err
	}
	if !view.Document.Writable {
		return Result{Code: exitFailure}, errors.New("the proxy reports its config file is not writable")
	}
	if view.Document.Revision == "" {
		return Result{Code: exitFailure}, errors.New("the proxy reported no config revision; the config file may not be writable")
	}
	values := map[string]any{}
	for _, pair := range ctx.Args[1:] {
		key, raw, ok := strings.Cut(pair, "=")
		if !ok || key == "" {
			return exitUsage(fmt.Errorf("expected KEY=VALUE, got %q", pair))
		}
		if _, duplicate := values[key]; duplicate {
			return exitUsage(fmt.Errorf("config key %q appears more than once", key))
		}
		field, ok := view.field(key)
		if !ok {
			return exitUsage(fmt.Errorf("unknown config key %q; run 'mvctl config keys'", key))
		}
		value, err := coerceConfigValue(field, raw)
		if err != nil {
			return exitUsage(fmt.Errorf("%s: %w", key, err))
		}
		values[key] = value
	}
	reveal := ctx.Flags.Bool("reveal", false)
	previewValues := values
	if !reveal {
		previewValues = redactConfigValues(values)
	}
	preview := heading("config change preview") + "\n" + prettyJSON(previewValues)
	if result, proceed, err := guarded(ctx, "save the live configuration", previewValues); !proceed {
		result.Pretty = preview + "\n\n" + result.Pretty
		if data, ok := result.Data.(map[string]any); ok {
			data["values"] = previewValues
			data["revision"] = view.Document.Revision
		}
		return result, err
	}
	reqCtx, cancel := ctx.bounded()
	defer cancel()
	out, err := ctx.C.SetConfig(reqCtx, mcp.SetConfigInput{Revision: view.Document.Revision, Values: values})
	if err != nil {
		return Result{Code: exitFailure}, err
	}
	document := map[string]any{
		"saved":            out.Saved,
		"restart_required": out.RestartRequired,
	}
	if out.Values != nil {
		document["values"] = out.Values
		if !reveal {
			document["values"] = redactConfigValues(out.Values)
		}
	}
	if out.Error != "" {
		document["error"] = sanitizeText(out.Error)
	}
	if out.Warning != "" {
		document["warning"] = sanitizeText(out.Warning)
	}
	pretty := green("config saved") + "\n" + prettyJSON(document)
	if out.Warning != "" {
		pretty = yellow("config saved, but the reload failed") + "\n" + sanitizeText(out.Warning) + "\n" + prettyJSON(document)
	}
	return Result{Pretty: pretty, Data: document}, nil
}

func fetchConfig(ctx *Context) (ConfigView, error) {
	reqCtx, cancel := ctx.bounded()
	defer cancel()
	document, err := ctx.C.GetConfig(reqCtx)
	if err != nil {
		return ConfigView{}, err
	}
	view, err := decodeConfig(document)
	if err != nil {
		return ConfigView{}, fmt.Errorf("decode /admin/config: %w", err)
	}
	return view, nil
}

func coerceConfigValue(field config.Field, raw string) (any, error) {
	switch field.Kind {
	case config.KindBool:
		if strings.TrimSpace(raw) == "" {
			return nil, errors.New("want true or false; an empty value is not a bool")
		}
		return parseBool(raw)
	case config.KindInt:
		value, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
		if err != nil {
			return nil, errors.New("want an integer")
		}
		return value, nil
	case config.KindDuration, config.KindString, config.KindBytes:
		// The server owns the duration and byte-size grammars; the command
		// transports the operator's text and lets the config boundary reject it.
		return raw, nil
	case config.KindStrings, config.KindProviders, config.KindAliases,
		config.KindModelRules, config.KindRequestOverrides, config.KindSubConversations:
		var value any
		if err := json.Unmarshal([]byte(raw), &value); err != nil {
			return nil, fmt.Errorf("want a JSON value for this setting: %w", err)
		}
		return value, nil
	default:
		return raw, nil
	}
}

func redactConfigDocument(raw map[string]any, reveal bool) map[string]any {
	if reveal {
		return raw
	}
	out := make(map[string]any, len(raw))
	for key, value := range raw {
		out[key] = value
	}
	for _, section := range []string{"values", "effective"} {
		if values, ok := raw[section].(map[string]any); ok {
			out[section] = redactConfigValues(values)
		}
	}
	// The last reload's error text can quote a value the loader rejected. Keep
	// the outcome metadata and omit the quoted text unless the operator asked
	// to reveal secrets.
	if reload, ok := raw["last_reload"].(map[string]any); ok {
		clone := make(map[string]any, len(reload))
		for key, value := range reload {
			clone[key] = value
		}
		if message, ok := clone["error"].(string); ok && strings.TrimSpace(message) != "" {
			clone["error"] = mcp.RedactionMarker
		}
		out["last_reload"] = clone
	}
	return out
}

// redactConfigValues delegates the one secret-bearing part of a saved config
// document to internal/config, which owns the schema shape and the credential
// header vocabulary.
func redactConfigValues(values map[string]any) map[string]any {
	return config.RedactProviderHeaders(values, mcp.RedactionMarker)
}

func documentMap(document map[string]any, key string) map[string]any {
	if value, ok := document[key].(map[string]any); ok {
		return value
	}
	return map[string]any{}
}

func sortedKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func displayValue(value any) string {
	switch typed := value.(type) {
	case nil:
		return "null"
	case string:
		return typed
	case bool:
		return strconv.FormatBool(typed)
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	default:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return fmt.Sprint(typed)
		}
		return string(encoded)
	}
}
