"""Optional preview lookups run outside message delivery and history reads."""
import logging
import threading
from concurrent.futures import ThreadPoolExecutor

import message_view
from link_preview import (
    BATCH_SIZE, CACHE_MAX, LinkCache, _cache_key, _extract_message_links, _parse_doc_entry,
    _parse_preview_entity, classify_url, collect_links, render_links,
)

log = logging.getLogger("larkx")

MISS_TTL = 30  # give empty or failed lookups a break from inbox polling


class LinkPreviews:
    """One login's preview cache and bounded lookup queue."""

    def __init__(self, adapter, client):
        self._U = adapter.U
        self.client = client
        self._names = adapter.names
        self.cache = LinkCache()
        self._pending = set()
        self._lock = threading.Lock()
        self._pool = ThreadPoolExecutor(max_workers=1)

    def close(self):
        with self._lock:
            if self._pool is not None:
                self._pool.shutdown(wait=False, cancel_futures=True)
                self._pool = None

    def enrich(self, pairs, *, include_fallback=False):
        found = collect_links(self._U, pairs)
        if not found:
            return
        with self._lock:
            if self._pool is None:
                return
            render_links(self._U, self.cache, found, include_fallback)
            if self.client is None:
                return
            keys, work = set(), []
            for msg, _, links, emb_eid, emb_url in found:
                missing = []
                for u, pid, eid, base in links:
                    key = _cache_key(u)
                    if key in self._pending or self.cache.get(key) is not None:
                        continue
                    if (emb_eid.get(eid) or emb_url.get(u) or {}).get("title"):
                        continue
                    has_lookup = base["supports_doc_api"] or pid or (
                        not msg.get("url_previews") and str(msg.get("msg_id") or "").isdigit())
                    if has_lookup:
                        keys.add(key)
                        missing.append((u, pid, base))
                if missing:
                    # The gateway can fill missing hang points without mutating the delivered message.
                    work.append((dict(msg), missing))
            if not keys or len(keys | self._pending) > CACHE_MAX:
                return
            self._pending.update(keys)
            self._pool.submit(self._fetch, work, keys)

    def _fetch(self, per_msg, keys):
        try:
            _resolve(self._U, self.client, self.cache, per_msg, keys,
                     lambda uid: self._names.peek("user:" + str(uid)) or "")
        except Exception:
            log.exception("resolving link previews")
        finally:
            with self._lock:
                for key in keys:
                    if self.cache.get(key) is None:
                        self.cache.put(key, {}, ttl=MISS_TTL)
                self._pending.difference_update(keys)


def _resolve(U, client, cache, per_msg, keys, user_peek):
    url_to_base, pids_to_fetch = {}, {}
    msgs_needing_hp = []
    for msg, missing in per_msg:
        for u, pid, base in missing:
            if base["supports_doc_api"]:
                url_to_base[u] = base
            if pid:
                pids_to_fetch.setdefault(pid, (u, base))
        if any(not base["supports_doc_api"] and not pid for _, pid, base in missing):
            msgs_needing_hp.append(msg)

    # A live push can lack the hang points that map Meego/minutes URLs to preview IDs.
    if msgs_needing_hp:
        mids = list(dict.fromkeys(str(m["msg_id"]) for m in msgs_needing_hp))[:BATCH_SIZE]
        try:
            pulled = (client.api("messages.PullMessagesByIdsRequest", {"messageIds": mids}) or {}).get("messages") or {}
        except Exception as e:
            log.debug("link preview pull messages: %s", e)
            pulled = {}
        for m in msgs_needing_hp:
            raw_m = message_view.obj(pulled.get(str(m["msg_id"])))
            if hp := message_view.obj(raw_m.get("urlPreviewHangPointMap")):
                m["url_previews"] = hp
                for u, pid, _, base in _extract_message_links(U, m):
                    if pid and _cache_key(u) in keys:
                        pids_to_fetch.setdefault(pid, (u, base))

    # Resolve cloud docs before trying generic URL previews.
    resolved_fresh = {}  # cache_key -> info
    if url_to_base:
        urls_list = list(url_to_base)
        for i in range(0, len(urls_list), BATCH_SIZE):
            chunk = urls_list[i:i + BATCH_SIZE]
            try:
                resp = client.api("docs.PullDocByURLsRequest", {"urls": chunk}) or {}
            except Exception as e:
                log.warning("docs.PullDocByURLsRequest: %s", e)
                continue
            for u, doc_obj in message_view.obj(resp.get("docs")).items():
                base = url_to_base.get(u) or classify_url(u)
                if base and isinstance(doc_obj, dict):
                    info = _parse_doc_entry(doc_obj, base)
                    ck = _cache_key(u)
                    resolved_fresh[ck] = info
                    cache.put(ck, info)
            for u in list(message_view.seq(resp.get("noPermUrls"))) + list(message_view.seq(resp.get("noPermUrlsV2"))):
                ck = _cache_key(u)
                if ck not in resolved_fresh and u in url_to_base:
                    info = dict(url_to_base[u], no_perm=True)
                    resolved_fresh[ck] = info
                    cache.put(ck, info)
            for u in message_view.seq(resp.get("deletedUrlsV2")):
                ck = _cache_key(u)
                if ck not in resolved_fresh and u in url_to_base:
                    info = dict(url_to_base[u], deleted=True)
                    resolved_fresh[ck] = info
                    cache.put(ck, info)

    seen_preview_cks = set()
    remaining_pids = {}
    for pid, (u, base) in pids_to_fetch.items():
        ck = _cache_key(u)
        if ck in seen_preview_cks or (resolved_fresh.get(ck) or {}).get("title") or cache.get(ck) is not None:
            continue
        seen_preview_cks.add(ck)
        remaining_pids[pid] = (u, base)
    if remaining_pids:
        pid_list = list(remaining_pids.keys())
        for i in range(0, len(pid_list), BATCH_SIZE):
            chunk = pid_list[i:i + BATCH_SIZE]
            try:
                resp = client.api("url_preview.GetPreviewRequest",
                                  {"appId": 1, "previewIds": chunk, "locale": "zh_CN"}) or {}
            except Exception as e:
                log.warning("url_preview.GetPreviewRequest: %s", e)
                continue
            pmap = message_view.obj(resp.get("previewEntityMap"))
            for pid in chunk:
                ent = message_view.obj(pmap.get(pid))
                if not ent:
                    continue
                u, base = remaining_pids[pid]
                info = _parse_preview_entity(ent, base, user_lookup=user_peek)
                cache.put(_cache_key(u), info)
