// Mouse wheel reports for an agent drawing fullscreen (paneWheel), and how
// far this page has turned its view up, so leaving can turn it back down:
// otherwise the agent stays scrolled up and reads of its screen see old
// output under "N new messages".
//
// Everything here starts with ESC, and an agent that reads an ESC with
// nothing after it yet takes it for the Esc key: Claude Code and Codex stop
// the turn they are on, and the rest of the sequence lands in the input box
// as text. A terminal passes input on in pieces (macOS holds 1 KB), so a
// long run of sequences can be cut right after an ESC; keep each send short.
(() => {
  const UP = 64, DOWN = 65; // SGR wheel buttons
  // Wheel reports in one send: about 13 bytes each.
  const BURST = 8;
  // Back to the bottom: ctrl+End is Claude Code's and Codex's jump there;
  // agy ignores it and goes down with PageDown, which the other two do
  // nothing with at the bottom. A few, for a long way up.
  const CTRL_END = "\x1b[1;5F", PAGE_DOWN = "\x1b[6~", HOME_PAGES = 4;
  // dy lines, toward older output when negative, at the middle of the pane.
  const report = (dy, cols, rows) =>
    `\x1b[<${dy < 0 ? UP : DOWN};${Math.ceil((cols || 2) / 2)};${Math.ceil((rows || 2) / 2)}M`.repeat(Math.abs(dy));
  function tracker() {
    let up = null; // { pane, lines }
    return {
      scrolled(pane, dy) {
        if (up?.pane !== pane) up = { pane, lines: 0 };
        up.lines = Math.max(0, up.lines - dy);
      },
      // What brings the view back to the bottom, once; null when it's there.
      home() {
        const u = up;
        up = null;
        return u?.lines ? { pane: u.pane, text: CTRL_END + PAGE_DOWN.repeat(HOME_PAGES) } : null;
      },
      forget() { up = null; },
    };
  }
  // Lines to scroll go out BURST at a time, one send in flight: a fast
  // flick, or a slow link, adds lines to the next send instead of growing
  // one long string. send(pane, text) returns a promise.
  function sender(send) {
    let owed = null; // { pane, lines, cols, rows }
    let busy = false;
    function pump() {
      if (busy || !owed?.lines) return;
      const { pane, cols, rows } = owed;
      const n = Math.max(-BURST, Math.min(BURST, owed.lines));
      owed.lines -= n;
      busy = true;
      Promise.resolve(send(pane, report(n, cols, rows))).finally(() => { busy = false; pump(); });
    }
    return {
      scroll(pane, dy, cols, rows) {
        if (owed?.pane !== pane) owed = { pane, lines: 0 };
        Object.assign(owed, { lines: owed.lines + dy, cols, rows });
        pump();
      },
      // Leaving the pane: what hasn't gone out stays here.
      drop() { owed = null; },
    };
  }
  window.collieWheel = { report, tracker, sender, BURST };
})();
