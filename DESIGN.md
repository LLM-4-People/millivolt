---
name: millivolt
description: A warm, dark precision instrument for reading live LLM proxy traffic at a glance.
colors:
  case: "#141210"
  panel: "#1a1714"
  panel-raised: "#221e19"
  rail: "#100e0b"
  hairline: "#2b2620"
  hairline-strong: "#3a342b"
  ink: "#e8e4de"
  ink-muted: "#97907e"
  ink-cancel: "#a8a096"
  signal: "#7aa0ff"
  signal-hover: "#9ab8ff"
  violet: "#b18cff"
  trace: "#38d5c0"
  ok: "#2fd186"
  warn: "#f4c14d"
  err: "#ff7b7b"
  err-ink: "#ff8a80"
  warn-ink: "#e3b341"
  throttle: "#ff9346"
  danger-hover: "#ff8a8a"
  scale-tick: "#7a72a0"
  scale-base: "#56507a"
  clear-case: "#f7f2e8"
  clear-panel: "#fffdf7"
  clear-ink: "#2a241a"
typography:
  readout:
    fontFamily: 'ui-monospace, "SFMono-Regular", "JetBrains Mono", Menlo, monospace'
    fontSize: "21px"
    fontWeight: 650
    lineHeight: 1.05
    letterSpacing: "-0.02em"
    fontFeature: "tnum"
  brand:
    fontFamily: 'ui-monospace, "SFMono-Regular", "JetBrains Mono", Menlo, monospace'
    fontSize: "15px"
    fontWeight: 650
    lineHeight: 1.5
    letterSpacing: "-0.01em"
  heading:
    fontFamily: '-apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif'
    fontSize: "13px"
    fontWeight: 650
    lineHeight: 1.3
  body:
    fontFamily: '-apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif'
    fontSize: "14px"
    fontWeight: 400
    lineHeight: 1.5
  data:
    fontFamily: 'ui-monospace, "SFMono-Regular", "JetBrains Mono", Menlo, monospace'
    fontSize: "11.5px"
    fontWeight: 400
    lineHeight: 1.5
  control:
    fontFamily: 'ui-monospace, "SFMono-Regular", "JetBrains Mono", Menlo, monospace'
    fontSize: "11px"
    fontWeight: 400
    letterSpacing: "normal"
  label:
    fontFamily: 'ui-monospace, "SFMono-Regular", "JetBrains Mono", Menlo, monospace'
    fontSize: "9px"
    fontWeight: 600
    lineHeight: 1.5
    letterSpacing: "0.12em"
rounded:
  micro: "4px"
  select: "6px"
  row: "7px"
  control: "8px"
  panel: "10px"
  node: "11px"
  sheet: "12px"
  card: "14px"
components:
  button:
    backgroundColor: "{colors.panel-raised}"
    textColor: "{colors.ink-muted}"
    typography: "{typography.control}"
    rounded: "{rounded.control}"
    padding: "5px 12px"
  button-hover:
    backgroundColor: "{colors.panel-raised}"
    textColor: "{colors.ink}"
  button-accent:
    backgroundColor: "{colors.signal}"
    textColor: "{colors.case}"
    typography: "{typography.control}"
    rounded: "{rounded.control}"
    padding: "5px 12px"
  button-danger-solid:
    backgroundColor: "{colors.err}"
    textColor: "{colors.case}"
    typography: "{typography.control}"
    rounded: "{rounded.control}"
    padding: "5px 12px"
  chart-trigger:
    backgroundColor: "{colors.panel-raised}"
    textColor: "{colors.ink-muted}"
    typography: "{typography.control}"
    rounded: "{rounded.row}"
    padding: "4px 10px"
    height: "24px"
  menu-panel:
    backgroundColor: "{colors.panel}"
    textColor: "{colors.ink}"
    typography: "{typography.control}"
    rounded: "{rounded.panel}"
    padding: "10px 12px"
    width: "min(340px, calc(100vw - 16px))"
  menu-chip:
    backgroundColor: "{colors.panel-raised}"
    textColor: "{colors.ink-muted}"
    rounded: "{rounded.row}"
    padding: "0 10px"
    height: "26px"
  kpi-band:
    backgroundColor: "{colors.panel}"
    textColor: "{colors.ink}"
    typography: "{typography.readout}"
    rounded: "{rounded.control}"
    padding: "0"
  request-status:
    backgroundColor: "{colors.panel-raised}"
    textColor: "{colors.ink}"
    rounded: "{rounded.select}"
    padding: "3px 8px"
  settings-sheet:
    backgroundColor: "{colors.panel}"
    textColor: "{colors.ink}"
    typography: "{typography.data}"
    rounded: "{rounded.card}"
    width: "min(1080px, calc(100vw - 24px))"
  card:
    backgroundColor: "{colors.panel}"
    textColor: "{colors.ink}"
    rounded: "{rounded.card}"
    padding: "16px 18px"
