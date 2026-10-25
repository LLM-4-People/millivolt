// Ordered section registry. The runner executes these module functions in
// array order, which is the suite's only ordering authority: the same
// contract index.html has for the dashboard's classic scripts.
module.exports = [
  require('./sections/render_live'),
  require('./sections/selection_drawer'),
  require('./sections/logs_menu'),
  require('./sections/model_canon'),
  require('./sections/menus_scroll'),
  require('./sections/archive_pages'),
  require('./sections/archive_scale'),
  require('./sections/chart_buckets'),
  require('./sections/chart_render'),
  require('./sections/chart_e2e'),
  require('./sections/restart_providers'),
  require('./sections/mcp_category'),
  require('./sections/request_overrides'),
  require('./sections/subconversations'),
  require('./sections/settings_dirty'),
  require('./sections/chart_registry'),
  require('./sections/operator_menus'),
  require('./sections/settings_search'),
  require('./sections/live_invariants'),
  require('./sections/reload_paths'),
  require('./sections/restart_bootstrap'),
  require('./sections/purge_lifecycle'),
  require('./sections/delegated_actions'),
  require('./sections/operator_unlock'),
];
