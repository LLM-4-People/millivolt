package main

import "github.com/LLM-4-People/millivolt/internal/mcp"

func cmdRestart(ctx *Context) (Result, error) {
	if err := ctx.requireNoExtraArgs("mvctl restart --yes"); err != nil {
		return exitUsage(err)
	}
	if result, proceed, err := guarded(ctx, "restart the proxy", nil); !proceed {
		return result, err
	}
	reqCtx, cancel := ctx.bounded()
	defer cancel()
	var out map[string]any
	if _, err := ctx.C.PostJSON(reqCtx, mcp.RouteRestart, nil, map[string]any{}, &out, nil); err != nil {
		return Result{Code: exitFailure}, err
	}
	return Result{Pretty: green("restart requested") + "\n" + prettyJSON(out), Data: out}, nil
}
