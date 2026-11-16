#!/usr/bin/env python3
"""One owner for the facts and copy the brand plates carry.

The specification readout, the social card and the cover share the same
product statement and the same sample-history numbers. They live here so a
tool addition or a copy change reaches every generated plate through one
value, and the brand check can compare the rendered art with the same facts.
"""
from __future__ import annotations

from scripts.mcp_registry import tool_count

TOOLS = tool_count()

HEADLINE_HTML = "See every AI request.<br>Keep your own routing."
HEADLINE_TEXT = "See every AI request. Keep your own routing."
EYEBROW = "one relay · three client wires · four upstream formats"
BRAND_NOTE = "model relay / dashboard"
FOOTER = "MIT · one Go binary · SQLite or in-memory"
SAMPLE_NOTE = "static capture · sample values, not a benchmark"

SOCIAL_LEDE = (
    "A self-hosted, cross-protocol inference proxy. Point any OpenAI or "
    "Anthropic client at <b>your upstream</b>, then watch the crossing in one "
    "live dashboard."
)

# The KPI values are a sample from the redacted history capture in
# docs/images/overview/dashboard.png. The plate and its caption say sample, not
# benchmark; the source capture is the one fixture both the cover and the
# social card show.
SAMPLE = {
    "requests": "100.16K",
    "requests_sub": "all tracked",
    "cost": "$3.34K",
    "cost_sub": "~3.785¢/req",
    "cache": "96.5%",
    "cache_sub": "20.78K of 21.53M in",
    "errors": "0.3%",
    "errors_sub": "284 of 100.16K",
}

# Four facts, one per recessed well. The tool count comes from the registry.
SPEC_ROWS = (
    {"value": "04", "label": "UPSTREAM FORMATS"},
    {"value": "01", "label": "GO BINARY"},
    {"value": str(TOOLS), "label": "MCP TOOLS"},
    {"value": "01", "label": "OPERATOR PLANE"},
)

SPEC_DESC = (
    f"Three client wires, four upstream formats, one Go binary, "
    f"{TOOLS} MCP tools, one operator plane."
)
