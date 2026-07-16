#!/usr/bin/env python3
"""Phase B 图源 + 许可分类(Python 侧, 建库机用)。pull_supplemental 从这里为欠覆盖库内株
深挖真实库外图。三源(用户选定):
  - Wikimedia Commons —— 深挖: 超出 imageingest 的 top-4(那是用户可见图库), gsrlimit 到 ~50。
  - GBIF occurrence media(api.gbif.org)—— StillImage, 服务端 license 预筛 + 逐图再分类。
  - PlantNet related-images —— 可选, 需 seed 图上传(消耗线上识别配额), 只对**非品种种级**株用
    (PlantNet 无 cultivar taxa, 品种用它会拉回母种图 → 重蹈误标覆辙, §pull_supplemental)。

★铁律(违反即违许可): 只收 CC0 / CC-BY / CC-BY-SA。NC(非商用)/ND(禁改)绝不用 —— YardMate 是
商用 App, 拿 NC 图训商用模型违反许可, "训完即删"救不了。classify_license 是这条铁律的命门。

这里只负责"抓 + 分类 + 下载", 不判断植物学正确性(那是 supp_verify 的事)。返回 Candidate 列表。
"""
import io
import os
import re
import json
import time
import urllib.parse
import urllib.request

UA = "YardMate-L1-vision/1.0 (reference index build; contact emanon.me@gmail.com)"

# 下载护栏(建库机 = 可信操作者, 无 SSRF 面; 仅防坏图/超大图/挂死)。
MAX_IMG_BYTES = 20 << 20   # 20 MiB 单图上限
DL_TIMEOUT = 25            # 秒
API_TIMEOUT = 30


class Candidate:
    """一个库外候选图(未做正确性判定)。source/license/attribution 入 manifest 存档
    (BY/SA 需署名, 我们不再分发原图、只留派生向量, 但署名留档合规且可审计)。"""
    __slots__ = ("source", "url", "page_url", "license", "author", "dedup_key")

    def __init__(self, source, url, page_url, license, author, dedup_key):
        self.source = source
        self.url = url
        self.page_url = page_url
        self.license = license          # 归一后的 'CC0'|'PD'|'CC-BY'|'CC-BY-SA'
        self.author = author or ""
        self.dedup_key = dedup_key      # 跨源去重键(photo id / File: page / 图 URL)

    def as_manifest(self):
        return {"source": self.source, "url": self.url, "page_url": self.page_url,
                "license": self.license, "author": self.author}


# ────────────────────────────── 许可分类(§2.4.3 的 Python port) ──────────────────────────────

_CC_URL_RE = re.compile(r"creativecommons\.org/licenses/([a-z0-9-]+)")


