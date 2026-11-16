#!/usr/bin/env python3
"""Pin the MCP documentation to the registration and route sources.

The tool count is hand-maintained in prose, so it drifts silently when a tool
is added. The tool table and the operator route list can drift the same way.
This module reads the real owners instead of trusting a copy:

* every registered tool is exactly one docs/mcp.md table row, in order;
* every "<n> tools" mention in tracked Markdown equals the registration count;
* every route the MCP client owns appears in the operations route table;
* every operator path docs/mcp.md names is a route the client owns.
"""

import re
import unittest

from ..support import ROOT

SERVER = ROOT / "internal/mcp/server.go"
CLIENT = ROOT / "internal/mcp/client.go"
ROUTES = ROOT / "docs/routes.md"
MCP_DOC = ROOT / "docs/mcp.md"
SKIP_PARTS = frozenset({"agents", "node_modules", ".venv", ".git", "site"})
TOOL = re.compile(r'addTool\(server, &sdk\.Tool\{\s*\n\s*Name:\s+"([a-z_]+)"')
ROUTE = re.compile(r'^\t(Route\w+)\s+Route\s*=\s*"([^"]+)"', re.MULTILINE)
COUNT = re.compile(r"\b(\d+) (?:MCP )?tools\b")
TABLE_ROW = re.compile(r"^\|\s*`([a-z_]+)`\s*\|")
ROUTE_ROW = re.compile(r"^\|\s*`(?:GET|POST|GET/POST|GET/PUT|DELETE)\s+(\S+)")
DOC_PATH = re.compile(r"`(/(?:admin|metrics)/[a-z/]*)")


def registered_tools() -> list[str]:
    return TOOL.findall(SERVER.read_text(encoding="utf-8"))


def owned_routes() -> dict[str, str]:
    return dict(ROUTE.findall(CLIENT.read_text(encoding="utf-8")))


def documented_route_lines() -> list[str]:
    return [line for line in ROUTES.read_text(encoding="utf-8").splitlines()
            if ROUTE_ROW.match(line)]


def route_documented(path: str, rows: list[str]) -> bool:
    """A row may list sibling paths in its own cell (`/explorer`, `/log`)."""
    if any(path in row for row in rows):
        return True
    head, _, tail = path.rpartition("/")
    return any(head + "/" in row and f"/{tail}`" in row for row in rows)


def markdown_files():
    for path in ROOT.rglob("*.md"):
        if SKIP_PARTS.isdisjoint(path.relative_to(ROOT).parts):
            yield path


class MCPDocs(unittest.TestCase):
    def test_tool_table_matches_registration(self):
        tools = registered_tools()
        self.assertGreater(len(tools), 0, "no addTool registrations parsed")
        rows = {match.group(1) for line in MCP_DOC.read_text(encoding="utf-8").splitlines()
                if (match := TABLE_ROW.match(line))}
        self.assertEqual(rows, set(tools))

    def test_every_tool_count_mention_matches(self):
        tools = registered_tools()
        mentions = []
        for path in markdown_files():
            for line_number, line in enumerate(path.read_text(encoding="utf-8").splitlines(), 1):
                for match in COUNT.finditer(line):
                    mentions.append((path.relative_to(ROOT), line_number, int(match.group(1))))
        self.assertGreaterEqual(len(mentions), 3, mentions)
        wrong = [mention for mention in mentions if mention[2] != len(tools)]
        self.assertEqual(wrong, [], f"{len(tools)} tools are registered")

    def test_operations_route_table_documents_every_owned_route(self):
        rows = documented_route_lines()
        missing = {name: path for name, path in owned_routes().items()
                   if not route_documented(path, rows)}
        self.assertEqual(missing, {})

    def test_mcp_doc_names_only_owned_operator_paths(self):
        owned = set(owned_routes().values())
        named = set(DOC_PATH.findall(MCP_DOC.read_text(encoding="utf-8")))
        self.assertEqual(named - owned, set())


if __name__ == "__main__":
    unittest.main(verbosity=2)
