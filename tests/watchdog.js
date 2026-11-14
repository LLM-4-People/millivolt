// watchdog fails a Node suite that never reaches its verdict.
//
// Every entry point package.json runs executes its body inside an async
// function whose only exits are the verdict and a catch. If an awaited promise
// never settles - a stubbed fetch that is never called, a jsdom event that never
// fires, a sandbox promise that never resolves - the event loop drains, Node
// exits 0, and `npm test` chains on as if the suite passed. That is the one
// failure a green log cannot show, so it gets an explicit timer.
//
// The owner is this module rather than the jsdom harness, because two of the
// four suites (the service-worker sandbox and the merge benchmark) never load
// jsdom at all. Keeping the timer beside the harness meant those two were
// silently unarmed.
//
// The timer is refed, not unrefed: an unrefed timer let the process exit 0
// before it could fire, which is the failure this exists to catch. Each caller
// clears it on its own exit path, so a normal run still ends immediately.
function watchdog(ms, label) {
  const timer = setTimeout(() => {
    console.error('watchdog: ' + label + ' did not finish within ' + Math.round(ms / 1000) + 's');
    process.exit(1);
  }, ms);
  return () => clearTimeout(timer);
}

// WATCHDOG_MS is the shared ceiling for every suite. It is far beyond any
// healthy run (the slowest is well under two minutes) and still ends a wedged
// one, so a single number is enough and no suite needs its own guess.
const WATCHDOG_MS = 60 * 60 * 1000;

module.exports = {watchdog, WATCHDOG_MS};
