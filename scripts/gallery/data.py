"""The published dashboard capture registry.

Order is reading order: stations follow the dashboard guide's own sequence, and
a plate's number is its position here. Add a view by adding one Plate and its
scene handler; the capture run, gallery page, site plates and tests all follow.
"""
from __future__ import annotations

from .model import Gallery, Plate, Station, load

STATIONS = (
    Station(
        "overview", "The whole bench",
        "The default view at 1920 px and at phone width; every plate carries "
        "the Black and Clear surfaces across one diagonal."),
    Station(
        "timelines", "Six readings of one ruler",
        "The plotted presets, the summary tiles, and the window picker that "
        "bounds them."),
    Station(
        "explorer", "Nine ways into the traffic",
        "The dimension rail, its cards, and one scoped reading."),
    Station(
        "requests", "Every crossing",
        "The request log, an absorbed retry, and the drawer that explains a "
        "single crossing."),
    Station(
        "console", "The operator plane",
        "The five policy sections and the composer behind them."),
    Station(
        "settings", "The patch panel",
        "Five categories of the public example configuration."),
    Station(
        "themes", "One bench, two surfaces",
        "The shared theme tray on Black and on Clear."),
)


def _plate(plate_id: str, path: str, station: str, title: str, alt: str, note: str,
           kind: str, wide: bool = False, **setup: object) -> Plate:
    return Plate(id=plate_id, path="images/" + path, station=station, title=title,
                 alt=alt, note=note, kind=kind, setup=setup, wide=wide)


