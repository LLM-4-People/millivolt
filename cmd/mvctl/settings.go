package main

// Client-side defaults. Every value is either the shipped internal/config
// default (the server already owns the number), a grammar owned by
// internal/backup or internal/mcp, or a command-local policy explained here.
// Parsing, help text and every command read this one struct, so a default
// changes here and nowhere else.

import (
	"time"

	"github.com/LLM-4-People/millivolt/internal/backup"
	"github.com/LLM-4-People/millivolt/internal/config"
)

// Client environment settings. The credential variable stays with
// internal/mcp; these two are the only names mvctl introduces.
const (
	proxyURLEnv = "MILLIVOLT_URL"
	noColorEnv  = "NO_COLOR"
)

// Exit codes. A command returns one of these through Result.Code or an error;
// main translates a canceled context to exitInterrupted and anything else to
// exitFailure unless the command chose a more specific code.
const (
	exitOK          = 0
	exitFailure     = 1
	exitUsageCode   = 2
	exitInterrupted = 130
)

type settings struct {
	Timeout      time.Duration
	QueryTimeout time.Duration

	RequestsPage int

	// WatchSamples is a command policy: five points make a readable sparkline
	// without holding a terminal for long. The band bounds --samples.
	WatchSamples    int
	WatchSamplesMin int
	WatchSamplesMax int
	WatchInterval   time.Duration

	BackupConfig   bool
	BackupDatabase bool

	RestoreConfig   bool
	RestoreDatabase bool

	RestoreConfigMode   string
	RestoreDatabaseMode string
}

func defaultSettings() settings {
	shipped := config.Default()
	return settings{
		Timeout:      shipped.MCPTimeout,
		QueryTimeout: shipped.MCPQueryTimeout,
		RequestsPage: shipped.DashLogRows,

		WatchSamples:    5,
		WatchSamplesMin: 2,
		WatchSamplesMax: 300,
		WatchInterval:   shipped.DashPollInterval,

		BackupConfig:   true,
		BackupDatabase: true,

		RestoreConfig:   true,
		RestoreDatabase: true,

		RestoreConfigMode:   backup.ModeReplace,
		RestoreDatabaseMode: backup.ModeReplace,
	}
}
