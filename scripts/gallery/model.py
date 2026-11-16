"""Schema and validation for the dashboard capture gallery.

One registry describes every published application capture: where it is
published, which scene captures it, which viewport it uses, what it proves and
which magnified details it carries. The capture runner, the site plate
renderer, the gallery page generator and the tests all read this module, so a
new view reaches every surface through one declaration.

Coordinates and sizes are data, never layout guesses:

* a loupe box is a fraction of the plate (0..1), so a recapture at a different
  pixel size keeps the same detail window;
* a viewport is a named entry in VIEWPORTS, so plates cannot each invent a
  private size;
* a scene kind declares exactly which setup keys it accepts, so a typo fails
  at import instead of capturing the wrong surface.
"""
from __future__ import annotations

from dataclasses import dataclass, field
from pathlib import Path
from types import MappingProxyType
from typing import Iterator, Mapping

ROOT = Path(__file__).resolve().parents[2]
IMAGE_ROOT = ROOT / "docs/images"


@dataclass(frozen=True)
class Viewport:
    name: str
    width: int
    height: int


@dataclass(frozen=True)
class Plate:
    id: str
    path: str
    station: str
    title: str
    alt: str
    note: str
    kind: str
    setup: Mapping[str, object]
    wide: bool = False


@dataclass(frozen=True)
class Station:
    id: str
    title: str
    lede: str


VIEWPORTS = MappingProxyType({
    "desktop": Viewport("desktop", 1920, 1200),
    "explorer": Viewport("explorer", 1440, 1180),
    "card": Viewport("card", 1180, 1000),
    "wide": Viewport("wide", 2208, 900),
    "drawer": Viewport("drawer", 1180, 850),
    "drawer-short": Viewport("drawer-short", 1180, 600),
    "console": Viewport("console", 1440, 900),
    "settings": Viewport("settings", 1440, 720),
    "settings-tall": Viewport("settings-tall", 1440, 800),
    "mobile": Viewport("mobile", 390, 844),
})

# A scene kind is a capture strategy, not a view. It declares the setup keys it
# understands so the registry stays a schema and the runner stays one module.
SCENE_KEYS = MappingProxyType({
    "page": frozenset({"dim", "viewport", "window", "preset", "pct", "full_page"}),
    "element": frozenset({"dim", "viewport", "selector", "window", "theme", "require_fit"}),
    "console": frozenset({"section", "viewport", "composer"}),
    "settings": frozenset({"category", "viewport"}),
    "drawer": frozenset({"viewport", "group"}),
    "attempts": frozenset({"viewport"}),
    "chart": frozenset({"preset", "pct", "viewport", "window"}),
    "chart-menu": frozenset({"viewport"}),
    "theme": frozenset({"viewport"}),
    "scoped": frozenset({"viewport", "selector"}),
})

REQUIRED_KEYS = MappingProxyType({
    "page": frozenset({"dim", "viewport"}),
    "element": frozenset({"dim", "viewport", "selector"}),
    "console": frozenset({"section", "viewport"}),
    "settings": frozenset({"category", "viewport"}),
    "drawer": frozenset({"viewport"}),
    "attempts": frozenset({"viewport"}),
    "chart": frozenset({"preset", "viewport"}),
    "chart-menu": frozenset({"viewport"}),
    "theme": frozenset({"viewport"}),
    "scoped": frozenset({"viewport", "selector"}),
})

CONSOLE_SECTIONS = ("overview", "limits", "pause", "capture", "export", "system")
DRAWER_GROUPS = ("usage", "diagnostics")
SETTINGS_CATEGORIES = ("dashboard", "queue", "storage", "providers", "backup")
EXPLORER_DIMENSIONS = ("provider", "model", "client", "conversation", "tool",
                      "time", "status", "error", "key")
CHART_PRESETS = ("traffic", "errors", "tokens", "latency", "cost", "overview")
PLATE_NOTE_LIMIT = 140