def classify_license(code_or_url, short_name=""):
    """token-membership 许可分类(port proxy/imageingest/license.go §2.4.3)。
    返回 'CC0'|'PD'|'CC-BY'|'CC-BY-SA', 或 None(拒: NC/ND/ARR/GFDL/未知 —— 一律保守拒)。

    统一吃三种形态:
      - iNat 无版本码      'cc-by' / 'cc-by-sa' / 'cc0'
      - Wikimedia 版本码    'cc-by-sa-4.0' / 'CC BY 4.0'(短名)
      - GBIF 许可 URL/枚举  'http://creativecommons.org/licenses/by-nc/4.0/' / 'CC_BY_SA_4_0'
    版本号(4.0/3.0)无关, 只看 token 成员。NC/ND 命中即拒(优先级最高)。"""
    raw = (code_or_url or "").strip().lower()
    sn = (short_name or "").strip().lower()

    # publicdomain URL 特判(路径里没有 by/cc token)。
    if "publicdomain/zero" in raw:
        return "CC0"
    if "publicdomain/mark" in raw or "publicdomain/1.0" in raw:
        return "PD"

    # CC 许可 URL → 抽出路径码(by / by-sa / by-nc-nd ...), 再走 token 逻辑。
    m = _CC_URL_RE.search(raw)
    body = m.group(1) if m else raw

    # 拆 token —— ★code 和 short_name **一起**拆。命门(否则违铁律): 老 Wikimedia 文件机读 License
    # 码常为空, 许可只写在 LicenseShortName(如 'CC BY-NC 2.0'); 只查 code 会漏掉短名里的 NC/ND
    # → 把 NC 图误判 CC-BY 折进商用索引。连字符/下划线/斜杠/空格/点 都是分隔(覆盖 'cc-by-sa'/
    # GBIF 'CC_BY_SA_4_0'/短名 'CC BY-SA 3.0')。
    toks = set(t for t in re.split(r"[-_/ .]+", body) if t)
    toks |= set(t for t in re.split(r"[-_/ .]+", sn) if t)

    # NC/ND 命中即拒 —— 最高优先级, 对 code + 短名 一并查, 挡在所有"允许"之前。
    if toks & {"nc", "nd", "noncommercial", "noderivs", "noderivatives"}:
        return None

    if "cc0" in toks or "zero" in toks:
        return "CC0"
    if "pd" in toks or "publicdomain" in body or "public domain" in sn:
        return "PD"

    # 是否像 CC 许可: 来自 CC URL, 或带 'cc' token(code 或短名)。防把随机 'by' 文本误判。
    is_cc = bool(m) or ("cc" in toks) or ("creativecommons" in raw)
    if is_cc and "by" in toks:
        return "CC-BY-SA" if "sa" in toks else "CC-BY"
    return None


# ★不用 GBIF 服务端 license 预筛(实测两个坑): (1) 重复 license 参数 → HTTP 400;
# (2) 更要命 —— occurrence 的 license(那个参数过滤的字段)≠ 照片本身的 license: 实测
# license=CC_BY_4_0 过滤出的记录, 其 media[].license 却是 cc-by-nc(iNat 照片几乎都是 NC)。
# 所以服务端过滤既报错又滤错字段。正确做法: 只按**逐图** media[].license 分类(下方 classify_license)。


# ────────────────────────────── 网络原语 ──────────────────────────────

def _get_json(url, timeout=API_TIMEOUT):
    req = urllib.request.Request(url, headers={"User-Agent": UA})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.load(r)


def download(url, path):
    """下载到 path, 校验 magic bytes 是 JPEG/PNG/WEBP(挡 404 HTML / 非图)。成功 True。
    建库机可信环境: 允许任意 https 主机(GBIF identifier 指向各家 CDN), 仅限大小 + 超时。"""
    try:
        req = urllib.request.Request(url, headers={"User-Agent": UA})
        with urllib.request.urlopen(req, timeout=DL_TIMEOUT) as r:
            data = r.read(MAX_IMG_BYTES + 1)
    except Exception:
        return False
    if len(data) > MAX_IMG_BYTES or len(data) < 500:
        return False
    sig = data[:12]
    is_img = (sig[:3] == b"\xff\xd8\xff"                      # JPEG
              or sig[:4] == b"\x89PNG"                        # PNG
              or (sig[:4] == b"RIFF" and sig[8:12] == b"WEBP"))  # WEBP
    if not is_img:
        return False
    try:
        with open(path, "wb") as f:
            f.write(data)
    except OSError:
        return False   # 磁盘满/路径竞争等: 当下载失败处理, 不炸整个 build
    return True


# ────────────────────────────── Wikimedia Commons(深挖) ──────────────────────────────

_COMMONS_API = "https://commons.wikimedia.org/w/api.php"
# 标题噪声词: 地图/分布/标本/插画/图解 —— 深挖时命中即降权(直接跳过, 减 verify 负担)。
_NOISE_TITLE = re.compile(
    r"\b(map|range|distribution|herbarium|illustration|diagram|chart|locator|"
    r"labell?ed|drawing|engraving|plate|figure|scan|specimen sheet)\b", re.I)


