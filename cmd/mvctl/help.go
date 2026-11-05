package main

import (
	"strconv"
	"strings"

	"github.com/LLM-4-People/millivolt"
)

func helpText(cmds []Command, inv invocation) string {
	defaults := inv.global.Settings
	if inv.command != nil {
		return commandHelp(*inv.command, inv.global.Settings)
	}
	if len(inv.args) > 0 {
		for i := range cmds {
			if cmds[i].Name == inv.args[0] {
				return commandHelp(cmds[i], inv.global.Settings)
			}
		}
		return "unknown command " + strconv.Quote(inv.args[0])
	}
	var out strings.Builder
	out.WriteString("mvctl " + millivolt.Version() + " - millivolt operator CLI\n\n")
	out.WriteString("Usage: mvctl [global flags] <command> [flags] [args]\n\n")
	out.WriteString("Global flags:\n")
	for _, spec := range globalFlagSpecs(defaults) {
		out.WriteString("  " + flagLabel(spec) + "  " + spec.usage + "\n")
	}
	out.WriteString("\nCommands:\n")
	group := ""
	for _, cmd := range cmds {
		if cmd.Group != group {
			group = cmd.Group
			out.WriteString("  " + title(group) + "\n")
		}
		out.WriteString("    " + pad(cmd.Name, 12, false) + cmd.Summary + "\n")
	}
	out.WriteString("\nRun 'mvctl help <command>' for command flags.\n")
	return strings.TrimRight(out.String(), "\n")
}

func commandHelp(cmd Command, defaults settings) string {
	var out strings.Builder
	out.WriteString("Usage: mvctl " + cmd.Name)
	if cmd.Args != "" {
		out.WriteString(" " + cmd.Args)
	}
	out.WriteString("\n\n" + cmd.Summary + "\n")
	if len(cmd.Flags) > 0 {
		out.WriteString("\nFlags:\n")
		for _, spec := range cmd.Flags {
			out.WriteString("  " + flagLabel(spec) + "  " + spec.usage + "\n")
		}
	}
	out.WriteString("\nGlobal flags:\n")
	for _, spec := range globalFlagSpecs(defaults) {
		out.WriteString("  " + flagLabel(spec) + "  " + spec.usage + "\n")
	}
	return strings.TrimRight(out.String(), "\n")
}

func flagLabel(spec flagSpec) string {
	label := "--" + spec.name
	if spec.kind != kindBool {
		label += " VALUE"
	}
	if spec.short != 0 {
		label = "-" + string(spec.short) + ", " + label
	}
	return pad(label, 22, false)
}

func title(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