PLATES = (
    # -- overview ---------------------------------------------------------
    _plate(
        "overview", "overview/dashboard.png", "overview",
        "The whole bench",
        "The millivolt dashboard at 1920 pixels: nine global readings above a "
        "provider explorer, a traffic timeline and a request log of recorded "
        "requests.",
        "Global totals stay fixed while the explorer scopes the timeline and "
        "log; upper-left Black, lower-right Clear.",
        "page", dim="provider", viewport="desktop", window="all", full_page=True
    ),
    _plate(
        "overview-mobile", "overview/mobile.png", "overview",
        "390 pixels wide",
        "The dashboard at phone width, with the KPI band stacked and the "
        "explorer reduced to a dimension picker.",
        "Below 1200 pixels the locked bench becomes a scrolling page and the "
        "nine-item rail becomes one picker.",
        "page", dim="provider", viewport="mobile", window="all", full_page=False
    ),

    # -- timelines --------------------------------------------------------
    _plate(
        "chart-overview", "charts/overview.png", "timelines",
        "Summary tiles",
        "The Overview preset: five tiles for volume, tokens, cost, health and "
        "speed, each with a share line and a sparkline.",
        "The overview preset answers what the window contains before any "
        "series is drawn.",
        "chart", preset="overview", viewport="card", window="all"
    ),
    _plate(
        "chart-traffic", "charts/traffic.png", "timelines",
        "Traffic",
        "Traffic timeline with request and error counts from recorded history.",
        "Requests and errors share one time axis; the totals strip carries the "
        "window rate.",
        "chart", preset="traffic", viewport="card", window="all"
    ),
    _plate(
        "chart-errors", "charts/errors.png", "timelines",
        "Errors",
        "Error timeline with affected-request bars and a separate percentage "
        "axis.",
        "A 429 is a rate limit, not an error; the two series stay separate.",
        "chart", preset="errors", viewport="card", window="all"),
    _plate(
        "chart-tokens", "charts/tokens.png", "timelines",
        "Tokens",
        "Token timeline with input, output, reasoning and cached usage.",
        "Cached reads are drawn beside billed input, so a high cache share is "
        "visible rather than inferred.",
        "chart", preset="tokens", viewport="card", window="all"),
    _plate(
        "chart-latency", "charts/speed-latency.png", "timelines",
        "Speed and latency",
        "Speed and latency timeline with independent axes and the p95 "
        "percentile selected.",
        "The percentile control applies to every measured rate in the preset.",
        "chart", preset="latency", pct="95", viewport="card", window="all"
    ),
    _plate(
        "chart-cost", "charts/cost.png", "timelines",
        "Provider-reported cost",
        "Cost timeline with spend and request-count axes.",
        "Spend is whatever the upstream reported, plotted beside the request "
        "volume that produced it.",
        "chart", preset="cost", viewport="card", window="all"),
    _plate(
        "chart-window", "charts/window.png", "timelines",
        "The window picker",
        "The timeframe panel open over the timeline: a coverage rail, a zoom "
        "ladder from one minute to one year, and calendar presets.",
        "Windows wider than the stored history are disabled, so the picker "
        "cannot promise history that is not there.",
        "chart-menu", viewport="card"
    ),

    # -- explorer ---------------------------------------------------------
    _plate(
        "explorer-providers", "explorer/providers.png", "explorer",
        "Providers",
        "Provider explorer with pseudonymous groups, recorded costs and health "
        "signals.",
        "The rail counts distinct values; each card shows its share and the "
        "signals that matter for that dimension.",
        "element", dim="provider", viewport="explorer", selector="#explorer"
    ),
    _plate(
        "explorer-models", "explorer/models.png", "explorer",
        "Models",
        "Model explorer with pseudonymous model groups and recorded usage "
        "metrics.",
        "The model dimension applies the configured grouping rules before it "
        "counts.",
        "element", dim="model", viewport="explorer", selector="#explorer"),
    _plate(
        "explorer-clients", "explorer/clients.png", "explorer",
        "Clients",
        "Client explorer with pseudonymous client groups and their recorded "
        "traffic.",
        "The client dimension answers which caller produced a share of the "
        "window.",
        "element", dim="client", viewport="explorer", selector="#explorer"),
    _plate(
        "explorer-conversations", "explorer/conversations.png", "explorer",
        "Conversations",
        "Conversation explorer with main and sub counts and recorded request "
        "totals.",
        "Conversation filters select exact leaves; the rail reports main and "
        "sub conversations separately.",
        "element", dim="conversation", viewport="explorer", selector="#explorer"),
    _plate(
        "explorer-tools", "explorer/tools.png", "explorer",
        "Tools",
        "Tool explorer with recorded call counts and the traffic behind them.",
        "The tool dimension counts calls the relay observed, not calls a "
        "client claimed.",
        "element", dim="tool", viewport="explorer", selector="#explorer"),
    _plate(
        "explorer-time", "explorer/time.png", "explorer",
        "Time",
        "Time explorer with recorded buckets and their request totals.",
        "Time is a grouping like any other, so a bucket can be filtered and "
        "compared with the rest of the window.",
        "element", dim="time", viewport="explorer", selector="#explorer"),
    _plate(
        "explorer-status", "explorer/status.png", "explorer",
        "Status",
        "Status explorer with final HTTP classes and their recorded counts.",
        "The status dimension separates what the client received from what the "
        "upstream first returned.",
        "element", dim="status", viewport="explorer", selector="#explorer"),
    _plate(
        "explorer-errors", "explorer/errors.png", "explorer",
        "Error class",
        "Error explorer with canonical error classes and their affected "
        "request counts.",
        "The error dimension groups by the proxy's canonical class, not by "
        "provider wording.",
        "element", dim="error", viewport="explorer", selector="#explorer"),
    _plate(
        "explorer-keys", "explorer/keys.png", "explorer",
        "API keys",
        "API key explorer with pseudonymous key groups and their recorded "
        "traffic.",
        "Keys are hashed in the copy, so a group is stable without exposing a "
        "credential.",
        "element", dim="key", viewport="explorer", selector="#explorer"),
    _plate(
        "explorer-scoped", "explorer/scoped.png", "explorer",
        "A scoped reading",
        "The explorer filtered to one provider, with the scope chip above the "
        "rail and the selected card marked.",
        "A card toggles an exact filter; the chip removes it, and the timeline "
        "and log follow the same scope.",
        "scoped", viewport="explorer", selector="#explorer"),

    # -- requests ---------------------------------------------------------
    _plate(
        "requests-table", "requests/table.png", "requests",
        "The request log",
        "Request table with recorded statuses, token usage, latency, "
        "throughput and cost.",
        "Each row is one crossing; the columns are the facts a later review "
        "needs first.",
        "element", dim="model", viewport="wide",
        selector=".grid-pair > .card:last-child",
        require_fit="#tbl-requests", wide=True
    ),
    _plate(
        "requests-retries", "requests/retries.png", "requests",
        "An absorbed retry",
        "The request log with one row expanded into its absorbed retry "
        "attempts, each carrying its own status and timing.",
        "A retry the client never saw is still a recorded upstream attempt.",
        "attempts", viewport="wide", wide=True
    ),
    _plate(
        "requests-details", "requests/details.png", "requests",
        "The request drawer",
        "Request drawer showing pseudonymous identity, the summary readout and "
        "recorded request parameters.",
        "The drawer opens on the record, not on a reconstruction of it.",
        "drawer", viewport="drawer"
    ),
    _plate(
        "requests-performance", "requests/performance.png", "requests",
        "Tokens and performance",
        "The same request drawer showing token and performance measurements.",
        "Usage is grouped by the question it answers, so timing and tokens do "
        "not compete in one list.",
        "drawer", viewport="drawer-short", group="usage"),
    _plate(
        "requests-diagnostics", "requests/diagnostics.png", "requests",
        "Diagnostics",
        "The request drawer's diagnostics group, with queue waits, rate-limit "
        "handling and absorbed attempts.",
        "The diagnostics group is where a transparent retry stops being "
        "invisible.",
        "drawer", viewport="drawer", group="diagnostics"
    ),

    # -- console ----------------------------------------------------------
    _plate(
        "console-overview", "console/overview.png", "console",
        "Overview",
        "The operator console overview with the live line, section rail and "
        "the change ledger.",
        "The overview is the register of what is in force, not a second "
        "settings form.",
        "console", section="overview", viewport="console"),
    _plate(
        "console-limits", "console/limits.png", "console",
        "Limits",
        "The limits section with no policy set and an empty rule register.",
        "An empty policy surface states the state instead of hiding it.",
        "console", section="limits", viewport="console"),
    _plate(
        "console-limits-composer", "console/limits-composer.png", "console",
        "The limit composer",
        "The limit composer open with its scope chips, budgets, window, "
        "schedule and live preview.",
        "A draft is local until it is committed; the composer previews the "
        "effect before a write.",
        "console", section="limits", viewport="console", composer=True
    ),
    _plate(
        "console-pause", "console/pause.png", "console",
        "Pause",
        "The pause section before any hold exists.",
        "A hold parks new work for a scope without changing a route.",
        "console", section="pause", viewport="console"),
    _plate(
        "console-capture", "console/capture.png", "console",
        "Capture",
        "The capture section with no session running.",
        "A capture session retains bodies for a bounded scope and time; the "
        "panel states both.",
        "console", section="capture", viewport="console"),
    _plate(
        "console-export", "console/export.png", "console",
        "Export",
        "The export section previewing the whole-history record count and the "
        "download receipt.",
        "The receipt counts what the chosen mode will write before the file is "
        "produced.",
        "console", section="export", viewport="console"),
    _plate(
        "console-system", "console/system.png", "console",
        "System",
        "The system section with restart available and the four restart steps.",
        "A restart is a state machine with named steps, not a button with a "
        "spinner.",
        "console", section="system", viewport="console"),

    # -- settings ---------------------------------------------------------
    _plate(
        "settings-dashboard", "settings/dashboard.png", "settings",
        "Dashboard",
        "Settings showing the public dashboard configuration fields.",
        "Every field is generated from the schema, including its help and the "
        "restart badge.",
        "settings", category="dashboard", viewport="settings"),
    _plate(
        "settings-queue", "settings/queue.png", "settings",
        "Queue and retry",
        "Queue and retry Settings with their public values and field "
        "descriptions.",
        "Retry policy, error classes and pacing sit together because they act "
        "on the same wait.",
        "settings", category="queue", viewport="settings-tall"),
    _plate(
        "settings-storage", "settings/storage.png", "settings",
        "Storage",
        "Storage Settings with the bounded writer, queue, persistence and "
        "deletion controls.",
        "Deletion is scoped and previewed; the storage facts come from the "
        "running store.",
        "settings", category="storage", viewport="settings"),
    _plate(
        "settings-providers", "settings/providers.png", "settings",
        "Providers",
        "Provider Settings showing the public Cursor compatibility profile.",
        "Provider data is a field map in configuration, not a branch in the "
        "router.",
        "settings", category="providers", viewport="settings"
    ),
    _plate(
        "settings-backup", "settings/backup.png", "settings",
        "Backup and restore",
        "Backup Settings with the download and restore rows and their scope "
        "choices.",
        "A restore is inspected before it is applied; the sheet says what will "
        "be replaced.",
        "settings", category="backup", viewport="settings-tall"
    ),

    # -- themes -----------------------------------------------------------
    _plate(
        "theme-tray", "theme/tray.png", "themes",
        "The theme tray",
        "The shared theme tray in both surfaces: two comparison panes, a "
        "surface switch and eight palettes.",
        "Palette, surface and order come from the same registry the site "
        "publishes; the seam is the surface switch made visible.",
        "theme", viewport="desktop"
    ),
)

GALLERY: Gallery = load(STATIONS, PLATES)