def wikimedia_search(name, limit=50):
    """generator=search 深挖(超出 imageingest 的 top-4)。返回 CC0/BY/SA 的 Candidate。
    标题带明显噪声词(地图/标本/插画)的直接跳过。"""
    q = {
        "action": "query", "format": "json", "maxlag": "5",
        "generator": "search", "gsrsearch": name, "gsrnamespace": "6",
        "gsrlimit": str(min(limit, 100)),
        "prop": "imageinfo", "iiprop": "url|mime|size|extmetadata", "iiurlwidth": "1024",
    }
    url = _COMMONS_API + "?" + urllib.parse.urlencode(q)
    try:
        resp = _get_json(url)
    except Exception as e:
        print("  [wikimedia] err", name, e)
        return []
    pages = (resp.get("query") or {}).get("pages") or {}
    out = []
    for page in pages.values():
        title = page.get("title", "")
        if _NOISE_TITLE.search(title):
            continue
        infos = page.get("imageinfo") or []
        if not infos:
            continue
        ii = infos[0]
        mime = (ii.get("mime") or "").lower()
        if mime in ("image/svg+xml", "image/gif", "image/tiff"):
            continue  # 矢量/动图/tiff 不是照片
        ext = ii.get("extmetadata") or {}
        code = (ext.get("License") or {}).get("value", "")
        short = (ext.get("LicenseShortName") or {}).get("value", "")
        lic = classify_license(code, short)
        if not lic:
            continue
        dl = ii.get("thumburl") or ii.get("url")
        if not dl:
            continue
        author = (ext.get("Artist") or {}).get("value", "")
        author = re.sub(r"<[^>]+>", "", author).strip()  # Artist 是 HTML, 剥标签
        out.append(Candidate("wikimedia_commons", dl, ii.get("descriptionurl", ""),
                             lic, author, ii.get("descriptionurl", "") or dl))
    return out


# ────────────────────────────── GBIF occurrence media ──────────────────────────────

_GBIF = "https://api.gbif.org/v1"


def _gbif_interpret(d, name):
    """纯函数(可单测): 从 species/match 响应决定 (taxon_key, accepted_name, specific)。
      - specific: 是否解析到种级(GENUS/HIGHERRANK → False, 泛指名 'Petunia spp.'/'x hybrida' 不深挖)。
      - accepted_name: 若 **EXACT** 匹配到 SYNONYM, 换成其接受名(Azalea calendulacea→Rhododendron
        calendulaceum; Hosta fortunei→Hosta sieboldiana), 让 Wikimedia 能按正名搜到真图。FUZZY 同义词
        有误配风险(flammea→flava), 保守不换。否则 == 原名。
    d 空 / matchType NONE → None。"""
    if not d or d.get("matchType") in (None, "NONE"):
        return None
    rank = d.get("rank")
    specific = (rank in ("SPECIES", "SUBSPECIES", "VARIETY", "FORM")
                and d.get("matchType") != "HIGHERRANK")
    key = d.get("usageKey")
    accepted = name
    if d.get("status") == "SYNONYM" and d.get("matchType") == "EXACT":
        acc = d.get("species")
        if acc:
            accepted = acc
            key = d.get("speciesKey") or d.get("acceptedUsageKey") or key
    return (key, accepted, specific)


def gbif_resolve(name):
    """species/match → (taxon_key, accepted_name, specific)。无匹配/网络失败 → None(调用方退化用原名)。"""
    url = _GBIF + "/species/match?" + urllib.parse.urlencode({"name": name})
    try:
        d = _get_json(url)
    except Exception as e:
        print("  [gbif] match err", name, e)
        return None
    return _gbif_interpret(d, name)


def gbif_media(species, limit=60):
    """按名解析 → 按 taxon key 取 media(见 gbif_media_by_key)。"""
    r = gbif_resolve(species)
    if not r or not r[0]:
        return []
    return gbif_media_by_key(r[0], limit)


