// The shared menu foundation: one lifecycle for the dashboard's menu-like
// surfaces. Conversions (the header flyouts, the summary metrics picker, the
// explorer dimension rail, the timeframe control) adopt this module instead
// of hand-rolling open, close, placement, roving focus and dismissal per
// surface. The module is side-effect free at load: every global here is a
// declaration, and all DOM work happens inside the exported API.
//
// Placement: menuPlacement is the one owner of the anchor-vs-sheet decision.
// Anchor panels are position: fixed and anchored under the trigger (left- or
// right-aligned per opts.align, clamped to the viewport, flipping above the
// trigger near the window's bottom edge, with the max-height clamped to the
// space on the chosen side); opts.container measures and clamps against a
// modal surface instead, for a panel whose containing block is that ancestor.
// Sheet panels hand their geometry to CSS through [data-placement="sheet"] and
// dim the page behind the shared .mn-scrim (opts.scrim:false for a menu that
// already lives on a modal surface). The open panel re-anchors on resize and
// on any scroll through one throttled frame, with the listener teardown owned
// here rather than re-armed per open; the same sync flips the placement when
// the viewport crosses the breakpoint.
//
// The shared class vocabulary (dashboard.css owns the styling, conversions
// emit the markup): .mn (panel), .mn-on (open), .mn-scrim (sheet scrim),
// .mn-hd, .mn-title, .mn-sub, .mn-body, .mn-sec, .mn-sec-h, .mn-item,
// .mn-check, .mn-chips, .mn-chip, .mn-note, .mn-ft, .mn-actions, .mn-rail
// and .mn-rail-fill. choiceMenu emits the panel, head, body, section, row and
// check classes; the remaining blocks are what conversion bodies compose.

const MENU_SHEET_QUERY = '(max-width: 800px)';
const MENU_SHEET_PX = 800;
const MENU_GAP_PX = 4;
const MENU_EDGE_PX = 8;
const MENU_ITEM_SELECTOR = '[role^="menuitem"], .mn-item';

// One open menu at a time. This is the module's complete open state; a close
// clears it before onClose runs, so a close callback may safely open the next
// menu. menuSyncRaf is the one pending reposition frame for the open panel.
let menuOpenState = null;
let menuScrimEl = null;
let menuSyncRaf = 0;
// The active roving selector per panel; panels are shared across opens, so
// the keydown listener is installed once and reads its selector from here.
const _menuRovingSelectors = new WeakMap();

// menuPlacement is the single owner of the narrow-viewport decision: 'sheet'
// on a narrow viewport, 'anchor' otherwise. matchMedia is authoritative when
// the platform provides it; the innerWidth comparison is the fallback.
function menuPlacement() {
  if (typeof window.matchMedia === 'function') {
    return window.matchMedia(MENU_SHEET_QUERY).matches ? 'sheet' : 'anchor';
  }
  return window.innerWidth <= MENU_SHEET_PX ? 'sheet' : 'anchor';
}

