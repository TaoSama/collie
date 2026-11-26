#!/bin/sh
# Builds the browser-only demo into docs/demo/: the real web app plus
# scripts/demo/demo.js, which answers its API with made-up data.
#
#   sh scripts/demo.sh
set -eu
cd "$(dirname "$0")/.."
out=docs/demo
rm -rf "$out" && mkdir -p "$out"
cp -R web/vendor web/fonts web/imgedit.js web/threads.js web/tasks.js web/machines.js web/ext.js web/workbench.js web/app.css web/skins.css web/icon.svg web/icon-512.png "$out/"
cp scripts/demo/demo.js scripts/demo/demo-qr.png scripts/demo/bench-pagination.html "$out/"
# Extension icons with their licenses, which the hub hands out from the manifests (extensions/*/icon.svg).
mkdir "$out/ext-icons" && for f in extensions/*/icon.svg; do
  id=$(basename "$(dirname "$f")"); cp "$f" "$out/ext-icons/$id.svg"; cp "extensions/$id/icon.LICENSE" "$out/ext-icons/$id.LICENSE"
done
# Use paths relative to the demo directory when serving it on GitHub Pages.
sed -e 's#src="/icon.svg"#src="icon.svg"#g' -e 's#"/api/qr?text=#"demo-qr.png?text=#g' web/app.js > "$out/app.js"
sed -e 's#href="/#href="#g' -e 's#src="/#src="#g' \
    -e 's#<script src="vendor/xterm.js"></script>#<script src="demo.js"></script>\n<script src="vendor/xterm.js"></script>#' \
    -e 's#<title>Collie</title>#<title>Collie 演示</title>#' \
    -e '/rel="manifest"/d' web/index.html > "$out/index.html"
cat >> "$out/app.css" <<'CSS'

/* demo only */
.demo-badge { position: fixed; right: 12px; bottom: calc(12px + env(safe-area-inset-bottom)); z-index: 999; font: 500 12px/1 var(--sans); padding: 7px 10px; border-radius: 999px; background: var(--fg); color: var(--bg); text-decoration: none; opacity: .85; }
.demo-badge:hover { opacity: 1; }
CSS
echo "built $out"
