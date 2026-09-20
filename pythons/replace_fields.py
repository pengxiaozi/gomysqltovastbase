#!/usr/bin/env python3
# -*- coding: utf-8 -*-
r"""按字段清单，在指定目录里**严格大小写匹配**地替换字段名，逐处确认。

（本段是 raw string：示例里有 Windows 路径 controllers\C.php，不这样写
 Python 会把 \C 当成非法转义并告警。）

和 scan_php.py 的区别：那个是「分析工程里引用了哪些列，分类批量改」，
这个是「拿一份已知的字段清单，一处一处给你看，你按 y 才改这一处」。
适合清单已经明确、想自己掌控每一处改动的场景。

用法：

    # 清单来自 mysql_case_fields.py 的输出
    python replace_fields.py --fields mysql_case_fields.txt --src D:\项目 --dirs models,controllers

    # 只列不改，先看看会动哪些地方
    python replace_fields.py --fields mysql_case_fields.txt --src D:\项目 --dry-run

确认时的按键：

    y        替换（默认模式=该字段的全部命中；--per-match=当前这一处）
    回车/n   跳过
    a        剩余全部替换，不再询问
    q        退出（已确认的仍会替换）

默认按**字段**确认：把该字段在所有文件里的位置全列出来，确认一次全部替换。

    ══════════════════════════════════════
    字段  SN  →  sn
    共 3 处，涉及 2 个文件：
        controllers\C.php                 1 处
        models\M.php                      2 处
    ──────────────────────────────────────
        controllers\C.php:2
            $col = array_column($rows, '>>>SN<<<');
        models\M.php:2
            $sql = "select `>>>SN<<<`,`ChnName` from t";
        models\M.php:3
            $row['>>>SN<<<'] = 1;

    替换上面 3 处 SN → sn ？[y=全部替换 / 回车=跳过 / ...]:

命中位置一律用 >>> <<< 标出。加 --per-match 可改成逐处确认，
适合同一个字段在不同地方含义不同的情况。

确认的改动先攒着，最后一次性落盘——这样每个文件只备份一次、只跑一次
php -l，而且按偏移倒序应用时前面的替换不会影响后面的位置。
"""

from __future__ import annotations

import argparse
import os
import re
import shutil
import subprocess
import sys
from pathlib import Path
from typing import Dict, List, Optional, Set, Tuple

# 复用 scan_php 里的词法工具：注释剥离、路径、语法检查
sys.path.insert(0, str(Path(__file__).resolve().parent))
import scan_php as sp                                       # noqa: E402


# ── 字段清单 ──────────────────────────────────────────────────────

def load_fields(path: Path, include_tables: bool) -> List[Tuple[str, str]]:
    """解析字段清单，返回 [(原名, 新名), ...]。

    认的是 mysql_case_fields.py 生成的格式：

        原名                             -> 新名
        # 表名中含大写的（共 5 个）        ← 分段标记
        表名                             -> 表名

    表格段默认跳过——改表名影响面比改列名大得多，要显式开关。
    """
    out: List[Tuple[str, str]] = []
    seen: Set[str] = set()
    in_tables = False
    for raw in path.read_text(encoding="utf-8", errors="replace").splitlines():
        line = raw.strip()
        if not line:
            continue
        if line.startswith("#"):
            if "表名" in line and "大写" in line:
                in_tables = True
            continue
        if "->" not in line:
            continue
        if in_tables and not include_tables:
            continue
        old, new = (p.strip() for p in line.split("->", 1))
        # 只保留「确实有大小写差异」的项，其余跳过
        if not old or not new or old == new or old in seen:
            continue
        seen.add(old)
        out.append((old, new))
    return out


# ── 查找 ──────────────────────────────────────────────────────────

