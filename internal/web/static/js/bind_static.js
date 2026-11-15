// One id -> handler table for the shell's authored controls. The document used
// inline onclick/oninput/onkeydown attributes, and a document with inline
// handlers cannot claim a script-src without 'unsafe-inline'. Every handler is
// already a top-level function, so binding it here is the same call the
// attribute made.
//
// The table resolves each name at press time (a lazy call, not a captured
// reference) because the Settings modules load on first open: binding
// closeSettings directly would capture undefined. This file is the last
// deferred script, so every eager module is already defined.
//
// An entry names the event types the attribute pair used. A new shell control
// either joins this table or gets its own delegated owner; it must not go back
// to an attribute.
const STATIC_BINDINGS = {
  'skip-link': { click: e => skipToMain(e) },
  'btn-controls': { click: e => toggleOperatorConsole(e) },
  'btn-nav': { click: e => toggleNavMenu(e) },
  'btn-theme': { click: e => toggleThemeMenu(e), keydown: e => themeTriggerKey(e) },
  'theme-reset': { click: () => resetTheme() },
  'theme-close': { click: () => closeThemeMenu() },
  'btn-settings': { click: e => toggleSettings(e) },
  'btn-signout': { click: () => signOutOperator() },
  'settings-veil': { click: () => closeSettings() },
  'settings-q': { input: () => filterSettings() },
  'btn-settings-close': { click: () => closeSettings() },
  'btn-settings-revert': { click: () => revertSettings() },
  'btn-settings-apply': { click: () => applySettings() },
  'drawer-veil': { click: () => closeDrawer() },
  'drawer-prev': { click: () => drawerNav(-1) },
  'drawer-next': { click: () => drawerNav(1) },
  'drawer-copy': { click: () => drawerCopyRecord() },
  'drawer-close': { click: () => closeDrawer() },
};

for (const [id, events] of Object.entries(STATIC_BINDINGS)) {
  const el = document.getElementById(id);
  if (!el) continue;
  for (const [type, handler] of Object.entries(events)) el.addEventListener(type, handler);
}