// menuOpen opens a panel on a trigger. It closes the currently open menu
// first (one open menu at a time), clears the panel's geometry for the
// placement menuPlacement chooses, and owns the dismissal and re-anchor
// listeners until menuClose. opts:
//   align: 'start' | 'end' (default 'end') - anchor alignment to the trigger
//   onClose: called exactly once on every close, after the state clears
//   roving: enable arrow/Home/End/Enter/Space roving over MENU_ITEM_SELECTOR
//   initialFocus: 'item' | 'panel' | 'none' (default 'item' when roving,
//     else 'panel')
//   container: an optional positioned ancestor whose padding box is the
//     panel's containing block; anchor placement is then measured and clamped
//     inside that surface instead of the viewport (a menu inside a modal)
//   scrim: false suppresses the sheet scrim for a menu that already lives on
//     a modal surface (default true)
function menuOpen(trigger, panel, opts = {}) {
  if (!trigger || !panel) return false;
  if (menuOpenState) menuClose(menuOpenState.panel);
  const roving = !!opts.roving;
  const state = {
    panel,
    trigger,
    // The close focus home: Escape restores focus here when focus was inside
    // the panel or on the trigger.
    restoreTarget: trigger,
    opts: {
      align: opts.align === 'start' ? 'start' : 'end',
      onClose: typeof opts.onClose === 'function' ? opts.onClose : null,
      roving,
      initialFocus: opts.initialFocus || (roving ? 'item' : 'panel'),
      container: opts.container && typeof opts.container.getBoundingClientRect === 'function' ? opts.container : null,
      scrim: opts.scrim !== false,
    },
  };
  menuOpenState = state;
  panel.hidden = false;
  panel.classList.add('mn-on');
  panel.dataset.placement = menuPlacement();
  menuSetExpanded(trigger, true);
  if (roving) menuRoving(panel, MENU_ITEM_SELECTOR);
  menuPlace(state);
  // Capture phase: an open menu owns Escape. Bubbling document handlers
  // (core.js's settings/dim/header order) must never close the surface the
  // menu was opened from before the menu itself gets the key.
  document.addEventListener('keydown', menuKeydown, true);
  document.addEventListener('pointerdown', menuPointerdown, true);
  window.addEventListener('resize', menuScheduleSync);
  window.addEventListener('scroll', menuScheduleSync, true);
  menuFocusInitial(state);
  return true;
}

// menuClose closes the named panel if it is the open one. The state clears
// first, so the onClose callback can never observe a half-closed menu.
function menuClose(panel) {
  const state = menuOpenState;
  if (!state || state.panel !== panel) return false;
  menuOpenState = null;
  document.removeEventListener('keydown', menuKeydown, true);
  document.removeEventListener('pointerdown', menuPointerdown, true);
  window.removeEventListener('resize', menuScheduleSync);
  window.removeEventListener('scroll', menuScheduleSync, true);
  if (menuSyncRaf) {
    cancelAnimationFrame(menuSyncRaf);
    menuSyncRaf = 0;
  }
  state.panel.hidden = true;
  state.panel.classList.remove('mn-on');
  menuClearGeometry(state.panel);
  delete state.panel.dataset.placement;
  menuSetExpanded(state.trigger, false);
  menuShowScrim(false);
  // The one focus rule: closing while focus is inside the panel hands it back
  // to the trigger. A close with focus elsewhere (an outside press) leaves
  // that focus alone.
  const restore = state.panel.contains(document.activeElement);
  if (restore && state.restoreTarget && state.restoreTarget.isConnected) {
    state.restoreTarget.focus();
  }
  if (state.opts.onClose) state.opts.onClose();
  return true;
}

function menuToggle(trigger, panel, opts) {
  if (!trigger || !panel) return false;
  if (menuIsOpen(panel)) {
    menuClose(panel);
    return false;
  }
  return menuOpen(trigger, panel, opts);
}

function menuIsOpen(panel) {
  return !!menuOpenState && menuOpenState.panel === panel;
}

function menuCloseAll() {
  if (menuOpenState) menuClose(menuOpenState.panel);
}

// menuRoving wires the roving keyboard cursor onto a panel. Rows are the
// focusable items matching itemSelector, skipping disabled, hidden and
// aria-disabled rows; arrows wrap, Home/End jump, and Enter/Space activate
// the focused row. Tab stops follow the theme.js roving-radio convention:
// the focused row owns the group's one tab stop.
function menuRoving(panel, itemSelector) {
  if (!panel || !itemSelector) return;
  _menuRovingSelectors.set(panel, itemSelector);
  if (panel.dataset.mnRoving === '1') return;
  panel.dataset.mnRoving = '1';
  panel.addEventListener('keydown', event => menuRovingKey(panel, event));
}

