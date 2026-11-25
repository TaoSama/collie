"""Markdown to a Feishu post's rich text, and a received one back to text.

Blocks become paragraphs, quotes and code blocks; inside a line, `code`,
**bold**, *italic* and [links](https://…) keep their look. Anything else
(tables, HTML) stays as the text it was written as.
"""
import re

# RichTextElement.Tag
TAG_TEXT, TAG_IMG, TAG_P, TAG_AT, TAG_A, TAG_B, TAG_I, TAG_U = 1, 2, 3, 5, 6, 7, 8, 9
TAG_EMOTION, TAG_LINK, TAG_MEDIA, TAG_DOCS = 10, 17, 18, 22
TAG_QUOTE, TAG_CODE, TAG_CODE_BLOCK, TAG_CODE_BLOCK_V2 = 29, 30, 31, 37
MSG_POST = 2

BOLD = {"fontWeight": "bold"}
ITALIC = {"fontStyle": "italic"}

# Code block languages by the name after ```; the rest show as plain text.
LANGUAGES = {
    "bash": "BASH", "sh": "SHELL", "shell": "SHELL", "zsh": "SHELL", "console": "SHELL",
    "c": "C", "cpp": "CPP", "c++": "CPP", "cs": "C_SHARP", "csharp": "C_SHARP", "css": "CSS",
    "diff": "DIFF", "dockerfile": "DOCKER_FILE", "go": "GO", "golang": "GO", "graphql": "GRAPHQL",
    "html": "HTML", "http": "HTTP", "java": "JAVA", "js": "JAVASCRIPT", "javascript": "JAVASCRIPT",
    "json": "JSON", "kotlin": "KOTLIN", "kt": "KOTLIN", "lua": "LUA", "makefile": "MAKEFILE",
    "md": "MARKDOWN", "markdown": "MARKDOWN", "nginx": "NGINX", "php": "PHP", "proto": "PROTOBUF",
    "protobuf": "PROTOBUF", "py": "PYTHON", "python": "PYTHON", "rb": "RUBY", "ruby": "RUBY",
    "rs": "RUST", "rust": "RUST", "scala": "SCALA", "sql": "SQL", "swift": "SWIFT", "toml": "TOML",
    "ts": "TYPESCRIPT", "typescript": "TYPESCRIPT", "tsx": "TYPESCRIPT", "jsx": "JAVASCRIPT",
    "xml": "XML", "yaml": "YAML", "yml": "YAML",
}

_FENCE = re.compile(r"^\s{0,3}(```+|~~~+)\s*([\w+#.-]*)")
_QUOTE = re.compile(r"^\s{0,3}>\s?")
_HEADING = re.compile(r"^\s{0,3}#{1,6}\s+")
_BULLET = re.compile(r"^(\s*)[-*+]\s+(\[[ xX]\]\s+)?")
_RULE = re.compile(r"^\s{0,3}([-*_])(\s*\1){2,}\s*$")
# One inline piece: `code`, a [link](url), **bold**/__bold__, *italic*/_italic_, or a bare url.
_INLINE = re.compile(
    r"(`+)(.+?)\1"
    r"|\[([^\]\n]+)\]\((https?://[^\s)]+)\)"
    r"|\*\*(?=\S)(.+?)(?<=\S)\*\*|__(?=\S)(.+?)(?<=\S)__"
    r"|(?<![\w*])\*(?=[^\s*])(.+?)(?<=[^\s*])\*(?!\w)|(?<![\w_])_(?=[^\s_])(.+?)(?<=[^\s_])_(?![\w])"
    r"|(https?://[^\s<>()\[\]]*[^\s<>()\[\].,;:!?。，；：！？、）】」』])"
)


def inline(text, style=None):
    """A line's pieces: ("text", s, style), ("a", s, href) or ("code", s)."""
    out, at = [], 0
    style = style or {}

    def plain(s):
        if s:
            out.append(("text", s, dict(style)))

    for m in _INLINE.finditer(text):
        plain(text[at:m.start()])
        at = m.end()
        if m.group(2) is not None:
            out.append(("code", m.group(2).strip() if m.group(1) != "`" else m.group(2)))
        elif m.group(3) is not None:
            out.append(("a", m.group(3), m.group(4)))
        elif m.group(5) is not None or m.group(6) is not None:
            out.extend(inline(m.group(5) or m.group(6), dict(style, **BOLD)))
        elif m.group(7) is not None or m.group(8) is not None:
            out.extend(inline(m.group(7) or m.group(8), dict(style, **ITALIC)))
        else:
            out.append(("a", m.group(9), m.group(9)))
    plain(text[at:])
    return out


def blocks(markdown, footer=""):
    """Markdown as ("p", pieces), ("quote", [pieces…]) and ("code", lang, lines); footer as a last quote."""
    out, fence, code, lang, quote = [], None, [], "", []

    def end_quote():
        if quote:
            out.append(("quote", list(quote)))
            del quote[:]

    for raw in (markdown or "").replace("\r\n", "\n").strip("\n").split("\n"):
        if fence is not None:
            m = _FENCE.match(raw)
            if m and m.group(1)[0] == fence[0] and len(m.group(1)) >= len(fence) and not m.group(2):
                out.append(("code", lang, code))
                fence, code = None, []
            else:
                code.append(raw)
            continue
        m = _FENCE.match(raw)
        if m:
            end_quote()
            fence, lang, code = m.group(1), m.group(2).lower(), []
            continue
        if _QUOTE.match(raw):
            quote.append(inline(_QUOTE.sub("", raw, count=1)))
            continue
        end_quote()
        if not raw.strip():
            if out and out[-1] != ("p", []):
                out.append(("p", []))
            continue
        if _RULE.match(raw):
            out.append(("p", [("text", "────────", {})]))
            continue
        if _HEADING.match(raw):
            out.append(("p", inline(_HEADING.sub("", raw, count=1), BOLD)))
            continue
        b = _BULLET.match(raw)
        if b:
            box = (b.group(2) or "").strip()
            mark = {"[ ]": "☐ ", "[x]": "☑ ", "[X]": "☑ "}.get(box, "• ")
            raw = b.group(1) + mark + raw[b.end():]
        out.append(("p", inline(raw)))
    if fence is not None:
        out.append(("code", lang, code))
    end_quote()
    while out and out[-1] == ("p", []):
        out.pop()
    if footer.strip():
        out.append(("quote", [inline(line) for line in footer.strip().split("\n")]))
    return out


