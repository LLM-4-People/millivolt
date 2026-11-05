package main

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	backupparams "github.com/LLM-4-People/millivolt/internal/backup/params"
	"github.com/LLM-4-People/millivolt/internal/config"
	"github.com/LLM-4-People/millivolt/internal/mcp"
)

func cmdBackup(ctx *Context) (Result, error) {
	if err := ctx.requireNoExtraArgs("mvctl backup --out FILE [--config=false] [--database=false] [--overwrite]"); err != nil {
		return exitUsage(err)
	}
	out := strings.TrimSpace(ctx.Flags.String("out", ""))
	if out == "" {
		return exitUsage(errors.New("--out is required: choose the archive path explicitly"))
	}
	wantConfig := ctx.Flags.Bool("config", ctx.G.Settings.BackupConfig)
	wantDatabase := ctx.Flags.Bool("database", ctx.G.Settings.BackupDatabase)
	if !wantConfig && !wantDatabase {
		return exitUsage(errors.New("select config, database, or both"))
	}
	if err := checkOutputPath(out, ctx.Flags.Bool("overwrite", false)); err != nil {
		return exitUsage(err)
	}
	query := url.Values{
		backupparams.ParamConfig:   {backupparams.BoolParam(wantConfig)},
		backupparams.ParamDatabase: {backupparams.BoolParam(wantDatabase)},
	}
	// Backup streams for as long as a large database snapshot takes. Only
	// cancellation bounds it; the generic --timeout is deliberately not
	// applied to a transfer whose size the proxy owns.
	body, err := ctx.C.GetStream(ctx.Ctx, mcp.RouteBackup, query)
	if err != nil {
		return Result{Code: exitFailure}, err
	}
	defer body.Close()
	written, err := writeArchive(out, body, ctx.Flags.Bool("overwrite", false))
	if err != nil {
		return Result{Code: exitFailure}, err
	}
	return Result{
		Pretty: fmt.Sprintf("%s %s written (%s)", green("backup saved"), sanitizeCell(out), config.FormatByteSize(written)),
		Data:   map[string]any{"file": out, "bytes": written, "config": wantConfig, "database": wantDatabase},
	}, nil
}

// writeArchive publishes only a complete, magic-checked archive. A failed
// download removes its private temporary file; an existing destination is
// replaced only with --overwrite.
func writeArchive(out string, body io.Reader, overwrite bool) (int64, error) {
	dir := filepath.Dir(out)
	base := filepath.Base(out)
	file, err := os.CreateTemp(dir, "."+base+".*.part")
	if err != nil {
		return 0, fmt.Errorf("create temporary archive: %w", err)
	}
	tmp := file.Name()
	published := false
	defer func() {
		if !published {
			_ = os.Remove(tmp)
		}
	}()
	prefix := make([]byte, len(backupparams.Magic))
	if _, err := io.ReadFull(body, prefix); err != nil {
		_ = file.Close()
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return 0, errors.New("millivolt returned a truncated backup (no archive header)")
		}
		return 0, fmt.Errorf("read backup header: %w", err)
	}
	if !backupparams.HasMagic(prefix) {
		_ = file.Close()
		return 0, errors.New("millivolt returned a body that is not a millivolt archive")
	}
	if _, err := file.Write(prefix); err != nil {
		_ = file.Close()
		return 0, fmt.Errorf("write backup header: %w", err)
	}
	rest, copyErr := io.Copy(file, body)
	if copyErr != nil {
		_ = file.Close()
		return 0, fmt.Errorf("stream backup: %w", copyErr)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return 0, fmt.Errorf("sync backup: %w", err)
	}
	if err := file.Close(); err != nil {
		return 0, fmt.Errorf("close backup: %w", err)
	}
	if !overwrite {
		if _, err := os.Lstat(out); err == nil {
			return 0, fmt.Errorf("%s appeared while the backup was downloading; pass --overwrite to replace it", out)
		} else if !errors.Is(err, os.ErrNotExist) {
			return 0, err
		}
	}
	if err := os.Rename(tmp, out); err != nil {
		return 0, fmt.Errorf("publish backup: %w", err)
	}
	published = true
	// A rename is durable only after the directory entry is flushed. The
	// archive content was synced above; this makes the published name survive a
	// crash as well. The file already exists if this fails, so the error says
	// the archive was written but its directory entry could not be synced.
	if dirFile, err := os.Open(dir); err == nil {
		syncErr := dirFile.Sync()
		closeErr := dirFile.Close()
		if syncErr != nil || closeErr != nil {
			if syncErr == nil {
				syncErr = closeErr
			}
			return 0, fmt.Errorf("%s was written but its directory entry could not be synced: %w", out, syncErr)
		}
	} else {
		return 0, fmt.Errorf("%s was written but its directory could not be opened for sync: %w", out, err)
	}
	return int64(len(prefix)) + rest, nil
}

func checkOutputPath(out string, overwrite bool) error {
	info, err := os.Stat(out)
	switch {
	case err == nil && info.IsDir():
		return fmt.Errorf("%s is a directory", out)
	case err == nil && !overwrite:
		return fmt.Errorf("%s already exists; pass --overwrite to replace it", out)
	case err != nil && !errors.Is(err, os.ErrNotExist):
		return err
	}
	return nil
}