// choiceMenu builds the single-select listbox for short vocabularies:
//   spec.items  [{id, label, group?, disabled?}]
//   spec.value  the current selection id
//   spec.label  the element that displays the selection label, or null
//   spec.onPick called with the chosen id (after the internal selection moves)
//   spec.title  optional panel title (visible head plus aria-label)
//   spec.keepOpen  keep the panel open after a pick (default: close)
// The returned controller is { open(), close(), toggle(), sync(next) }; the
// trigger click is wired here. sync() accepts a partial spec (items, value,
// label, onPick, title, keepOpen): rows rebuild only when the item set
// changes, otherwise selection updates in place (aria-checked, check glyph,
// label element text). choiceMenu never invents aria-haspopup.
function choiceMenu(trigger, spec) {
  const source = spec && typeof spec === 'object' ? spec : {};
  const ui = {
    trigger: trigger || null,
    value: source.value,
    spec: {
      items: Array.isArray(source.items) ? source.items : [],
      label: source.label || null,
      onPick: typeof source.onPick === 'function' ? source.onPick : null,
      title: source.title || '',
      keepOpen: !!source.keepOpen,
    },
    panel: null,
    signature: '',
  };
  const controller = {
    open() { menuChoiceOpen(ui); return controller; },
    close() { if (ui.panel) menuClose(ui.panel); return controller; },
    toggle() { menuChoiceToggle(ui); return controller; },
    sync(next) { menuChoiceSync(ui, next); return controller; },
  };
  if (ui.trigger) {
    if (!ui.trigger.hasAttribute('aria-expanded')) ui.trigger.setAttribute('aria-expanded', 'false');
    ui.trigger.addEventListener('click', () => controller.toggle());
  }
  return controller;
}

// ---------- placement ----------
// menuPlace applies the geometry for the panel's current data-placement: the
// sheet branch clears the inline anchor geometry and shows the scrim (CSS
// owns the sheet box), the anchor branch computes anchored coordinates.
function menuPlace(state) {
  if (state.panel.dataset.placement === 'sheet') {
    menuShowScrim(!!state.opts.scrim);
    menuClearGeometry(state.panel);
    return;
  }
  menuShowScrim(false);
  menuPlaceAnchor(state);
}

// menuPlaceAnchor fixes the panel under the trigger: aligned per opts.align,
// clamped to 8px from each edge, flipped above the trigger when the space
// below cannot hold the measured panel and the space above is larger, and
// max-height clamped to the remaining space minus the 8px edge. The CSS base
// max-height is re-read on every placement (inline first removed) so a clamp
// cannot ratchet across resizes. With opts.container the same math runs
// against that surface's border box and the panel's left/top are expressed in
// the container's padding box (its absolute containing block).
function menuPlaceAnchor(state) {
  const panel = state.panel;
  // Clear a previous run's clamp before measuring: a short inline max-height
  // must not make the panel look like it fits below when it does not.
  panel.style.removeProperty('max-height');
  const container = state.opts.container;
  const base = container ? container.getBoundingClientRect() : null;
  const originX = base ? base.left + container.clientLeft : 0;
  const originY = base ? base.top + container.clientTop : 0;
  const leftEdge = base ? base.left : 0;
  const topEdge = base ? base.top : 0;
  const rightEdge = base ? base.right : window.innerWidth;
  const bottomEdge = base ? base.bottom : window.innerHeight;
  const rect = state.trigger.getBoundingClientRect();
  const own = panel.getBoundingClientRect();
  const below = bottomEdge - rect.bottom - MENU_GAP_PX;
  const above = rect.top - topEdge - MENU_GAP_PX;
  const flip = below < own.height + MENU_EDGE_PX && above > below;
  const top = flip ? rect.top - MENU_GAP_PX - own.height : rect.bottom + MENU_GAP_PX;
  const room = Math.max(0, (flip ? above : below) - MENU_EDGE_PX);
  const styled = parseFloat(getComputedStyle(panel).maxHeight);
  panel.style.maxHeight = (Number.isFinite(styled) ? Math.min(styled, room) : room) + 'px';
  let left = state.opts.align === 'start' ? rect.left : rect.right - own.width;
  if (own.width) {
    const right = Math.max(leftEdge + MENU_EDGE_PX, rightEdge - own.width - MENU_EDGE_PX);
    left = Math.min(Math.max(left, leftEdge + MENU_EDGE_PX), right);
  } else {
    left = Math.max(left, leftEdge + MENU_EDGE_PX);
  }
  panel.style.left = Math.round(left - originX) + 'px';
  panel.style.top = Math.round(top - originY) + 'px';
  panel.style.right = 'auto';
  panel.style.removeProperty('bottom');
}

