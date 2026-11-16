#!/usr/bin/env python3
"""One reader for the MCP tool registry in internal/mcp/server.go.

The registration calls are the public tool surface. The docs table, the
prose count and the brand readout all need that count; each used to parse or
copy it separately. This module owns the parse, so a new tool reaches every
consumer through one function.
"""
from __future__ import annotations

import re
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
SERVER = ROOT / "internal/mcp/server.go"
TOOL = re.compile(r'addTool\(server, &sdk\.Tool\{\s*\n\s*Name:\s+"([a-z_]+)"')


def registered_tools() -> list[str]:
    """Every registered tool name, in registration order."""
    return TOOL.findall(SERVER.read_text(encoding="utf-8"))


def tool_count() -> int:
    count = len(registered_tools())
    if count == 0:
        raise RuntimeError(f"no addTool registrations parsed from {SERVER}")
    return count
