"""Read-only link preview resolution for Feishu cloud docs and Meego (飞书项目) URLs.

Priority order:
1. Message-embedded entities (DocEntity, MessageUrlPreview).
2. Feishu gateway read-only batch APIs with in-memory TTL cache:
   - docs.PullDocByURLsRequest (Feishu docs/sheets/bitable/wiki/slides/mindnotes/files)
   - url_preview.GetPreviewRequest (Meego issues/stories/views, Feishu minutes/boards, fallback)
3. URL structure fallback (type, project space, work-item id) when offline or without permission.
"""
import collections
import datetime as dt
import logging
import re
import threading
import time
from urllib.parse import urlparse

import message_view

log = logging.getLogger("larkx")

CACHE_TTL = 900  # 15 minutes
CACHE_MAX = 600
BATCH_SIZE = 20
MAX_CARDS = 3

URL_RE = re.compile(r"https?://[^\s<>()\"'\u3000-\u303f\uff00-\uffef]+")
MEEGO_HOST_RE = re.compile(r"^(?:meego|project)\.(?:feishu\.cn|larkoffice\.com|larksuite\.com|bytedance\.net)$")
LARK_HOST_SUFFIXES = (".feishu.cn", ".larkoffice.com", ".larksuite.com")

# lark.entities.DocType enum -> (kind, label, short_label)
DOC_TYPE_INFO = {
    1: ("doc", "飞书文档", "文档"),
    2: ("sheet", "电子表格", "表格"),
    3: ("bitable", "多维表格", "多维表格"),
    4: ("mindnote", "思维笔记", "思维笔记"),
    5: ("file", "云文件", "云文件"),
    6: ("slides", "幻灯片", "幻灯片"),
    7: ("wiki", "知识库", "知识库"),
    8: ("docx", "飞书文档", "文档"),
    9: ("folder", "云文件夹", "文件夹"),
    11: ("slides", "幻灯片", "幻灯片"),
}

LARK_PATH_INFO = {
    "docx": ("docx", "飞书文档", "文档", True),
    "docs": ("doc", "飞书文档", "文档", True),
    "sheets": ("sheet", "电子表格", "表格", True),
    "base": ("bitable", "多维表格", "多维表格", True),
    "wiki": ("wiki", "知识库", "知识库", True),
    "slides": ("slides", "幻灯片", "幻灯片", True),
    "mindnotes": ("mindnote", "思维笔记", "思维笔记", True),
    "file": ("file", "云文件", "云文件", True),
    "minutes": ("minutes", "飞书妙记", "妙记", False),
    "board": ("board", "画板", "画板", False),
}

MEEGO_TITLE_PREFIX = re.compile(r"^\s*\[(?:Issues?|Features?|Stories?|Story|View|Subtasks?|Tasks?|缺陷|需求|视图)\]\s*", re.IGNORECASE)
MEEGO_PRIORITY_RE = re.compile(r"^(?:P[0-4]|Urgent|High|Medium|Low|紧急|高|中|低)$", re.IGNORECASE)

PERM_LABELS = {
    "you can edit": "可编辑",
    "you can view": "可阅读",
    "you can comment": "可评论",
    "no permission": "无权限",
}

STATUS_LABELS = {
    "in development": "开发中",
    "to be launched": "待上线",
    "launched": "已上线",
    "in testing": "测试中",
    "in progress": "进行中",
    "pending": "待处理",
    "resolved": "已解决",
    "rejected": "已拒绝",
    "terminated": "已终止",
}

STATUS_TONES = {
    "open": "info",
    "开发中": "info",
    "进行中": "info",
    "测试中": "info",
    "待处理": "info",
    "待上线": "attn",
    "待终验和集成测试": "attn",
    "closed": "muted",
    "已解决": "ok",
    "已上线": "ok",
    "已完成": "ok",
    "已拒绝": "muted",
    "已终止": "muted",
}