def find_occurrences(files: List[Path], fields: List[Tuple[str, str]]
                     ) -> Dict[str, List[Tuple[Path, int, int, int, int, str]]]:
    """找出每个字段在哪些位置出现。

    返回 {字段原名: [(文件, 行号, 起, 止, 所在行文本), ...]}。

    严格大小写匹配：用 \\b 定边界但 **不加 re.I**，所以 `SN` 只匹配 `SN`，
    不会碰 `sn` 或 `Sn`。
    """
    result: Dict[str, List[Tuple[Path, int, int, int, str]]] = {}
    pats = {old: re.compile(r"\b" + re.escape(old) + r"\b") for old, _ in fields}
    texts: Dict[Path, str] = {}
    stripped: Dict[Path, str] = {}

    for p in files:
        try:
            t = p.read_text(encoding="utf-8", errors="replace")
        except OSError:
            continue
        texts[p] = t
        stripped[p] = sp.strip_php_comments(t)

    # 同一个位置只记一次：普通正则 \bName\b 也会命中 {$v.Name} 里的 Name，
    # 不去重的话报告里每处会出现两遍，替换时也会重复计算
    seen: Set[Tuple[Path, int, int]] = set()

    def record(old: str, p: Path, start: int, end: int) -> None:
        key = (p, start, end)
        if key in seen:
            return
        seen.add(key)
        t = texts[p]
        line = t.count("\n", 0, start) + 1
        ls = t.rfind("\n", 0, start) + 1
        le = t.find("\n", start)
        le = len(t) if le < 0 else le
        # 同时留下行首偏移，展示时才能把命中位置标出来
        result.setdefault(old, []).append((p, line, start, end, ls, t[ls:le]))

    for old, pat in pats.items():
        for p, st in stripped.items():
            # ① 普通匹配：SQL 片段、PHP 变量、数组键里的字段名
            for m in pat.finditer(st):
                record(old, p, m.start(), m.end())

            # ② Smarty 模板：{$v.Name}、{$v->Name}、{$v.0.Name}
            # 点号/箭头后面那一段才是键名，只替换那一段，别碰前面的变量名。
            if p.suffix.lower() not in sp._TEMPLATE_EXTS:
                continue
            for m in sp._SMARTY_ACCESS.finditer(st):
                base = m.start(2)
                for seg_m in sp._SMARTY_SEG.finditer(m.group(2)):
                    arr, obj = seg_m.group(1), seg_m.group(2)
                    seg = arr if arr is not None else obj
                    if seg != old:          # 严格大小写，且必须整段相等
                        continue
                    start = base + seg_m.start() + (2 if arr is not None else 1)
                    record(old, p, start, start + len(seg))

    return result


# ── 展示 ──────────────────────────────────────────────────────────