---

# Design System: millivolt

## Overview

**Creative North Star: "One instrument, one ruler"**

The dashboard is a precision measurement instrument, not a content site. It borrows the physical language of a bench instrument: a warm near-black case, hairline seams, engraved mono labels, one blue indicator lamp, and a graduated scale printed along the edges. It is dense because the operator reads it at a glance, not because density is a style.

One instrument, one ruler. The menu-like surfaces share one box recipe and, in most cases, one anatomy: a titled head, a grouped body and an optional action foot; the chart, timeframe and provider pickers also share one open and close lifecycle. Time is a ruler drawn over the stored history rather than a dropdown of abstract windows. The world extends the incumbent dashboard: the same warm panels, mono readouts, blue chrome ink and the 1px/7px tick rail. Nothing here introduces a second visual world.

Depth is tone first. Panels are flat, separated by a hairline and a very wide, low-alpha ambient shadow; the stronger lifts belong to menus, sheets and the drawer. There are no decorative gradient backdrops, no glow blobs, no hard offset blocks and no deck of rounded cards.

**Key Characteristics:**
- Warm near-black tonal surfaces, hue about 70 degrees, very low chroma.
- One blue chrome accent; violet, teal and status hues speak only as data or state.
- Mono for every number, status and menu row; sans only for prose, setting names, titles and narrow header action labels.
- Hairline borders and the graduated 1px/7px tick rail as the recurring motif.
- 44px touch targets on the shared controls (menu rows and chips, triggers, the status filter, provider fields, sheet actions) wherever the pointer is coarse or the viewport is narrow.
- The one-screen side-by-side lock exists only at 1200px by 750px and larger.

## Colors

The palette is a warm instrument case, deliberately the inverse of the default blue-slate dashboard: near-black with a warm hue and almost no chroma, so the cool accent pair and the status hues read as lit indicators.

