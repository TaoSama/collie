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
  // nothing with at the bottom. 12 PageDowns (54 bytes total, well under
  // escChunkMax=256) cover ~400 lines in one shot.
  const CTRL_END = "\x1b[1;5F", PAGE_UP = "\x1b[5~", PAGE_DOWN = "\x1b[6~", HOME_PAGES = 12;
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
    let gen = 0;
    function pump() {
      if (busy || !owed?.lines) return;
      const { pane, cols, rows } = owed;
      const n = Math.max(-BURST, Math.min(BURST, owed.lines));
      owed.lines -= n;
      busy = true;
      const g = gen;
      Promise.resolve(send(pane, report(n, cols, rows))).finally(() => {
        if (g !== gen) return;
        busy = false;
        pump();
      });
    }
    return {
      scroll(pane, dy, cols, rows) {
        if (owed?.pane !== pane) owed = { pane, lines: 0 };
        Object.assign(owed, { lines: owed.lines + dy, cols, rows });
        pump();
      },
      // Leaving the pane: drop queued lines and detach any stalled in-flight
      // send so the next pane is never head-of-line blocked behind it.
      drop() { gen++; owed = null; busy = false; },
    };
  }
  window.collieWheel = { report, tracker, sender, BURST, PAGE_UP, PAGE_DOWN, HOME_SEQ: CTRL_END + PAGE_DOWN.repeat(HOME_PAGES) };
})();