function menuClearGeometry(panel) {
  for (const prop of ['left', 'right', 'top', 'bottom', 'max-height']) panel.style.removeProperty(prop);
}

// menuScheduleSync throttles re-anchoring to one frame: resize and every
// scroll (capture, so nested scroll regions count) schedule the same frame.
function menuScheduleSync() {
  if (!menuOpenState || menuSyncRaf) return;
  menuSyncRaf = requestAnimationFrame(menuSyncPlacement);
}

// menuSyncPlacement is the scheduled frame: it re-reads menuPlacement, flips
// the panel's placement when the viewport crossed the breakpoint, and
// re-applies the geometry.
function menuSyncPlacement() {
  menuSyncRaf = 0;
  const state = menuOpenState;
  if (!state) return;
  const placement = menuPlacement();
  if (state.panel.dataset.placement !== placement) state.panel.dataset.placement = placement;
  menuPlace(state);
}

// ---------- focus and dismissal ----------
function menuSetExpanded(element, open) {
  // Only a trigger that already carries aria-expanded is synced: menus never
  // invent the attribute (callers own their markup).
  if (element && element.hasAttribute('aria-expanded')) {
    element.setAttribute('aria-expanded', open ? 'true' : 'false');
  }
}

// menuFocusInitial applies the open focus: the selected row when one is
// checked (the radio-menu entry convention), else the first enabled row; the
// panel itself (tabindex added lazily); or nothing for 'none'.
function menuFocusInitial(state) {
  const mode = state.opts.initialFocus;
  if (mode === 'none') return;
  const panel = state.panel;
  if (mode === 'panel') {
    if (!panel.hasAttribute('tabindex')) panel.setAttribute('tabindex', '-1');
    panel.focus();
    return;
  }
  const rows = menuEnabledRows(panel, MENU_ITEM_SELECTOR);
  if (!rows.length) return;
  const selected = rows.find(row => row.getAttribute('aria-checked') === 'true');
  const target = selected || rows[0];
  if (state.opts.roving) rows.forEach(row => { row.tabIndex = row === target ? 0 : -1; });
  target.focus();
}

// menuKeydown closes on Escape and owns the key while a menu is open: the
// capture listener consumes it so no underlying surface (a settings sheet, a
// drawer) closes in the same press. menuClose is the one focus owner; this
// handler only routes the key and stops its further dispatch.
function menuKeydown(event) {
  if (event.key !== 'Escape') return;
  if (!menuOpenState) return;
  menuClose(menuOpenState.panel);
  event.preventDefault();
  event.stopPropagation();
}

// menuPointerdown closes on any press outside the panel and its trigger. The
// composed path is the source of truth (the chrome_menus.js dismissal
// pattern); the close deliberately steals no focus.
function menuPointerdown(event) {
  const state = menuOpenState;
  if (!state) return;
  const path = typeof event.composedPath === 'function' ? event.composedPath() : [];
  if (path.includes(state.panel) || path.includes(state.trigger)) return;
  menuClose(state.panel);
}

// menuEnabledRows are the roving rows matching selector, skipping disabled,
// hidden and aria-disabled entries in document order.
function menuEnabledRows(panel, selector) {
  return [...panel.querySelectorAll(selector)].filter(row =>
    !row.disabled && !row.hasAttribute('disabled') && !row.hidden &&
    row.getAttribute('aria-disabled') !== 'true');
}

// menuRovingKey implements the roving conventions on the focused row:
// modified arrows stay with the browser; Enter/Space activate through the
// row's own click so every surface keeps one activation path.
function menuRovingKey(panel, event) {
  if (event.altKey || event.ctrlKey || event.metaKey) return;
  const selector = _menuRovingSelectors.get(panel) || MENU_ITEM_SELECTOR;
  const rows = menuEnabledRows(panel, selector);
  const index = rows.indexOf(event.target);
  if (index < 0) return;
  if (event.key === 'Enter' || event.key === ' ') {
    event.preventDefault();
    rows[index].click();
    return;
  }
  let next = index;
  if (event.key === 'ArrowDown') next = (index + 1) % rows.length;
  else if (event.key === 'ArrowUp') next = (index - 1 + rows.length) % rows.length;
  else if (event.key === 'Home') next = 0;
  else if (event.key === 'End') next = rows.length - 1;
  else return;
  event.preventDefault();
  menuMoveRoving(rows, next);
}