### Primary
- **Signal Blue** (#7aa0ff): the single chrome ink. It marks focus edges, hover borders, selected checks, selection edges, the live pulse and one filled commit action for the decision at hand. **Signal Blue Hover** (#9ab8ff) lightens accent-filled controls one step on hover.

### Secondary
- **Violet Readout** (#b18cff): the second data voice. It colors tokens in across charts and totals, the cache metric's KPI tick, the throttled state, the method badge and theme preview outlines. It never replaces Signal Blue as the primary-action ink.

### Tertiary
- **Teal Trace** (#38d5c0): the third data voice, used for reasoning tokens, the debug pill and the debug-action pressed state. It is a measurement hue, not a decorative accent.

### Neutral
- **Instrument Case** (#141210): the page ground beneath every panel.
- **Case Panel** (#1a1714): cards, menus, the KPI band, the settings sheet and the drawer.
- **Raised Bay** (#221e19): buttons, chips, field fills and the second tonal step inside a panel.
- **Rail Shadow** (#100e0b): the header and footer instrument rails, one step darker than the panel.
- **Hairline Warm** (#2b2620): every default 1px divider and border.
- **Hairline Strong** (#3a342b): menu outlines, panel edges, active borders and field strokes.
- **Instrument Ink** (#e8e4de): primary text and values.
- **Faded Engraving** (#97907e): labels, units, secondary facts and muted copy.
- **Cancelled Ash** (#a8a096): the client-cancelled outcome, distinct from muted copy and from status ink.

### Status
- **Go Green** (#2fd186): healthy, live, done and success confirms.
- **Amber Flag** (#f4c14d): operator pauses, warnings, the cost series and the cost KPI tick.
- **Fault Coral** (#ff7b7b): errors, failed steps and destructive fills.
- **Fault Ink** (#ff8a80): error copy on tinted or bordered surfaces where the fill is too dim.
- **Amber Ink** (#e3b341): warning copy on tinted or bordered surfaces.
- **Throttle Orange** (#ff9346): rate-limited requests, distinct from errors and from warning copy.
- **Danger Coral** (#ff8a8a): the hover step for a solid danger fill.

### Brand
- **Scale Tick Violet** (#7a72a0) and **Scale Base Violet** (#56507a): the brand mark's graduated scale and baseline. The favicon mirrors them because a standalone SVG cannot resolve `var()`.

### Light Surface
- **Clear Case** (#f7f2e8), **Clear Panel** (#fffdf7) and **Clear Ink** (#2a241a): the light family's ground, panel and text. The theme system owns the rest of the light palette.

### Palette Variants
The shipped shell carries eight palettes (Instrument, Slate, Midnight, Nord, Forest, Rose, Ember, Contrast), each in Black and Clear. Every palette block owns six surface roles, four accent roles and two material roles (shadow and veil hue) for both modes; the mode block owns text, status, scrim alpha and brand neutrals. Instrument Black is the canonical family, mirrored between the `:root` block and `themes.css`. Contrast is the maximum-separation pair.

### Alpha Ladder
Shared strengths keep tints from drifting surface by surface: status pills rest at 13% tint and 16% once they flag an operator-visible state; hover halos sit at 14%; a selected palette row tints at 6%; the tick strip washes at 7% and its major tick at 11%; scrims sit at 35% for menu sheets and 72/60/55% for dialogs, storms and the drawer.

### Named Rules
**The Chrome-Not-Paint Rule.** Signal blue is chrome ink: hairlines, focus edges, checks, selection edges and one filled commit action for the decision at hand. Ambient blue stays a tint, 6% on a selected palette row, 10% on a pressed chip and 15% on the live pill, so a saturated blue fill always means commit.

## Typography

**Display Font:** system mono (`ui-monospace, "SFMono-Regular", "JetBrains Mono", Menlo, monospace`)
**Body Font:** system sans (`-apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif`)
**Label/Mono Font:** the same system mono

**Character:** The mono is the instrument's voice and the sans is the technician's marginal note. Measured facts never leave the mono; explanation never enters it.

### Hierarchy
- **Display** (650, 21px, line-height 1.05, letter-spacing -0.02em): KPI values and any single headline number. Dual-value tiles step down to 17px so both halves fit.
- **Headline** (650, 15px, line-height 1.5, letter-spacing -0.01em): the header wordmark only.
- **Title** (650, 13px, line-height 1.3): settings pane titles in sans, and the settings sheet title in mono at 13px.
- **Body** (400, 14px, line-height 1.5): help text, descriptions and dialog prose.
- **Data** (400, 11.5px base, 11px in the drawer and menus, line-height 1.5): table cells, drawer body, menu rows and readout bands, always with tabular numerals.
- **Control** (400, 11px): buttons, triggers and menu rows.
- **Label** (600, 9px, letter-spacing 0.12em): KPI labels; section and column labels run 9px to 10px at 0.05em to 0.08em.

### Named Rules
**The Readout Is Mono Rule.** Every measured value, identifier, status word, table cell and menu row is mono with tabular numerals. Sans is reserved for explanatory prose, help text, setting names, titles, sheet prose and the narrow header action labels.

## Layout

The shell runs full bleed with no maximum width. A sticky 56px rail (`--rail-h`, plus the top safe-area inset) holds the brand, the status subtitle and 32px square action buttons with 28px inline padding; an 8px feathered tick strip (1px ticks on a 7px pitch) hangs below it. The footer mirrors the rail above its top edge as an 11px mono status line. `<main>` sits at 20px by 28px and lets extra width land on the right of the two-column layouts.

The KPI band is one continuous hairline panel, not a deck: auto-fit cells with a 140px minimum, 1px left dividers, a 2px leading tick per metric and a 6px graduated tick baseline along the bottom. Cells read label, value, secondary fact, and optionally a 4px in-flight composition rail. On a phone the band becomes two columns; at 440px and below an odd last tile takes the full row.

The explorer is a fixed 216px dimension rail beside a flexible gallery (16px gap). Gallery cards use a 15rem minimum track, a 10px gap and whole-row proximity snapping; the rail collapses at 900px into a `by <dimension>` trigger that drops its list as a panel. Below the explorer, the timeline card and request log stack in one column with a 16px gap in every scrolling layout.

Only a viewport at least 1200px wide and 750px tall locks the whole page to the screen: the body fixes to 100vh, the explorer caps at a 220px band, the pair splits `minmax(572px, 1fr)` for the timeline and `minmax(0, 2fr)` for the log, the plot floors at 120px and the log fills the rest. Shorter or narrower windows keep natural heights and scroll; no window ever crams or crops a section.

Responsive steps:
- 1200px by 750px: enter the locked one-screen instrument.
- 900px: explorer rail becomes the dimension picker.
- 800px: header actions become a full-width flyout under the rail; chart and provider menus become bottom sheets while the dimension picker stays a dropdown under its trigger; menu rows and chips, triggers, the status filter, provider fields and sheet actions reach 44px.
- 720px: settings sheet goes single column, drawer goes full width.
- 640px: phone padding (14px by 10px), 12px card padding, two-column KPI band, footer wraps.
- 480px: chart totals widen their floor and wrap.
- 440px: an odd final KPI tile spans the row.
- 360px: the theme caption bar stacks.

Touch follows `--tap` (44px) for menu rows and chips, triggers, the status filter, provider fields and sheet actions at 800px and below or on any coarse pointer; small utility affordances (copy, collapse, drawer navigation) keep their 32px to 38px boxes. Dense 24px to 28px controls remain on a fine pointer at desktop width.

## Elevation & Depth

Depth is tonal. A surface steps up by fill (`case` to `panel` to `panel-raised`), gains a 1px hairline, and only then receives a wide, low-alpha ambient shadow. Nothing uses a hard offset block or a colored glow as structure; the strongest lifts belong to menus, sheets and the drawer, and every one is still a soft wash rather than a hard edge.

### Shadow Vocabulary
- **Card** (`0 1px 0 color-mix(in srgb, var(--text) 3%, transparent) inset, 0 12px 28px rgba(var(--shadow-rgb), .24)`): every content card, with a faint top highlight.
- **Rail** (`0 10px 24px rgba(var(--shadow-rgb), .30)` header, `0 -10px 24px rgba(var(--shadow-rgb), .28)` footer): keeps scrolling content from bleeding into the instrument rails.
- **Menu and sheet** (`0 12px 32px rgba(var(--shadow-rgb), .5)`): the shared lift for anchored panels and sheets.
- **Drawer** (`-24px 0 60px rgba(var(--shadow-rgb), .5)`): the side slide-over only.
- **Settings sheet** (`0 24px 64px rgba(var(--shadow-rgb), .55)` plus a 4% inset highlight): the modal instrument sheet.
- **Hover lift** (`0 8px 20px rgba(var(--shadow-rgb), .26)`): an explorer card under the pointer, paired with a 1px rise.
- **Toast** (`0 10px 30px rgba(var(--shadow-rgb), .45)`): the polite copy confirmation.
- **Tooltip** (`0 6px 20px rgba(var(--shadow-rgb), .3)`): the chart hover readout and the client hover card.

Light mode retunes the same shadows to a warm, lower-alpha family so they do not read as soot. Keyboard focus uses a 1px accent outline plus a 3px halo at 12% accent; pressed and hover states strengthen the halo to 14%.

### Named Rules
**The Soft-Lift Rule.** Depth never comes from a hard offset block. Surfaces separate by tone and a hairline first, and any shadow is a wide, low-alpha ambient wash.

## Shapes

The form language is rectangular and precise, with small radii that step with the size of the object and never turn a panel into a capsule. Borders are 1px; the only 2px edges are functional (KPI leading ticks, rail active edges, alert tops, active tab underlines, the storm banner).

- Micro 4px: status pills, day chips, method badges.
- 5px: copy buttons and section toggles.
- 6px: native selects, theme mode controls, small chip actions.
- 7px: menu rows, chips, chart triggers, rail rows, drawer tools.
- 8px: buttons, fields, the KPI band, the dimension trigger.
- 9px: brand mark, theme panes and rule cards.
- 10px: menus, header flyouts, the hero and inspector section cards.
- 11px: explorer node cards.
- 12px: sheet-placement menus, provider sections and the phone settings sheet.
- 14px: cards and the settings sheet.

The signature silhouette is the graduated scale: a 1px minor tick every 7px and a 1px major tick every 35px, masked to feather at the edge it belongs to. It appears below the header, above the footer, along the KPI baseline, inside the timeframe coverage rail and on request-log day rules. It is a ruler, not a rule; most wearing sites feather it in their own direction.

## Components

### Action Button
Quiet machine controls: small mono labels, a raised panel fill, a hairline border and one accent step on hover.

- **Shape:** softly squared (8px), 5px by 12px padding, 11px mono.
- **Base:** raised fill (`panel-raised`), hairline border (`hairline-strong`), muted ink; hover moves ink to primary and the border to Signal Blue with a 3px halo.
- **Accent:** Signal Blue fill with case-colored ink and 600 weight; hover uses Signal Blue Hover. Reserved for commit actions such as Apply and Restart now.
- **Danger:** three weights, a 16% tint fill, a 7% outline and a solid Fault Coral fill. The most destructive action on a surface wears the solid fill.
- **Focus / Disabled:** 1px accent outline at 1px offset; disabled drops to 45% opacity with the neutral skin.

### Menu Panel
The shared panel language the chart and timeframe controls use, and the box recipe the converted menus follow: one titled head, one grouped body and an optional action foot. Placement is JS-owned; the box is the same whether it is anchored or a bottom sheet.

- **Shape:** 10px radius (12px as a sheet), 1px `hairline-strong` border, Case Panel fill, menu shadow. Width defaults to 340px, clamped to `calc(100vw - 16px)`.
- **Head:** 11px/600 mono title with an optional 10px muted subline, 10px top and 12px side padding.
- **Body:** 10px top and bottom, 12px side padding, 10px gap, thin scrollbar, overscroll contained.
- **Rows:** 28px minimum height (44px on coarse pointers or at 800px and below), 7px radius. Selected rows carry an accent check and full ink; unselected rows mute; hover is a 5% text wash.
- **Chips:** 26px high at a 10.5px mono size, 7px radius, raised fill; pressed takes an accent border and a 10% accent tint. Chips grow to 38px at 800px and 44px on coarse pointers.
- **Foot:** a 1px top hairline and space-between actions; only surfaces with a commit step use it.
- **Open:** fades in from 4px above over 160ms ease-out. As a sheet it sits 8px from the window edges over a 35% scrim.

### Timeframe Control
Time is a ruler over real stored history. The panel draws the coverage rail, the zoom ladder and the calendar presets, and it disables any window wider than the data.

- **Shape:** a 320px menu panel opened from the chart-head clock trigger, anchored to the end of the trigger or dropped as a sheet under 800px.
- **Coverage rail:** 6px high, 3px radius, hairline border, the tick strip as its ground; a 45% Signal Blue fill spans the selection from the earliest stored request to now.
- **Note and ceiling:** a 10px muted note names the two ends (`4.37m of history · since 00:18`); when any window is gated, a 10px ceiling line states `disabled windows need more stored history` once, in text.
- **Groups:** `zoom` holds 1m through 1y plus all; `calendar` holds today, yesterday, this week and this month. Disabled chips sit at 45% and explain the limit in their title.
- **Behavior:** one pick repaints the rail, closes the panel and returns focus to the trigger. The trigger label reads `last 1h`, `all time` or the calendar name.

### Chart-Head Trigger
The card-head control that names the current metric, window or percentile without opening a native select.

- **Shape:** 24px high on a fine pointer, 7px radius, 4px by 10px padding, 11px mono, raised fill and hairline border.
- **Content:** label plus an optional 9px glyph (the window trigger carries a clock); the metrics trigger carries a tabular count chip that hides at 480px.
- **States:** muted ink at rest; hover and expanded move to primary ink and an accent border. Focus is a 2px accent outline at 2px offset.
- **Touch:** 44px high with 12px inline padding at 800px and below or on coarse pointers.

### KPI Tile
An instrument readout cell, not a card: no radius, no shadow, no panel of its own.

- **Band:** one hairline panel (8px radius) with auto-fit cells and 1px dividers; the first cell has no left border.
- **Cell:** 12px 15px 14px 17px padding; a 2px leading tick at 55% opacity carries the metric voice (violet for cache, amber for cost, coral for errors, hairline for the rest).
- **Type:** 9px/600 label at 0.12em, 21px/650 tabular value, 10px muted secondary line. Dual values step to 17px with an 11px unit.
- **Composition rail:** the in-flight tile adds a 4px split rail (Signal Blue sending, violet throttled, amber paused, muted pending).
- **Baseline:** a 6px feathered tick strip closes the band.

### Status Pill
The request outcome in one compact token, read by color and word together.

- **Shape:** 1px by 7px padding, 4px radius, 10px/600 mono.
- **Tint:** 13% of the status hue behind its own ink; paused, throttled and debug pills strengthen to 16% and become clickable; the live pill pulses between full and 55% opacity over 1.6s.
- **Set:** ok, err, warn, cancel (neutral ash), live, paused, throttled, debug. A bare 429 is throttling, not an error.

### Request Status Select
The one deliberate native control: the operating system picker is the accessible touch affordance for a long filter vocabulary.

- **Shape:** native select, 6px radius, 10px type in the card head (`all status`), raised fill and a `hairline-strong` border.
- **States:** hover and focus move the border to Signal Blue. On touch it takes the full 44px target.
- **Grouping:** an `all status` row, then `live states` and `http status` optgroups; a persisted value outside those groups keeps an ungrouped option so the control never loses its value.
- **Recorded exception:** the closed control is the designed surface; the native popup itself is not styled or replaced.

### Provider Picker
The settings sheet's add-provider menu: the shared panel recipe at a narrower width, with an entity glyph per row and a free-label field.

- **Shape:** 10px radius, 12px padding, 10px gap, 290px preferred width clamped to the sheet.
- **Rows:** 11px mono, 6px radius, entity glyph at 11px; 44px rows on touch.
- **Custom field:** raised rail fill, 7px radius, 1px `hairline` border; the add action is a small button at the end.
- **Phone:** becomes a bottom sheet at 8px insets with the full-width custom field.

### Settings Rail and Sheet
Settings reads as an explorer topology, not a form dump: a modal sheet with a fixed category rail and a field pane.

- **Sheet:** 1080px maximum width, 14px radius, Case Panel fill, strong hairline, settings shadow; header and footer use the rail fill with a 7px tick strip.
- **Rail:** 220px column on Rail Shadow; rows share the explorer rail chrome (7px radius, 2px active left edge) with the category's entity color and a right-aligned tabular count.
- **Fields:** label in 12px/600 sans, control in 12px mono on a raised fill with 8px radius; each row is a `1fr / minmax(220px, 300px)` grid separated by a hairline.
- **States:** search hits accent the count, misses drop to 40%; disabled fields sit at 50%; the settings switch is the only capsule control (36px by 20px, 10px track radius, round knob).
- **Actions:** Revert is a ghost button, Apply is the one accent-filled commit. At 720px the rail becomes a wrapped strip and rows go single column.

## Do's and Don'ts

### Do:
- **Do** set every measured value in mono with `font-variant-numeric: tabular-nums`, and keep numerals from shifting as live data lands.
- **Do** keep Signal Blue to chrome and one filled commit action for the decision at hand; let rarity carry the meaning.
- **Do** wear the graduated tick rail at rail edges, readout bands and day rules, feathered by a directional mask.
- **Do** hold 44px targets on menu rows and chips, triggers, the status filter, provider fields and sheet actions at coarse pointers and at 800px and below, while keeping the dense 24px to 28px controls on a fine desktop pointer.
- **Do** keep the side-by-side pair for viewports at least 1200px wide and 750px tall, and stack everything below that.
- **Do** write sentence case and lowercase labels, preserve identifiers, protocol names and acronyms, and use sentence punctuation instead of em dashes.
- **Do** treat a status pill's tint as 13% of its own status hue, strengthening to 16% only when the state needs operator attention.

### Don't:
- **Don't** introduce a second accent wash, a gradient hero or a deck of rounded, shadowed cards; the band and cards are one instrument.
- **Don't** hard-code a new palette, font or shadow color; `themes.css` theme blocks own surfaces, accents and material hues for both modes.
- **Don't** restyle or replace the native request-status popup; the closed control is the designed surface.
- **Don't** nest a second scroller inside the 800px header flyout or inside a menu panel; one surface owns vertical scrolling.
- **Don't** use a hard offset shadow, a 3px decorative side tab or a colored glow blob as structure; use tone, a hairline and the soft leading tick.
- **Don't** set copy or labels below 9px, drop tabular numerals, or set prose in mono; the rule-trace annotation at 8.5px is the single exception.
- **Don't** use uppercase or letter-spaced eyebrows as decoration; labels are lowercase or sentence case and earn their tracking from function.
