// watchdog fails a Node suite that never reaches its verdict.
//
// Every entry point package.json runs executes its body inside an async
// function whose only exits are the verdict and a catch. If an awaited promise
// never settles - a stubbed fetch that is never called, a jsdom event that never
// fires, a sandbox promise that never resolves - the event loop drains, Node
// exits 0, and `npm test` chains on as if the suite passed. That is the one
// failure a green log cannot show, so it gets two detections.
//
// The owner is this module rather than the jsdom harness, because two of the
// four suites (the service-worker sandbox and the merge benchmark) never load
// jsdom at all. Keeping the timer beside the harness meant those two were
// silently unarmed.
//
// A refed timer alone is both too slow and too weak. Draining is the common
// shape of this failure, and a refed timer is itself the only refed handle, so
// the process would sit for the full bound (an hour) before printing anything:
// every crash became a job timeout with no test output. So the timer is
// unrefed, which lets the loop empty, and `beforeExit` - the event Node emits
// exactly when nothing is left to run - reports the drain immediately. The
// timer still fires when a wedged suite holds another live handle (an interval,
// an open socket), because an unrefed timer runs while something else keeps the
// loop alive.
//
// Both paths report the same prefix and name which one fired. A suite that
// reached its verdict calls the returned clear, which retires both.
//
// A drain is only this module's business when it would otherwise read as
// success. The verdict is explicit: `clear()` means the suite finished, and a
// nonzero `process.exitCode` means it failed loudly and is allowed to exit -
// reporting over it would bury its message. `exitCode === 0` is also a
// verdict, because a suite may record success that way; treating the falsy zero
// as "no verdict" reported a passing suite as a failure.
function watchdog(ms, label) {
  let settled = false;
  const fail = reason => {
    if (settled) return;
    settled = true;
    console.error('watchdog: ' + label + ' did not finish (' + reason + ')');
    process.exit(1);
  };
  const timer = setTimeout(
    () => fail('still running after ' + Math.round(ms / 1000) + 's'), ms);
  timer.unref();
  const onDrain = () => {
    if (process.exitCode !== undefined) return;
    fail('the event loop emptied with no verdict; a suite that finished must '
      + 'call the clear function it was given');
  };
  process.once('beforeExit', onDrain);
  return () => {
    settled = true;
    clearTimeout(timer);
    process.removeListener('beforeExit', onDrain);
  };
}

// WATCHDOG_MS is the shared ceiling for every suite. It is far beyond any
// healthy run (the slowest is well under two minutes) and still ends a wedged
// one, so a single number is enough and no suite needs its own guess.
const WATCHDOG_MS = 60 * 60 * 1000;

module.exports = {watchdog, WATCHDOG_MS};
