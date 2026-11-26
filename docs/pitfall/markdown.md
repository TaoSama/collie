# Markdown 渲染（`web/markdown.js` 的 `md`、`web/app.css` 的 `.md-table`）

## 表格单元格里的长内容会压到相邻列

聊天消息和文件预览都用 `.msg` 加 `.md-table`。单元格有 `max-width`，宽度到了上限就不再变宽，放不下的内容如果不能折行，就会画到右边几列上面，而不是让表格变宽。

- 单元格用 `overflow-wrap: break-word`，不要用 `normal`。`.msg` 本身用的是 `anywhere`，它会把 min-content 压到一个字符宽，表格列就会被挤窄；`break-word` 只在溢出时断行，不影响列宽。
- 行内代码不要整体 `white-space: nowrap`。`md` 给不超过 `CODE_CHIP_MAX` 个字符的行内代码加上 `data-chip`，只有这种短代码在单元格里保持一行；长路径、长 URL 会在单元格内折行。
- 验收时检查单元格里 `code`、`a` 的 `getClientRects()` 有没有超出 `td` 的右边界，手机和桌面尺寸都要看。