def validate(stations: tuple[Station, ...], plates: tuple[Plate, ...]) -> None:
    """Fail the import on a registry that cannot be captured or rendered."""
    errors: list[str] = []
    station_ids = [station.id for station in stations]
    if len(set(station_ids)) != len(station_ids):
        errors.append("station ids are not unique")
    known_stations = set(station_ids)
    plate_ids: set[str] = set()
    paths: set[str] = set()
    for plate in plates:
        if plate.id in plate_ids:
            errors.append(f"duplicate plate id: {plate.id}")
        plate_ids.add(plate.id)
        if plate.path in paths:
            errors.append(f"duplicate plate path: {plate.path}")
        paths.add(plate.path)
        if plate.station not in known_stations:
            errors.append(f"{plate.id}: unknown station {plate.station!r}")
        if not plate.path.startswith("images/"):
            errors.append(f"{plate.id}: path must live under images/")
        if not plate.path.endswith(".png"):
            errors.append(f"{plate.id}: application captures must be PNG")
        if not plate.title.strip() or not plate.alt.strip() or not plate.note.strip():
            errors.append(f"{plate.id}: title, alt and note are required")
        if len(plate.note) > PLATE_NOTE_LIMIT:
            errors.append(f"{plate.id}: note is {len(plate.note)} characters, limit is {PLATE_NOTE_LIMIT}")
        if plate.kind not in SCENE_KEYS:
            errors.append(f"{plate.id}: unknown scene kind {plate.kind!r}")
            continue
        unknown = set(plate.setup) - SCENE_KEYS[plate.kind]
        missing = REQUIRED_KEYS[plate.kind] - set(plate.setup)
        if unknown:
            errors.append(f"{plate.id}: {plate.kind} does not accept {sorted(unknown)}")
        if missing:
            errors.append(f"{plate.id}: {plate.kind} requires {sorted(missing)}")
        viewport = plate.setup.get("viewport")
        if viewport not in VIEWPORTS:
            errors.append(f"{plate.id}: unknown viewport {viewport!r}")
        dim = plate.setup.get("dim")
        if dim is not None and dim not in EXPLORER_DIMENSIONS:
            errors.append(f"{plate.id}: unknown explorer dimension {dim!r}")
        section = plate.setup.get("section")
        if section is not None and section not in CONSOLE_SECTIONS:
            errors.append(f"{plate.id}: unknown console section {section!r}")
        category = plate.setup.get("category")
        if category is not None and category not in SETTINGS_CATEGORIES:
            errors.append(f"{plate.id}: unknown settings category {category!r}")
        preset = plate.setup.get("preset")
        if preset is not None and preset not in CHART_PRESETS:
            errors.append(f"{plate.id}: unknown chart preset {preset!r}")
        group = plate.setup.get("group")
        if group is not None and group not in DRAWER_GROUPS:
            errors.append(f"{plate.id}: unknown drawer group {group!r}")
        for flag in ("composer", "full_page"):
            if flag in plate.setup and not isinstance(plate.setup[flag], bool):
                errors.append(f"{plate.id}: {flag} must be a boolean")
        if "require_fit" in plate.setup and not isinstance(plate.setup["require_fit"], str):
            errors.append(f"{plate.id}: require_fit must be a selector")
    for station in stations:
        if not station.lede.strip():
            errors.append(f"{station.id}: a station needs a lede")
        if not any(plate.station == station.id for plate in plates):
            errors.append(f"{station.id}: station has no plates")
    if errors:
        raise ValueError("invalid capture gallery registry:\n  " + "\n  ".join(errors))


@dataclass(frozen=True)
class Gallery:
    stations: tuple[Station, ...]
    plates: tuple[Plate, ...]

    def ordered(self) -> Iterator[tuple[int, Station, tuple[Plate, ...]]]:
        for station in self.stations:
            members = tuple(plate for plate in self.plates if plate.station == station.id)
            yield self.stations.index(station) + 1, station, members

    def number(self, plate: Plate) -> int:
        return self.plates.index(plate) + 1

    def plate(self, plate_id: str) -> Plate:
        for plate in self.plates:
            if plate.id == plate_id:
                return plate
        raise KeyError(plate_id)

    def by_path(self, relative: str) -> Plate | None:
        for plate in self.plates:
            if plate.path == relative:
                return plate
        return None


def load(stations: tuple[Station, ...], plates: tuple[Plate, ...]) -> Gallery:
    validate(stations, plates)
    return Gallery(stations, plates)
