// Ordered section registry. The runner executes these module functions in
// array order, which is the suite's only ordering authority: the same
// contract index.html has for the dashboard's classic scripts.
module.exports = [
  require('./sections/render_live'),
  require('./sections/selection_drawer'),
  require('./sections/logs_menu'),
  require('./sections/model_canon'),
];