def gbif_media_by_key(key, limit=60):
    """occurrence media(StillImage, HUMAN_OBSERVATION —— 排标本压制图)。逐图 license 分类,
    只留 CC0/BY/SA。GBIF media 聚合自 iNat/Observation.org/标本馆等; iNat 来的多是 NC → 会被逐图拒,
    留下的是非 NC 的真照(数量不多但正确, 质量>数量)。key 已解析(同义词已换接受 taxon)。"""
    q = [("taxonKey", str(key)), ("mediaType", "StillImage"),
         ("limit", str(min(limit, 100))), ("basisOfRecord", "HUMAN_OBSERVATION")]
    url = _GBIF + "/occurrence/search?" + urllib.parse.urlencode(q)
    try:
        d = _get_json(url)
    except Exception as e:
        print("  [gbif] media err key", key, e)
        return []
    out = []
    seen = set()
    for rec in d.get("results", []):
        for m in rec.get("media", []):
            if (m.get("type") or "").lower() not in ("stillimage", ""):
                continue
            ident = m.get("identifier")
            if not ident or ident in seen:
                continue
            lic = classify_license(m.get("license", ""), "")
            if not lic:
                continue  # 逐图兜底(预筛偶漏)
            fmt = (m.get("format") or "").lower()
            if fmt in ("image/svg+xml", "image/gif", "image/tiff"):
                continue
            seen.add(ident)
            out.append(Candidate("gbif", ident, rec.get("occurrence", "") or ident,
                                 lic, m.get("rightsHolder", ""), ident))
    return out


# ────────────────────────────── PlantNet related-images(可选, 消耗配额) ──────────────────────────────

_PLANTNET = "https://my-api.plantnet.org/v2/identify/all"


def plantnet_related(seed_img_path, want_species, api_key, nb=6):
    """用 seed 图上传 PlantNet, 若 top 匹配 == want_species(母种), 收其 related-images(种级参考)。
    ★消耗线上识别配额, 且 PlantNet 无 cultivar → 只对非品种种级株用(调用方保证)。
    返回 Candidate(license 逐图分类)。任何失败(网络/配额/不匹配)返回 []。"""
    if not api_key:
        return []
    try:
        boundary = "----ymsupp"
        with open(seed_img_path, "rb") as f:
            img = f.read()
        body = (
            f"--{boundary}\r\nContent-Disposition: form-data; name=\"organs\"\r\n\r\nauto\r\n"
            f"--{boundary}\r\nContent-Disposition: form-data; name=\"images\"; filename=\"q.jpg\"\r\n"
            f"Content-Type: image/jpeg\r\n\r\n"
        ).encode() + img + f"\r\n--{boundary}--\r\n".encode()
        qs = urllib.parse.urlencode({"api-key": api_key, "include-related-images": "true",
                                     "nb-results": "5", "lang": "en"})
        req = urllib.request.Request(_PLANTNET + "?" + qs, data=body,
                                     headers={"User-Agent": UA,
                                              "Content-Type": f"multipart/form-data; boundary={boundary}"})
        with urllib.request.urlopen(req, timeout=API_TIMEOUT) as r:
            d = json.load(r)
    except Exception as e:
        print("  [plantnet] err", e)
        return []
    results = d.get("results", [])
    if not results:
        return []
    top = results[0]
    sp = ((top.get("species") or {}).get("scientificNameWithoutAuthor") or "").strip().lower()
    if sp != (want_species or "").strip().lower():
        return []  # top 匹配不是目标母种 → seed 图本身可疑, 不收 PlantNet 图
    out = []
    for im in (top.get("images") or [])[:nb]:
        lic = classify_license(im.get("license", ""), "")
        if not lic:
            continue
        u = (im.get("url") or {})
        dl = u.get("o") or u.get("m") or u.get("s")
        if not dl:
            continue
        out.append(Candidate("plantnet", dl, im.get("url", {}).get("o", ""),
                             lic, im.get("author", ""), dl))
    return out