def pieces_text(pieces):
    return "".join(p[1] for p in pieces)


def inner_text(bl):
    """The post as plain text: what notifications and search show."""
    lines = []
    for b in bl:
        if b[0] == "p":
            lines.append(pieces_text(b[1]))
        elif b[0] == "quote":
            lines.extend(pieces_text(p) for p in b[1])
        else:
            lines.extend(b[2])
    return "\n".join(lines)


def build(L, markdown, footer=""):
    """A lark.entities.RichText for the post, from lark_all_pb2 (L)."""
    E = L.entities.RichTextElement
    rt = L.entities.RichText()
    seq = [0]

    def add(parent, tag, prop, style=None):
        seq[0] += 1
        eid = str(seq[0])
        el = rt.elements.dictionary[eid]
        el.tag = tag
        el.property = prop.SerializeToString()
        for k, v in (style or {}).items():
            el.style[k] = v
        parent.append(eid)
        return el

    def paragraph(parent, pieces):
        p = add(parent, TAG_P, E.ParagraphProperty())
        for kind, *rest in pieces or [("text", " ", {})]:
            if kind == "text":
                add(p.childIds, TAG_TEXT, E.TextProperty(content=rest[0]), rest[1])
            elif kind == "a":
                add(p.childIds, TAG_A, E.AnchorProperty(href=rest[1], content=rest[0], textContent=rest[0], isCustom=True))
                rt.anchorIds.append(p.childIds[-1])
            else:
                c = add(p.childIds, TAG_CODE, E.CodeProperty())
                add(c.childIds, TAG_TEXT, E.TextProperty(content=rest[0]))

    bl = blocks(markdown, footer)
    for b in bl:
        if b[0] == "p":
            paragraph(rt.elementIds, b[1])
        elif b[0] == "quote":
            q = add(rt.elementIds, TAG_QUOTE, E.QuoteProperty())
            for pieces in b[1]:
                paragraph(q.childIds, pieces)
        else:
            cb = E.CodeBlockV2Property(wordWrap=True)
            cb.language = E.CodeBlockV2Property.Language.Value(LANGUAGES.get(b[1], "PLAIN_TEXT"))
            for line in b[2] or [""]:
                cb.contents.add().contents.add(content=line or " ")
            add(rt.elementIds, TAG_CODE_BLOCK_V2, cb)
    rt.innerText = inner_text(bl)
    rt.version = 1
    return rt


PLACEHOLDERS = {TAG_IMG: "[图片]", TAG_EMOTION: "[表情]", TAG_MEDIA: "[视频]", TAG_DOCS: "[文档]"}


def to_text(L, rich):
    """A received rich text (as protobuf_to_dict gives it) as Markdown-ish text: quotes keep >, code keeps ```."""
    E = L.entities.RichTextElement
    els = (rich.get("elements") or {}).get("dictionary") or {}

    def prop(cls, el):
        p = cls()
        try:
            p.ParseFromString(el.get("property") or b"")
        except Exception:
            pass
        return p

    def walk(eid, depth=0):
        el = els.get(eid)
        if el is None or depth > 40:
            return ""
        kids = "".join(walk(k, depth + 1) for k in el.get("childIds") or [])
        t = el.get("tag", TAG_TEXT)
        if t in (TAG_TEXT, TAG_B, TAG_I, TAG_U, TAG_LINK):
            return prop(E.TextProperty, el).content + kids
        if t == TAG_A:
            a = prop(E.AnchorProperty, el)
            shown = a.textContent or a.content or a.href
            return shown if not a.href or shown == a.href else "[%s](%s)" % (shown, a.href)
        if t == TAG_AT:
            return prop(E.AtProperty, el).content or "@"
        if t == TAG_P:
            return kids + "\n"
        if t == TAG_QUOTE:
            return "".join("> " + line + "\n" for line in kids.rstrip("\n").split("\n"))
        if t == TAG_CODE:
            return "`%s`" % kids
        if t == TAG_CODE_BLOCK_V2:
            cb = prop(E.CodeBlockV2Property, el)
            lines = ["".join(c.content for c in line.contents) for line in cb.contents]
            return "```\n" + "\n".join(lines) + "\n```\n"
        if t == TAG_CODE_BLOCK:
            return "```\n" + prop(E.CodeBlockProperty, el).content + "\n```\n"
        return PLACEHOLDERS.get(t, "") + kids

    roots = rich.get("elementIds")
    if not roots:
        kids = {k for el in els.values() for k in el.get("childIds") or []}
        roots = [k for k in els if k not in kids]
    text = "".join(walk(e) for e in roots)
    return re.sub(r"\n{3,}", "\n\n", text).strip() or (rich.get("innerText") or "").strip()