// menuMoveRoving moves the roving cursor and its one tab stop (theme.js's
// moveRovingFocus convention, applied to the enabled-row list).
function menuMoveRoving(rows, next) {
  const target = rows[next];
  if (!target) return;
  rows.forEach((row, index) => { row.tabIndex = index === next ? 0 : -1; });
  target.focus();
}

// ---------- sheet scrim ----------
// menuScrim lazily creates the one scrim element. It is a plain div and can
// never take focus; pointerdowns on it fall to the outside-dismissal path.
function menuScrim() {
  if (!menuScrimEl) {
    menuScrimEl = document.createElement('div');
    menuScrimEl.className = 'mn-scrim';
    menuScrimEl.setAttribute('aria-hidden', 'true');
    menuScrimEl.hidden = true;
    document.body.appendChild(menuScrimEl);
  }
  return menuScrimEl;
}

function menuShowScrim(show) {
  if (show) menuScrim().hidden = false;
  else if (menuScrimEl) menuScrimEl.hidden = true;
}

// ---------- choiceMenu internals ----------
function menuChoiceOpen(ui) {
  if (!ui.trigger) return;
  if (!ui.panel) menuChoiceBuild(ui);
  menuOpen(ui.trigger, ui.panel, { roving: true, initialFocus: 'item' });
}

function menuChoiceToggle(ui) {
  if (ui.panel && menuIsOpen(ui.panel)) {
    menuClose(ui.panel);
    return;
  }
  menuChoiceOpen(ui);
}

function menuChoiceBuild(ui) {
  const panel = document.createElement('div');
  panel.className = 'mn mn-menu';
  panel.setAttribute('role', 'menu');
  if (ui.spec.title) panel.setAttribute('aria-label', String(ui.spec.title));
  panel.hidden = true;
  // The trigger names its panel so assistive tech can follow the expanded
  // state to the list it owns (the markup's aria-haspopup stays the caller's).
  if (ui.trigger && ui.trigger.id) {
    panel.id = ui.trigger.id + '-menu';
    ui.trigger.setAttribute('aria-controls', panel.id);
  }
  ui.panel = panel;
  document.body.appendChild(panel);
  panel.addEventListener('click', event => menuChoiceClick(ui, event));
  menuChoiceRebuild(ui);
}

// menuChoiceRebuild emits the head and body: grouped items render as one
// .mn-sec section with a .mn-sec-h heading at the group's first item position
// (list a group's items contiguously); ungrouped items are direct rows.
function menuChoiceRebuild(ui) {
  const panel = ui.panel;
  panel.textContent = '';
  if (ui.spec.title) {
    const head = document.createElement('div');
    head.className = 'mn-hd';
    const title = document.createElement('div');
    title.className = 'mn-title';
    title.textContent = String(ui.spec.title);
    head.appendChild(title);
    panel.appendChild(head);
  }
  const body = document.createElement('div');
  body.className = 'mn-body';
  panel.appendChild(body);
  const sections = new Map();
  for (const item of ui.spec.items) {
    const row = menuChoiceRow(item);
    const group = item.group == null ? '' : String(item.group);
    if (!group) {
      body.appendChild(row);
      continue;
    }
    let section = sections.get(group);
    if (!section) {
      section = document.createElement('div');
      section.className = 'mn-sec';
      const heading = document.createElement('div');
      heading.className = 'mn-sec-h';
      heading.textContent = group;
      section.appendChild(heading);
      sections.set(group, section);
      body.appendChild(section);
    }
    section.appendChild(row);
  }
  ui.signature = menuChoiceSignature(ui.spec.items);
  menuChoiceSelection(ui);
}