def classify_url(raw_url):
    """Classify a Feishu cloud doc or Meego URL, or return None for other URLs."""
    if not isinstance(raw_url, str) or not raw_url.startswith(("http://", "https://")):
        return None
    try:
        u = urlparse(raw_url.strip())
    except ValueError:
        return None
    host = (u.hostname or "").lower()
    if not host:
        return None
    segs = [s for s in (u.path or "").split("/") if s]
    if not segs:
        return None

    if MEEGO_HOST_RE.match(host):
        space = segs[0]
        if len(segs) >= 4 and segs[2] == "detail":
            item_type, item_id = segs[1].lower(), segs[3]
            if item_type in ("issue", "bug"):
                return dict(family="meego", kind="meego-issue", label="飞书项目 · 缺陷",
                            short_label="缺陷", space=space, item_id=item_id, supports_doc_api=False)
            if item_type in ("story", "feature"):
                return dict(family="meego", kind="meego-story", label="飞书项目 · 需求",
                            short_label="需求", space=space, item_id=item_id, supports_doc_api=False)
            return dict(family="meego", kind="meego", label="飞书项目",
                        short_label="项目", space=space, item_id=item_id, supports_doc_api=False)
        if len(segs) >= 3:
            sub = segs[1].lower()
            if sub == "issueview":
                return dict(family="meego", kind="meego-view", label="飞书项目 · 缺陷视图",
                            short_label="缺陷视图", space=space, item_id=segs[2], supports_doc_api=False)
            if sub == "storyview":
                return dict(family="meego", kind="meego-view", label="飞书项目 · 需求视图",
                            short_label="需求视图", space=space, item_id=segs[2], supports_doc_api=False)
            if sub in ("multiprojectview", "view", "workitemview", "kanban"):
                return dict(family="meego", kind="meego-view", label="飞书项目 · 视图",
                            short_label="视图", space=space, item_id=segs[2], supports_doc_api=False)
        return None

    if any(host.endswith(suffix) for suffix in LARK_HOST_SUFFIXES):
        first = segs[0].lower()
        if first == "drive" and len(segs) >= 3 and segs[1].lower() == "folder":
            return dict(family="doc", kind="folder", label="云文件夹",
                        short_label="文件夹", space="", item_id=segs[2], supports_doc_api=True)
        hit = LARK_PATH_INFO.get(first)
        if hit and len(segs) >= 2:
            kind, label, short_label, supports_doc_api = hit
            return dict(family="doc", kind=kind, label=label,
                        short_label=short_label, space="", item_id=segs[1], supports_doc_api=supports_doc_api)
    return None