def write_dryrun_report(hits, fields_map: Dict[str, str], files, root: Path,
                        out_dir: Path, fields_path: Path, limit: int = 0) -> Path:
    """把全部命中写成 markdown。

    三千多处命中打到终端是没法看的——终端留摘要，明细进文件。
    文件按「字段」分组，每个字段下面按文件列出全部位置。
    """
    out_dir.mkdir(parents=True, exist_ok=True)
    path = out_dir / "replace_fields_report.md"

    # 行级索引：一行里所有字段的命中位置。展示时整行一起标，
    # 免得只标当前字段、其余看起来像"没被处理"。
    line_spans: Dict[Tuple[Path, int], List[Tuple[int, int]]] = {}
    for items in hits.values():
        for p, ln, s, e, _ls, _t in items:
            line_spans.setdefault((p, ln), []).append((s, e))

    total = sum(len(v) for v in hits.values())
    with path.open("w", encoding="utf-8") as fh:
        fh.write("# 字段替换清单\n\n")
        fh.write("- 清单来源：`%s`（%d 个字段）\n" % (fields_path.name, len(fields_map)))
        fh.write("- 扫描目录：`%s`\n" % root)
        fh.write("- 扫描文件：%d 个\n" % len(files))
        fh.write("- 命中：**%d 个字段，共 %d 处**\n\n" % (len(hits), total))

        fh.write("> 这是 `--dry-run` 的结果，**没有修改任何文件**。\n")
        fh.write("> 去掉 `--dry-run` 重跑即可逐字段确认替换。\n\n")

        fh.write("| 字段 | 改为 | 处数 | 文件数 |\n|---|---|---|---|\n")
        for old in sorted(hits, key=lambda k: -len(hits[k])):
            nf = len({p for p, *_ in hits[old]})
            fh.write("| `%s` | `%s` | %d | %d |\n"
                     % (old, fields_map[old], len(hits[old]), nf))
        fh.write("\n---\n\n")

        for old in sorted(hits, key=lambda k: -len(hits[k])):
            items = hits[old]
            by_file: Dict[Path, list] = {}
            for h in items:
                by_file.setdefault(h[0], []).append(h)
            fh.write("## %s → %s（%d 处，%d 个文件）\n\n"
                     % (old, fields_map[old], len(items), len(by_file)))
            for p in sorted(by_file, key=lambda x: sp.rel_path(x, root)):
                fh.write("### %s\n\n" % sp.rel_path(p, root))
                shown = 0
                for _p, ln, s, e, ls, text in by_file[p]:
                    if limit and shown >= limit:
                        fh.write("…… 另有 %d 处未列出\n\n" % (len(by_file[p]) - limit))
                        break
                    # 整行标注：这行里所有字段的命中都标出来，
                    # 免得只看到当前字段、以为别的没被处理
                    fh.write("**第 %d 行**\n\n```text\n%s\n```\n\n"
                             % (ln, marked_all(text, ls, line_spans.get((p, ln), []))))
                    shown += 1
    return path