function menuChoiceRow(item) {
  const row = document.createElement('button');
  row.type = 'button';
  row.className = 'mn-item';
  row.setAttribute('role', 'menuitemradio');
  row.dataset.mnId = String(item.id);
  row.disabled = !!item.disabled;
  const check = document.createElement('span');
  check.className = 'mn-check';
  check.setAttribute('aria-hidden', 'true');
  const label = document.createElement('span');
  label.textContent = String(item.label);
  row.append(check, label);
  return row;
}

// menuChoiceSelection is the one in-place selection writer: aria-checked and
// the check glyph on every row, and the stored label in labelElement. A value
// outside the item set leaves the label element untouched.
function menuChoiceSelection(ui) {
  const panel = ui.panel;
  if (!panel) return;
  const items = new Map(ui.spec.items.map(item => [String(item.id), item]));
  for (const row of panel.querySelectorAll('.mn-item')) {
    const item = items.get(row.dataset.mnId);
    const selected = !!item && String(ui.value) === String(item.id);
    row.setAttribute('aria-checked', selected ? 'true' : 'false');
    const check = row.querySelector('.mn-check');
    if (check) check.textContent = selected ? '✓' : '';
  }
  if (ui.spec.label) {
    const current = items.get(String(ui.value));
    if (current) ui.spec.label.textContent = String(current.label);
  }
}

// menuChoiceSignature is the one rebuild oracle: ids, labels, groups and
// disabled flags, in order. Selection is state, not identity, so it never
// forces a rebuild.
function menuChoiceSignature(items) {
  return JSON.stringify((items || []).map(item =>
    [String(item.id), String(item.label), item.group == null ? '' : String(item.group), !!item.disabled]));
}

function menuChoiceClick(ui, event) {
  const target = event.target;
  const row = target && typeof target.closest === 'function' ? target.closest('.mn-item') : null;
  if (!row || row.disabled || row.getAttribute('aria-disabled') === 'true') return;
  menuChoicePick(ui, row.dataset.mnId);
}

// menuChoicePick moves the stored value and the visible selection first, then
// notifies the caller, then closes unless keepOpen.
function menuChoicePick(ui, id) {
  const item = ui.spec.items.find(entry => String(entry.id) === String(id));
  if (!item || item.disabled) return false;
  ui.value = item.id;
  menuChoiceSelection(ui);
  if (ui.spec.onPick) ui.spec.onPick(item.id);
  if (!ui.spec.keepOpen && ui.panel) menuClose(ui.panel);
  return true;
}

// menuChoiceSync applies a partial spec update: rows rebuild only when the
// item set changed, selection and the label element update in place, and the
// optional title follows the panel head.
function menuChoiceSync(ui, next) {
  if (!next || typeof next !== 'object') return;
  let itemsChanged = false;
  if (Array.isArray(next.items)) {
    itemsChanged = menuChoiceSignature(next.items) !== ui.signature;
    ui.spec.items = next.items;
  }
  if ('value' in next) ui.value = next.value;
  if ('label' in next) ui.spec.label = next.label || null;
  if (typeof next.onPick === 'function') ui.spec.onPick = next.onPick;
  if ('keepOpen' in next) ui.spec.keepOpen = !!next.keepOpen;
  const title = 'title' in next ? (next.title || '') : ui.spec.title;
  if (!ui.panel) {
    ui.spec.title = title;
    return;
  }
  if (itemsChanged) menuChoiceRebuild(ui);
  else menuChoiceSelection(ui);
  if (title !== ui.spec.title) menuChoiceTitle(ui, title);
}

// menuChoiceTitle keeps the panel's accessible name and its visible head
// aligned with the stored title.
function menuChoiceTitle(ui, title) {
  ui.spec.title = title;
  const panel = ui.panel;
  const head = panel.querySelector('.mn-hd');
  if (title) {
    panel.setAttribute('aria-label', String(title));
    if (head) {
      head.querySelector('.mn-title').textContent = String(title);
    } else {
      const next = document.createElement('div');
      next.className = 'mn-hd';
      const label = document.createElement('div');
      label.className = 'mn-title';
      label.textContent = String(title);
      next.appendChild(label);
      panel.insertBefore(next, panel.firstChild);
    }
  } else {
    panel.removeAttribute('aria-label');
    if (head) head.remove();
  }
}
