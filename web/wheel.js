// Mouse wheel reports for an agent drawing fullscreen (paneWheel), and how
// far this page has turned its view up, so leaving can turn it back down:
// otherwise the agent stays scrolled up and reads of its screen see old
// output under "N new messages".
(() => {
  const UP = 64, DOWN = 65; // SGR wheel buttons
  // The way back down is longer than the way up when output came
  // meanwhile, and past the bottom the reports do nothing: send plenty.
  // Claude Code moves at most about 125 lines per burst of reports however
  // many it gets, so it also gets ctrl+End, its jump to the bottom (Codex
  // and agy ignore that key).
  const HOME_SLACK = 200;
  const CTRL_END = "\x1b[1;5F";
  // dy lines, toward older output when negative, at the middle of the pane.
  const report = (dy, cols, rows) =>
    `\x1b[<${dy < 0 ? UP : DOWN};${Math.ceil((cols || 2) / 2)};${Math.ceil((rows || 2) / 2)}M`.repeat(Math.abs(dy));
  function tracker() {
    let up = null; // { pane, lines, cols, rows }
    return {
      scrolled(pane, dy, cols, rows) {
        if (up?.pane !== pane) up = { pane, lines: 0 };
        Object.assign(up, { lines: Math.max(0, up.lines - dy), cols, rows });
      },
      // What brings the view back to the bottom, once; null when it's there.
      home() {
        const u = up;
        up = null;
        return u?.lines ? { pane: u.pane, text: report(2 * u.lines + HOME_SLACK, u.cols, u.rows) + CTRL_END } : null;
      },
      forget() { up = null; },
    };
  }
  window.collieWheel = { report, tracker };
})();
