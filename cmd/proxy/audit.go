package main

import (
	"fmt"
	"log"
	"strconv"
	"strings"

	"github.com/LLM-4-People/millivolt/internal/storage"
)

// auditLog is the one owner of the operator-action audit line. Purge, backup
// and restore change state that no request log would reconstruct, so each of
// them emits exactly one line per terminal outcome: the values an operator or a
// log reader needs to tell what was asked for and what happened, including when
// the response itself was lost. Fields are key/value pairs rendered as
// key="value" so a value carrying a space or an equals sign stays one token;
// an odd trailing field is a programming error and is rendered bare.
func auditLog(action string, fields ...any) {
	var b strings.Builder
	b.WriteString("audit: ")
	b.WriteString(action)
	for i := 0; i < len(fields); i += 2 {
		b.WriteByte(' ')
		fmt.Fprint(&b, fields[i])
		b.WriteString(`="`)
		if i+1 < len(fields) {
			fmt.Fprintf(&b, "%v", fields[i+1])
		} else {
			fields = append(fields, "")
		}
		b.WriteByte('"')
	}
	log.Print(b.String())
}

// describeMembers renders the archive member selection of a backup or restore
// as the audit value ("config", "database", "config+database").
func describeMembers(wantConfig, wantDB bool) string {
	switch {
	case wantConfig && wantDB:
		return "config+database"
	case wantConfig:
		return "config"
	case wantDB:
		return "database"
	default:
		return "none"
	}
}

// describeFilter renders the purge selection for an audit line. A nil filter is
// the bodyless full deletion; a non-nil one delegates to the filter's own
// renderer so the wording has one owner.
func describeFilter(filter *storage.PurgeFilter) string {
	if filter == nil {
		return "all"
	}
	return filter.Describe()
}

// reviewedText renders the optional previewed row count: "-" when the caller
// previewed nothing, so the audit line always carries the key.
func reviewedText(reviewed *int64) string {
	if reviewed == nil {
		return "-"
	}
	return strconv.FormatInt(*reviewed, 10)
}
