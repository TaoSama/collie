// An agent picks its colours for the terminal it was told it's in, and herdr
// tells it dark: Codex lays its prompt and input bars on dark grey. Drawn on
// a light skin those bars are heavy dark blocks. adapt() moves each dark
// background of a frame to a light one, as far from the skin's background
// as the original was from black, so the bars keep their order and sit in
// the page. Text, layout and every other colour stay as the agent drew them
// (minimumContrastRatio keeps the text readable on whatever is under it).
(() => {
  const DARK = 0.2; // relative luminance under which a background counts as dark
  const MAX_TINT = 0.25;
  const hex = (h) => {
    const m = /^#?([0-9a-f]{2})([0-9a-f]{2})([0-9a-f]{2})$/i.exec((h || "").trim());
    return m ? m.slice(1).map((x) => parseInt(x, 16)) : null;
  };
  const lum = ([r, g, b]) => {
    const c = (v) => { v /= 255; return v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4; };
    return 0.2126 * c(r) + 0.7152 * c(g) + 0.0722 * c(b);
  };
  const mix = (a, b, t) => a.map((v, i) => Math.round(v + (b[i] - v) * t));

  // text: ANSI rows; bg, fg: the skin's terminal colours (#rrggbb).
  function adapt(text, bg, fg) {
    const B = hex(bg), F = hex(fg);
    if (!B || !F || lum(B) < 0.5 || !text.includes("48;2;")) return text;
    return text.replace(/\x1b\[([0-9;]*)m/g, (all, params) => {
      const p = params.split(";");
      let changed = false;
      for (let i = 0; i < p.length; i++) {
        if ((p[i] === "38" || p[i] === "48") && p[i + 1] === "5") { i += 2; continue; }
        if (p[i] === "38" && p[i + 1] === "2") { i += 4; continue; }
        if (p[i] !== "48" || p[i + 1] !== "2" || i + 4 >= p.length) continue;
        const c = p.slice(i + 2, i + 5).map(Number);
        const l = lum(c);
        if (l < DARK) {
          p.splice(i + 2, 3, ...mix(B, F, Math.min(MAX_TINT, 0.04 + l * 0.8)).map(String));
          changed = true;
        }
        i += 4;
      }
      return changed ? `\x1b[${p.join(";")}m` : all;
    });
  }
  window.collieTermColor = { adapt };
})();