def _iso_time(ts):
    if not ts:
        return ""
    try:
        n = int(ts)
        if n <= 0:
            return ""
        if n >= 10 ** 12:
            n = n / 1000.0
        return dt.datetime.fromtimestamp(n, dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    except (ValueError, TypeError, OverflowError, OSError):
        return ""


def _cache_key(url):
    """Normalize a URL for cache lookup (drop volatile query params on Meego detail URLs)."""
    try:
        u = urlparse(url)
    except ValueError:
        return url
    host = (u.hostname or "").lower()
    if MEEGO_HOST_RE.match(host):
        return f"{u.scheme}://{host}{u.path}"
    return f"{u.scheme}://{host}{u.path}" + (f"?{u.query}" if u.query and "sheet=" in u.query else "")


class LinkCache:
    """Thread-safe bounded TTL cache for resolved link metadata."""

    def __init__(self, ttl=CACHE_TTL, limit=CACHE_MAX):
        self._ttl = ttl
        self._limit = limit
        self._mu = threading.Lock()
        self._items = collections.OrderedDict()

    def get(self, key):
        now = time.monotonic()
        with self._mu:
            hit = self._items.get(key)
            if not hit:
                return None
            if hit[0] <= now:
                self._items.pop(key, None)
                return None
            self._items.move_to_end(key)
            return dict(hit[1])

    def put(self, key, value):
        now = time.monotonic()
        with self._mu:
            self._items[key] = (now + self._ttl, dict(value))
            self._items.move_to_end(key)
            while len(self._items) > self._limit:
                self._items.popitem(last=False)


def _pick_i18n(tpl_map, key):
    entry = message_view.obj(message_view.obj(tpl_map).get(key))
    langs = message_view.obj(entry.get("i18nTemplates"))
    for k in ("zh_cn", "zh-CN", "zh_CN", "en_us", "en-US"):
        if langs.get(k):
            return str(langs[k]).strip()
    for v in langs.values():
        if v:
            return str(v).strip()
    return ""


def _tag_tone(tag, is_priority=False):
    if is_priority:
        up = tag.upper()
        if up in ("P0", "URGENT", "紧急"):
            return "danger"
        if up in ("P1", "HIGH", "高"):
            return "attn"
        return "muted"
    return STATUS_TONES.get(tag.lower(), STATUS_TONES.get(tag, "info"))


def _parse_preview_entity(ent, base_info, user_lookup=None):
    """Extract normalized link metadata from a url_preview.UrlPreviewEntity dict."""
    info = dict(base_info)
    tpl = ent.get("localeI18nTemplates") or {}
    raw_title = _pick_i18n(tpl, ent.get("i18nTitleKey") or "title")
    title = raw_title
    if info["family"] == "meego" and title:
        title = MEEGO_TITLE_PREFIX.sub("", title).strip() or raw_title

    body = message_view.obj(ent.get("previewBody"))
    comps = message_view.obj(body.get("components"))
    els = message_view.obj(comps.get("elements"))

    if info["family"] == "doc":
        for ev in els.values():
            ev = message_view.obj(ev)
            doc_img = message_view.obj(ev.get("docImage"))
            dtype = doc_img.get("docType")
            if dtype in DOC_TYPE_INFO:
                dk, dl, ds = DOC_TYPE_INFO[dtype]
                if info["kind"] == "wiki" and dk != "wiki":
                    info["kind"] = dk
                    info["label"] = f"知识库 · {dl}"
                    info["short_label"] = ds
                else:
                    info["kind"], info["label"], info["short_label"] = dk, dl, ds
            txt = message_view.obj(ev.get("text")).get("text")
            if isinstance(txt, str) and txt.strip().lower() in PERM_LABELS:
                info["permission"] = PERM_LABELS[txt.strip().lower()]
        if title:
            info["title"] = title
        return info

    # Meego entity parsing
    space_label = ""
    priorities = []
    statuses = []
    creator_id = ""
    view_items = 0

    for ek, ev in els.items():
        ev = message_view.obj(ev)
        hdr = message_view.obj(ev.get("header"))
        if hdr.get("title"):
            ht = str(hdr["title"]).strip()
            if "·" in ht:
                parts = [p.strip() for p in ht.split("·") if p.strip()]
                if parts:
                    space_label = parts[-1]
        if "subtitle" in ek and not title:
            st = message_view.obj(ev.get("text")).get("text")
            if st:
                title = str(st).strip()
        if "creator" in ek and not creator_id:
            av = message_view.obj(ev.get("avatar"))
            ci = message_view.obj(av.get("chatterInfo"))
            if ci.get("chatterId"):
                creator_id = str(ci["chatterId"])
        if ek.startswith("common-text-width-lineheight-text-custom-"):
            view_items += 1
        tlist = message_view.obj(ev.get("tagList"))
        for raw_tag in message_view.seq(tlist.get("tags")):
            tag = str(raw_tag or "").strip()
            if not tag:
                continue
            if MEEGO_PRIORITY_RE.match(tag):
                if tag not in priorities:
                    priorities.append(tag)
            elif info["kind"] != "meego-view":
                localized = STATUS_LABELS.get(tag.lower(), tag)
                if localized not in statuses:
                    statuses.append(localized)

    if title:
        info["title"] = title
    if space_label:
        info["space_label"] = space_label
    elif info.get("space"):
        info["space_label"] = info["space"]

    badges = []
    for p in priorities[:1]:
        badges.append({"text": p, "value": _tag_tone(p, is_priority=True)})
    for s in statuses[:1]:
        badges.append({"text": s, "value": _tag_tone(s, is_priority=False)})
    if badges:
        info["badges"] = badges

    if creator_id and callable(user_lookup):
        cname = user_lookup(creator_id)
        if cname:
            info["owner"] = cname
    elif view_items > 0 and info["kind"] == "meego-view":
        info["item_count"] = view_items
    return info


def _parse_doc_entry(doc_obj, base_info):
    """Extract normalized link metadata from docs.PullDocByURLsResponse.docs entry."""
    info = dict(base_info)
    dtype = doc_obj.get("docType")
    if dtype in DOC_TYPE_INFO:
        dk, dl, ds = DOC_TYPE_INFO[dtype]
        if info["kind"] == "wiki" and dk not in ("wiki", "docx", "doc"):
            info["kind"] = dk
            info["label"] = f"知识库 · {dl}"
            info["short_label"] = ds
        elif info["kind"] != "wiki":
            info["kind"], info["label"], info["short_label"] = dk, dl, ds
    name = (doc_obj.get("name") or "").strip()
    if name:
        info["title"] = name
    owner = (doc_obj.get("ownerName") or "").strip()
    if owner:
        info["owner"] = owner
    updated = _iso_time(doc_obj.get("updateTime") or doc_obj.get("createTime"))
    if updated:
        info["updated_at"] = updated
    return info


def _fallback_info(url, base_info):
    """Build a readable fallback when no API metadata is available."""
    info = dict(base_info)
    if info.get("title"):
        return info
    if info["family"] == "meego":
        space = info.get("space_label") or info.get("space") or ""
        item_id = info.get("item_id") or ""
        if info["kind"] == "meego-view":
            info["title"] = f"{space} {info['short_label']}".strip() if space else info["label"]
        else:
            info["title"] = f"{info['short_label']} #{item_id}" if item_id else info["label"]
        if space:
            info["space_label"] = space
    else:
        if info.get("deleted"):
            info["title"] = f"{info['label']}（已删除）"
        elif info.get("no_perm"):
            info["title"] = f"{info['label']}（无权限访问）"
        else:
            info["title"] = info["label"]
    return info


def _extract_embedded(msg):
    """Priority 1: extract preview metadata already embedded in the message itself."""
    data = message_view.obj(msg.get("content_data"))
    embedded_by_eid = {}
    doc_ent = message_view.obj(data.get("docEntity"))
    for eid, ref in message_view.obj(doc_ent.get("elementEntityRef")).items():
        ref = message_view.obj(ref)
        title = (ref.get("title") or "").strip()
        if title and not title.startswith(("http://", "https://")):
            embedded_by_eid[str(eid)] = {
                "title": title,
                "docType": ref.get("docType"),
            }
    embedded_by_url = {}
    for item in message_view.seq(data.get("urls")):
        item = message_view.obj(item)
        u = (item.get("url") or "").strip()
        t = (item.get("title") or "").strip()
        if u and t and not t.startswith(("http://", "https://")):
            embedded_by_url[u] = {"title": t}
    return embedded_by_eid, embedded_by_url


def _extract_message_links(U, msg):
    """Return list of (url, preview_id, eid, base_info) for previewable links in a message."""
    if msg.get("msg_type") not in (message_view.POST, message_view.TEXT):
        return []
    data = message_view.obj(msg.get("content_data"))
    rich = message_view.obj(data.get("richText"))
    els = message_view.obj(message_view.obj(rich.get("elements")).get("dictionary"))
    hang_points = message_view.obj(msg.get("url_previews"))
    E = U.L.entities.RichTextElement

    # Map eid -> (url, preview_id) from hang_points
    hp_by_eid = {}
    hp_by_url = {}
    for eid, hp in hang_points.items():
        hp = message_view.obj(hp)
        u = (hp.get("url") or "").strip()
        pid = (hp.get("previewId") or "").strip()
        if u:
            hp_by_eid[str(eid)] = (u, pid)
            if pid and u not in hp_by_url:
                hp_by_url[u] = pid

    found = []
    seen = set()

    def add(url, pid="", eid=""):
        url = (url or "").strip()
        if not url:
            return
        base = classify_url(url)
        if not base:
            return
        if not pid:
            pid = hp_by_url.get(url, "")
        key = (url, eid)
        if key in seen:
            return
        seen.add(key)
        found.append((url, pid, eid, base))

    from google.protobuf.message import DecodeError
    # Walk richText in visual document order (elementIds -> childIds), then any remaining elements.
    roots = list(message_view.seq(rich.get("elementIds") or rich.get("element_ids")))
    if not roots and els:
        descendants = {k for e in els.values() for k in message_view.seq(message_view.obj(e).get("childIds")) if isinstance(k, str)}
        roots = [k for k in els if k not in descendants]
    ordered_eids = []
    visited = set()

    def visit(ids, depth=0):
        if depth > message_view.VIEW_DEPTH:
            return
        for eid in message_view.seq(ids):
            if not isinstance(eid, str) or eid in visited:
                continue
            visited.add(eid)
            ordered_eids.append(eid)
            el = message_view.obj(els.get(eid))
            visit(el.get("childIds"), depth + 1)

    visit(roots)
    for eid in els:
        if isinstance(eid, str) and eid not in visited:
            ordered_eids.append(eid)

    for eid in ordered_eids:
        el = message_view.obj(els.get(eid))
        tag = el.get("tag")
        if isinstance(tag, str):
            tag = E.Tag.Value(tag) if tag in E.Tag.keys() else 0
        raw = el.get("property") or b""
        if tag == E.A and isinstance(raw, bytes):
            a = E.AnchorProperty()
            try:
                a.ParseFromString(raw)
            except DecodeError:
                continue
            u = (a.pcHref or a.href or "").strip()
            hp_u, pid = hp_by_eid.get(str(eid), ("", ""))
            add(u or hp_u, pid, str(eid))
        elif tag == E.LINK and isinstance(raw, bytes):
            lp = E.LinkProperty()
            try:
                lp.ParseFromString(raw)
            except DecodeError:
                continue
            u = (lp.pcUrl or lp.url or "").strip()
            hp_u, pid = hp_by_eid.get(str(eid), ("", ""))
            add(u or hp_u, pid, str(eid))
        elif tag in (E.TEXT, E.B, E.I, E.U) and isinstance(raw, bytes):
            tp = E.TextProperty()
            try:
                tp.ParseFromString(raw)
            except DecodeError:
                continue
            for m in URL_RE.finditer(tp.content or ""):
                u = m.group(0).rstrip(".,;:!?)]}")
                add(u, hp_by_url.get(u, ""), "")

    # Also check hang_points directly and plain text fallback
    for eid, (u, pid) in hp_by_eid.items():
        add(u, pid, eid)
    if not found:
        plain = message_view.content(data.get("text"))
        for m in URL_RE.finditer(plain):
            u = m.group(0).rstrip(".,;:!?)]}")
            add(u, "", "")
    return found


def _looks_like_raw_url(text, url):
    s = (text or "").strip()
    if not s:
        return True
    if s == url or s.startswith(("http://", "https://")):
        return True
    return False


def _meta_text(info):
    """Build secondary metadata line (owner or Meego space/item count)."""
    parts = []
    if info["family"] == "meego":
        if info.get("space_label"):
            parts.append(info["space_label"])
        if info.get("owner"):
            parts.append(info["owner"])
        elif info.get("item_count"):
            parts.append(f"{info['item_count']} 项预览")
    else:
        if info.get("owner"):
            parts.append(info["owner"])
        elif info.get("permission"):
            parts.append(info["permission"])
    return " · ".join(parts)


def _link_card_node(url, info):
    """Build a link-card MessageNode."""
    n = {
        "kind": "link-card",
        "url": url,
        "text": info.get("title") or info["label"],
        "tone": info["kind"],
        "name": info["label"],
    }
    meta = _meta_text(info)
    if meta:
        n["meta"] = meta
    if info.get("updated_at"):
        n["updated_at"] = info["updated_at"]
    if info.get("badges"):
        n["options"] = info["badges"]
    return n


def _split_text_urls(text, resolved_map):
    """Split a plain text string containing raw previewable URLs into text + link nodes."""
    if not text or "http" not in text:
        return None
    parts = []
    pos = 0
    matched = False
    for m in URL_RE.finditer(text):
        u = m.group(0).rstrip(".,;:!?)]}")
        end = m.start() + len(u)
        if u not in resolved_map:
            continue
        matched = True
        if m.start() > pos:
            parts.append(message_view.node("text", text[pos:m.start()]))
        parts.append(message_view.node("link", u, url=u))
        pos = end
    if not matched:
        return None
    if pos < len(text):
        parts.append(message_view.node("text", text[pos:]))
    return parts


def _enrich_nodes(nodes, resolved_map, card_mode):
    """Walk MessageNodes: replace raw URLs with rich inline links and promote standalone links."""
    promoted_urls = set()

    def walk_inline(items):
        out = []
        for n in items:
            kind = n.get("kind")
            if kind == "text" and not n.get("children") and n.get("text"):
                split = _split_text_urls(n["text"], resolved_map)
                if split:
                    out.extend(walk_inline(split))
                    continue
            if kind == "link":
                u = n.get("url") or ""
                info = resolved_map.get(u)
                if info:
                    new_n = dict(n)
                    new_n["tone"] = info["kind"]
                    new_n["name"] = info["short_label"]
                    raw_text = n.get("text") or ""
                    has_children = bool(n.get("children"))
                    if not has_children and _looks_like_raw_url(raw_text, u):
                        new_n["text"] = info.get("title") or info["label"]
                    elif has_children:
                        child_text = message_view.words(n["children"])
                        if _looks_like_raw_url(child_text, u):
                            new_n["text"] = info.get("title") or info["label"]
                            new_n.pop("children", None)
                    if not card_mode and info.get("badges"):
                        new_n["options"] = info["badges"]
                    out.append(new_n)
                    continue
            if n.get("children"):
                new_n = dict(n)
                new_n["children"] = walk_inline(n["children"])
                out.append(new_n)
            else:
                out.append(n)
        return out

    enriched = walk_inline(nodes)
    if not card_mode:
        return enriched, promoted_urls

    # Promote top-level paragraphs containing only a single previewable link into link-card nodes
    final_nodes = []
    for n in enriched:
        if n.get("kind") == "paragraph" and n.get("children"):
            non_blank = [
                c for c in n["children"]
                if not (c.get("kind") == "text" and not c.get("children") and not (c.get("text") or "").strip())
            ]
            if len(non_blank) == 1 and non_blank[0].get("kind") == "link":
                u = non_blank[0].get("url") or ""
                info = resolved_map.get(u)
                if info:
                    final_nodes.append(_link_card_node(u, info))
                    promoted_urls.add(u)
                    continue
        final_nodes.append(n)
    return final_nodes, promoted_urls


def _replace_preview_urls(preview, resolved_map):
    if not preview:
        return preview
    out = preview
    for u, info in resolved_map.items():
        if u in out:
            title = info.get("title") or info["label"]
            tag = info.get("short_label") or info["label"]
            out = out.replace(u, f"[{tag}] {title}")
    return " ".join(out.split())[:500]


def collect_links(U, pairs):
    """Collect the links and embedded titles shared by rendering and background lookup."""
    found = []
    for msg, out in pairs:
        links = _extract_message_links(U, msg)
        if links:
            emb_eid, emb_url = _extract_embedded(msg)
            found.append((msg, out, links, emb_eid, emb_url))
    return found


def render_links(U, cache, per_msg):
    """Enrich views and previews without changing the text used by agents."""
    for msg, out, links, emb_eid, emb_url in per_msg:
        resolved_map = {}
        ordered_urls = []
        for u, pid, eid, base in links:
            if u in resolved_map:
                continue
            # Priority 1: message-embedded entity first
            e_hit = emb_eid.get(eid) or emb_url.get(u)
            if e_hit and e_hit.get("title"):
                info = dict(base, title=e_hit["title"])
                dt_val = e_hit.get("docType")
                if dt_val in DOC_TYPE_INFO and info["kind"] != "wiki":
                    info["kind"], info["label"], info["short_label"] = DOC_TYPE_INFO[dt_val]
            else:
                ck = _cache_key(u)
                info = cache.get(ck) or dict(base)
            info = _fallback_info(u, info)
            resolved_map[u] = info
            ordered_urls.append(u)

        if not resolved_map:
            continue

        # Ensure view exists (TEXT messages without emojis/images start with view=None)
        view = out.get("view")
        if view is None:
            view, _ = message_view.describe(U, msg, force_post=True)
            if view is None:
                continue
            out["view"] = view

        card_mode = len(ordered_urls) <= MAX_CARDS
        new_nodes, promoted = _enrich_nodes(view.get("nodes") or [], resolved_map, card_mode=card_mode)

        # Append preview cards for inline links when the message has <= MAX_CARDS unique previewable links
        if card_mode:
            for u in ordered_urls:
                if u not in promoted:
                    new_nodes.append(_link_card_node(u, resolved_map[u]))

        view["nodes"] = new_nodes
        base_preview = out.get("preview") or out.get("text") or ""
        updated_preview = _replace_preview_urls(base_preview, resolved_map)
        if updated_preview:
            out["preview"] = updated_preview