func cmdRestore(ctx *Context) (Result, error) {
	if err := ctx.requireNoExtraArgs("mvctl restore --file FILE [--inspect] [--reveal] [--yes]"); err != nil {
		return exitUsage(err)
	}
	path := strings.TrimSpace(ctx.Flags.String("file", ""))
	if path == "" {
		return exitUsage(errors.New("--file is required"))
	}
	wantConfig := ctx.Flags.Bool("config", ctx.G.Settings.RestoreConfig)
	wantDatabase := ctx.Flags.Bool("database", ctx.G.Settings.RestoreDatabase)
	if !wantConfig && !wantDatabase {
		return exitUsage(errors.New("select config, database, or both"))
	}
	configMode, err := restoreMode(ctx, "config-mode", ctx.G.Settings.RestoreConfigMode)
	if err != nil {
		return exitUsage(err)
	}
	databaseMode, err := restoreMode(ctx, "database-mode", ctx.G.Settings.RestoreDatabaseMode)
	if err != nil {
		return exitUsage(err)
	}
	inspect := ctx.Flags.Bool("inspect", false) || !ctx.G.Yes
	file, err := os.Open(path)
	if err != nil {
		return Result{Code: exitFailure}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return Result{Code: exitFailure}, err
	}
	if !info.Mode().IsRegular() {
		return exitUsage(fmt.Errorf("%s is not a regular file", path))
	}
	prefix := make([]byte, len(backupparams.Magic))
	if _, err := io.ReadFull(file, prefix); err != nil {
		return exitUsage(fmt.Errorf("%s is too small to be a millivolt archive", path))
	}
	if !backupparams.HasMagic(prefix) {
		return exitUsage(fmt.Errorf("%s is not a millivolt archive", path))
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return Result{Code: exitFailure}, err
	}
	query := url.Values{
		backupparams.ParamConfig:       {backupparams.BoolParam(wantConfig)},
		backupparams.ParamDatabase:     {backupparams.BoolParam(wantDatabase)},
		backupparams.ParamConfigMode:   {configMode},
		backupparams.ParamDatabaseMode: {databaseMode},
	}
	if inspect {
		query.Set(backupparams.ParamInspect, backupparams.BoolParam(true))
	}
	// The upload is the restore's long leg. It is bounded by cancellation,
	// not by the generic request timeout.
	raw, err := ctx.C.PostStream(ctx.Ctx, mcp.RouteRestore, query, "application/octet-stream", file, info.Size())
	if err != nil {
		return Result{Code: exitFailure}, err
	}
	var out map[string]any
	if err := decodeJSON(raw, &out); err != nil {
		return Result{Code: exitFailure}, fmt.Errorf("decode restore response: %w", err)
	}
	selection := restoreSelection(wantConfig, wantDatabase, configMode, databaseMode)
	if inspect {
		document := redactRestoreInspect(out, ctx.Flags.Bool("reveal", false))
		pretty := green("inspect complete") + "\n" + selection + "\n" + prettyJSON(document)
		if !ctx.Flags.Bool("reveal", false) {
			pretty += "\n" + dim("Credential-class provider header values are redacted; pass --reveal to print them.")
		}
		return Result{
			Pretty: pretty,
			Data: map[string]any{
				"inspect": true, "applied": false, "selection": restoreSelectionData(wantConfig, wantDatabase, configMode, databaseMode),
				"result": document,
			},
		}, nil
	}
	pretty := green("restore applied") + "\n" + selection + "\n" + prettyJSON(out)
	if restart, ok := out["restart_required"].([]any); ok && len(restart) > 0 {
		pretty = yellow("Restore applied; restart required before every change takes effect.") + "\n" + selection + "\n" + prettyJSON(out)
	}
	return Result{
		Pretty: pretty,
		Data: map[string]any{
			"inspect": false, "applied": true,
			"selection": restoreSelectionData(wantConfig, wantDatabase, configMode, databaseMode),
			"result":    out,
		},
	}, nil
}

func restoreMode(ctx *Context, flag, fallback string) (string, error) {
	if !ctx.Flags.Present(flag) {
		return fallback, nil
	}
	mode, err := backupparams.ParseRestoreMode(ctx.Flags.String(flag, ""))
	if err != nil {
		return "", fmt.Errorf("--%s: %w", flag, err)
	}
	return mode, nil
}

// redactRestoreInspect removes the one secret-bearing member of an inspect
// document: the archived config's provider headers. The rest is counts, key
// names and database spans.
func redactRestoreInspect(document map[string]any, reveal bool) map[string]any {
	if reveal {
		return document
	}
	out := make(map[string]any, len(document))
	for key, value := range document {
		out[key] = value
	}
	section, ok := document["config"].(map[string]any)
	if !ok {
		return out
	}
	configSection := make(map[string]any, len(section))
	for key, value := range section {
		configSection[key] = value
	}
	if values, ok := section["values"].(map[string]any); ok {
		configSection["values"] = config.RedactProviderHeaders(values, mcp.RedactionMarker)
	}
	out["config"] = configSection
	return out
}

func restoreSelectionData(wantConfig, wantDatabase bool, configMode, databaseMode string) map[string]any {
	return map[string]any{
		"config": wantConfig, "database": wantDatabase,
		"config_mode": configMode, "database_mode": databaseMode,
	}
}

// restoreSelection names the members and modes the apply would use. The
// inspect response itself has no mode field, so the command states them
// beside it; an operator sees that an omitted mode means the broader replace.
func restoreSelection(wantConfig, wantDatabase bool, configMode, databaseMode string) string {
	return heading("restore selection") + "\n" + kv([][2]string{
		{"config", boolText(wantConfig)},
		{"database", boolText(wantDatabase)},
		{"config mode", configMode},
		{"database mode", databaseMode},
	})
}