def marked_all(text: str, ls: int, spans: List[Tuple[int, int]],
               width: int = 220) -> str:
    """把一行里**所有**命中都标出来，不只是当前字段的。

    一行里常常同时有多个字段：<{if $where.SubjectTypeC == $v.Code}>。
    只标当前那个，其余看起来就像"没被处理"——会让人以为漏扫了。
    全部标出来，完整画面一眼可见。
    """
    if not spans:
        return " ".join(text.split())
    spans = sorted(set(spans))
    if len(text) <= width:
        s0, e0 = 0, len(text)
    else:
        mid = (spans[0][0] - ls + spans[-1][1] - ls) // 2
        s0 = max(0, mid - width // 2)
        e0 = min(len(text), mid + width // 2)

    out, cur = [], s0
    for a, b in spans:
        a, b = a - ls, b - ls
        if b <= s0 or a >= e0:
            continue
        a, b = max(a, s0), min(b, e0)
        if cur < a:
            out.append(text[cur:a])
        out.append(">>>" + text[a:b] + "<<<")
        cur = b
    if cur < e0:
        out.append(text[cur:e0])
    seg = " ".join("".join(out).split())
    return ("…" if s0 > 0 else "") + seg + ("…" if e0 < len(text) else "")


def marked(text: str, ls: int, s: int, e: int, width: int = 150) -> str:
    """把命中位置用 >>> <<< 标出来。

    行太长时以命中为中心截断——不然命中可能落在 150 字符之外，
    看到的内容和实际要改的位置对不上。
    """
    a, b = s - ls, e - ls
    if len(text) <= width:
        s0, e0 = 0, len(text)
    else:
        s0 = max(0, a - width // 2)
        e0 = min(len(text), b + width // 2)
    seg = text[s0:a] + ">>>" + text[a:b] + "<<<" + text[b:e0]
    return ("…" if s0 > 0 else "") + " ".join(seg.split()) + ("…" if e0 < len(text) else "")


def show_field(old: str, new: str, hits, root: Path, limit: int = 0) -> None:
    """列出该字段的全部命中位置。

    默认不截断——确认一次就替换这个字段的所有处，那就得让你看到所有处，
    否则等于让你在一份不完整的清单上签字。--limit 可以主动限长。
    """
    by_file: Dict[Path, int] = {}
    for p, _ln, _s, _e, _ls, _t in hits:
        by_file[p] = by_file.get(p, 0) + 1

    print("═" * 78)
    print("字段  %s  →  %s" % (old, new))
    print("共 %d 处，涉及 %d 个文件：" % (len(hits), len(by_file)))
    for p, n in sorted(by_file.items(), key=lambda kv: sp.rel_path(kv[0], root)):
        print("    %-52s %d 处" % (sp.rel_path(p, root), n))
    print("─" * 78)

    for i, (p, ln, s, e, ls, text) in enumerate(hits):
        if limit and i >= limit:
            print("    …… 另有 %d 处未显示（--limit %d）" % (len(hits) - limit, limit))
            break
        print("    %s:%d" % (sp.rel_path(p, root), ln))
        print("        %s" % marked(text, ls, s, e))
    print()


# ── 应用 ──────────────────────────────────────────────────────────

def php_lint(path: Path) -> Optional[str]:
    exe = shutil.which("php")
    if not exe:
        return None
    try:
        r = subprocess.run([exe, "-l", str(path)],
                           capture_output=True, text=True, timeout=60)
    except (OSError, subprocess.SubprocessError) as e:
        return "php -l 执行失败: %s" % e
    return "" if r.returncode == 0 else (r.stdout + r.stderr).strip()


def apply_edits(edits: List[Tuple[Path, int, int, str]], root: Path,
                backup_root: Path) -> Tuple[int, int, int]:
    """把确认过的改动一次性落盘，参数是 [(文件, 起, 止, 新文本), ...]。

    先攒齐再统一写入，而不是确认一处写一处：
      - 按 (文件 → 偏移倒序) 应用，前面的替换不会改变后面的偏移
      - 每个文件只备份一次、只做一次 php -l
    """
    per_file: Dict[Path, List[Tuple[int, int, str]]] = {}
    for p, s, e, new in edits:
        per_file.setdefault(p, []).append((s, e, new))

    changed = replaced = reverted = 0
    for p in sorted(per_file, key=lambda x: sp.rel_path(x, root)):
        items = sorted(per_file[p], key=lambda x: x[0], reverse=True)
        src = p.read_text(encoding="utf-8", errors="replace")
        out = src
        n = 0
        for s, e, new in items:
            out = out[:s] + new + out[e:]
            n += 1
        if out == src:
            continue

        dest = sp.backup_path(p, root, backup_root)
        dest.parent.mkdir(parents=True, exist_ok=True)
        if not dest.exists():
            shutil.copy2(p, dest)
        p.write_text(out, encoding="utf-8")

        lint = php_lint(p) if p.suffix.lower() == ".php" else ""
        if lint:
            shutil.copy2(dest, p)
            print("语法检查未通过，已回滚 %s\n%s" % (p, lint), file=sys.stderr)
            reverted += 1
            continue
        changed += 1
        replaced += n
    return changed, replaced, reverted


# ── 主流程 ────────────────────────────────────────────────────────

def collect_files(root: Path, exts: Set[str], skip: Set[str],
                  dirs: List[str]) -> List[Path]:
    roots: List[Path] = []
    for d in dirs:
        d = d.strip()
        if not d:
            continue
        sub = Path(d)
        if not sub.is_absolute():
            sub = root / sub
        if sub.is_dir():
            roots.append(sub)
        else:
            print("warn: 目录不存在，已跳过: %s" % sub, file=sys.stderr)
    if not roots:
        roots = [root]

    seen: Set[Path] = set()
    out: List[Path] = []
    for r in roots:
        for p in sorted(r.rglob("*")):
            if not p.is_file() or p.suffix.lower() not in exts:
                continue
            if any(part in skip for part in p.parts):
                continue
            rp = p.resolve()
            if rp not in seen:
                seen.add(rp)
                out.append(p)
    return out


def main() -> int:
    for stream in (sys.stdout, sys.stderr):
        try:
            stream.reconfigure(encoding="utf-8")
        except (AttributeError, OSError):
            pass

    ap = argparse.ArgumentParser(
        description="按字段清单严格大小写匹配地替换，逐项确认")
    ap.add_argument("--fields", default="mysql_case_fields.txt",
                    help="字段清单，格式为『原名 -> 新名』")
    ap.add_argument("--src", required=True, help="要替换的目录")
    ap.add_argument("--dirs", default="",
                    help="只处理这些子目录（相对 --src，逗号分隔）；留空处理全部")
    ap.add_argument("--ext", default=".php,.phtml,.html,.htm",
                    help="处理的扩展名")
    ap.add_argument("--skip-dir", default="vendor,node_modules,.git,runtime,cache,"
                                          "helpers,libraries,plugin,third_party",
                    help="跳过的目录名")
    ap.add_argument("--include-tables", action="store_true",
                    help="连表名一起替换（默认只处理列名，改表名影响面大得多）")
    ap.add_argument("--backup-dir", default="php_align_backup",
                    help="备份目录，按原目录结构镜像")
    ap.add_argument("--dry-run", action="store_true",
                    help="只列出会改哪些地方，不做任何确认和替换；"
                         "同时把明细写进 replace_fields_report.md")
    ap.add_argument("--out-dir", default="php_align_report",
                    help="dry-run 报告输出目录（默认 php_align_report）")
    ap.add_argument("--per-match", action="store_true",
                    help="改成逐处确认（默认是逐字段：列出该字段在所有文件里的"
                         "位置，确认一次全部替换）。适合同一个字段在不同地方"
                         "含义不同的情况")
    ap.add_argument("--limit", type=int, default=0,
                    help="逐字段模式下每个字段最多显示多少处（0=全部显示，默认）")
    ap.add_argument("--all", action="store_true",
                    help="跳过逐项确认，全部替换（慎用）")
    args = ap.parse_args()

    fields_path = Path(args.fields)
    if not fields_path.is_file():
        print("字段清单不存在: %s" % fields_path, file=sys.stderr)
        return 2
    fields = load_fields(fields_path, args.include_tables)
    if not fields:
        print("清单里没有可用的条目。")
        return 0

    root = Path(args.src)
    if not root.is_dir():
        print("目录不存在: %s" % root, file=sys.stderr)
        return 2

    exts = {"." + e.strip().lstrip(".") for e in args.ext.split(",") if e.strip()}
    skip = {d.strip() for d in args.skip_dir.split(",") if d.strip()}
    files = collect_files(root, exts, skip,
                          args.dirs.split(",") if args.dirs.strip() else [])

    print("字段清单 : %s（%d 个字段）" % (fields_path, len(fields)))
    print("目标目录 : %s" % root)
    print("扫描文件 : %d" % len(files))
    print()

    hits = find_occurrences(files, fields)
    if not hits:
        # 空结果最难排查——没有任何报错，看着就像"确实没有要改的"。
        # 所以把扫描范围和清单样本都摊开，让问题一眼可见。
        print("清单里的字段一个都没在这批文件里出现。")
        print()
        print("排查提示")
        print("─" * 62)
        print("  扫描根目录 : %s" % root)
        print("  文件类型   : %s" % ", ".join(sorted(exts)))
        print("  实际扫到   : %d 个文件" % len(files))

        tops: Dict[str, int] = {}
        for p in files:
            top = re.split(r"[\\/]", sp.rel_path(p, root))[0]
            tops[top] = tops.get(top, 0) + 1
        if tops:
            print("  目录分布   : %s"
                  % ", ".join("%s(%d)" % kv
                              for kv in sorted(tops.items(), key=lambda kv: -kv[1])))
        print("  清单样本   : %s" % ", ".join(f[0] for f in fields[:6]))
        print()
        print("  常见原因：")
        print("    1. --src 指到了子目录。SQL 通常写在 models/ 里，不在 controllers/；")
        print("       应该把 --src 指到应用根目录，再用 --dirs models,controllers 圈范围")
        print("    2. 清单和这个应用对不上（来自另一个数据库 / 另一个站点）")
        print("    3. 这个应用确实不直接引用这些表")
        return 0

    fields_map = dict(fields)

    total_hits = sum(len(v) for v in hits.values())
    print("命中 %d 个字段，共 %d 处" % (len(hits), total_hits))
    print()

    if args.dry_run:
        # 三千多处命中全打到终端根本没法看：终端只给「按字段汇总」，
        # 明细写进文件，用编辑器翻。
        report = write_dryrun_report(hits, fields_map, files, root,
                                     Path(args.out_dir), fields_path, args.limit)
        print("按字段汇总：")
        for old in sorted(hits, key=lambda k: -len(hits[k]))[:25]:
            nf = len({p for p, *_ in hits[old]})
            print("    %-26s → %-26s %5d 处 / %d 个文件"
                  % (old, fields_map[old], len(hits[old]), nf))
        if len(hits) > 25:
            print("    …… 另有 %d 个字段" % (len(hits) - 25))
        print()
        print("明细已写入: %s" % report)
        print("这是 --dry-run，没有做任何修改。")
        return 0

    backup_root = Path(args.backup_dir)

    edits: List[Tuple[Path, int, int, str]] = []
    all_rest = args.all

    if args.per_match:
        # 细粒度模式：一处一处过。适合同一个字段在不同地方含义不同的情况。
        # 按 (文件, 行号) 排序，读文件本来就是自上而下的。
        flat: List[Tuple[str, Path, int, int, int, int, str]] = []
        for old, hits_list in hits.items():
            for p, ln, s, e, ls, text in hits_list:
                flat.append((old, p, ln, s, e, ls, text))
        flat.sort(key=lambda x: (sp.rel_path(x[1], root), x[2], x[3]))

        total = len(flat)
        for i, (old, p, ln, s, e, ls, text) in enumerate(flat, 1):
            new = fields_map[old]
            if not all_rest:
                print("[%d/%d] %s:%d" % (i, total, sp.rel_path(p, root), ln))
                print("    %s" % marked(text, ls, s, e))
                print("    %s  →  %s" % (old, new))
                try:
                    answer = input("    替换这一处？[y=替换 / 回车=跳过 / a=剩余全部 / q=退出]: ").strip().lower()
                except EOFError:
                    answer = ""
                print()
                if answer == "q":
                    break
                if answer == "a":
                    all_rest = True
                elif answer != "y":
                    continue
            edits.append((p, s, e, new))
    else:
        # 默认模式：按字段确认。先把该字段在所有文件里的位置全列出来，
        # 确认一次，这个字段的全部命中一起替换。
        # 命中多的先看——影响面大的早暴露，便于及早发现清单本身有问题。
        for old in sorted(hits, key=lambda k: -len(hits[k])):
            new = fields_map[old]
            if not all_rest:
                show_field(old, new, hits[old], root, limit=args.limit)
                try:
                    answer = input(
                        "替换上面 %d 处 %s → %s ？[y=全部替换 / 回车=跳过 / a=剩余全部 / q=退出]: "
                        % (len(hits[old]), old, new)).strip().lower()
                except EOFError:
                    answer = ""
                print()
                if answer == "q":
                    break
                if answer == "a":
                    all_rest = True
                elif answer != "y":
                    continue
            for p, _ln, s, e, _ls, _t in hits[old]:
                edits.append((p, s, e, new))

    if not edits:
        print("没有确认任何位置，未修改文件。")
        return 0

    print("将替换 %d 处，涉及 %d 个文件" % (len(edits), len({p for p, _, _, _ in edits})))
    print("原文件会备份到 %s（按原目录结构镜像）" % backup_root)
    print("回滚：把备份目录里的文件拷回原位即可")
    print()

    changed, replaced, reverted = apply_edits(edits, root, backup_root)
    print()
    print("完成：改写 %d 个文件 / %d 处。" % (changed, replaced))
    if reverted:
        print("已回滚 %d 个文件（语法检查未通过）。" % reverted)
    return 0


if __name__ == "__main__":
    sys.exit(main())
