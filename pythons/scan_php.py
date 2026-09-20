#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""扫描 PHP 工程中引用了目标库字段的 SQL，给出与 PostgreSQL 列名对齐的修改建议。

配合 gomysql2pg 的 dumpSchema 子命令使用：

    gomysql2pg --config x.yml dumpSchema -o schema.json
    python scan_php.py --schema schema.json --src /path/to/php

默认只出报告，不改任何文件。加 --apply 会先打印修改计划、等确认后再写，
写之前把原文件备份为 .bak。

改动分两类，置信度差别很大：

  1. 反引号标识符（高置信）——MySQL 用反引号包裹标识符，PostgreSQL 不支持反引号，
     每一处都是语法错误。这是本次迁移在 PHP 工程里最主要的问题。
     只要反引号里的词能对上 schema 里的表名/列名，就认定它是标识符并改写。

  2. 裸标识符大小写（中置信）——只有当目标列名不是全小写时才可能有问题，
     且依赖是否处于 SQL 语境，因此只报告、不自动改。
"""

# 让类型注解延迟求值：本文件里函数的定义顺序与其引用的类顺序无关，
# 否则前置定义的工具函数引用后面才定义的 Finding 会直接 NameError。
from __future__ import annotations

import argparse
import csv
import difflib
import json
import os
import re
import shutil
import subprocess
import sys
from dataclasses import dataclass, field
from pathlib import Path
from typing import Dict, List, Optional, Set, Tuple

# ────────────────────────────── schema ──────────────────────────────


class Schema:
    """目标库的表名/列名词典，用于把 SQL 里写的标识符解析成真实列名。"""

    def __init__(self, manifest: dict):
        self.identifier_case: str = manifest.get("identifier_case", "preserve")
        self.schema: str = manifest.get("schema", "")
        self.tables: Dict[str, str] = {}          # 小写名 -> 真实名
        self.columns_by_table: Dict[str, Dict[str, str]] = {}
        # 全局列名索引：小写名 -> {真实名}，用于无法确定表归属时的解析
        self._columns_global: Dict[str, Set[str]] = {}

        for t in manifest.get("tables", []):
            tname = t["name"]
            self.tables[tname.lower()] = tname
            cols = {}
            for c in t.get("columns", []):
                cname = c["name"]
                cols[cname.lower()] = cname
                self._columns_global.setdefault(cname.lower(), set()).add(cname)
            self.columns_by_table[tname.lower()] = cols

    def resolve_identifier(self, name: str) -> Tuple[Optional[str], str]:
        """把标识符解析成 schema 中的真实名。

        返回 (真实名, 判定)。判定取值：
          resolved   —— 唯一命中
          ambiguous  —— 命中多个不同大小写的真实名，无法确定
          unknown    —— schema 里没有（可能是别名、函数名或普通词）
        """
        key = name.lower()
        candidates: Set[str] = set()
        if key in self.tables:
            candidates.add(self.tables[key])
        candidates |= self._columns_global.get(key, set())

        if len(candidates) == 1:
            return next(iter(candidates)), "resolved"
        if len(candidates) > 1:
            return None, "ambiguous"
        return None, "unknown"

    def lower_names(self) -> Set[str]:
        """所有列名的小写形式，用于判断某个写法是否可能是列引用。"""
        return set(self._columns_global)

    def bare_identifier_ok(self, actual: str) -> bool:
        """裸标识符是否能正确解析到 actual。

        PostgreSQL 会把未加引号的标识符折叠成小写，所以只有当真实列名本身
        就是全小写时，裸写才能命中；混合大小写必须加双引号。
        """
        return actual == actual.lower()


# ────────────────────────────── PHP 词法 ──────────────────────────────


@dataclass
class PhpString:
    """PHP 字符串字面量在源码中的位置与内容。"""

    start: int           # 起始引号在文件中的偏移
    end: int             # 结束引号之后的位置
    content_start: int
    content_end: int
    interpolating: bool  # 双引号 / heredoc 会做变量替换
    kind: str            # 'single' / 'double' / 'heredoc' / 'nowdoc'
    line: int


_HEREDOC_OPEN = re.compile(r"<<<[ \t]*([\"']?)([A-Za-z_][A-Za-z0-9_]*)\1[ \t]*\r?\n")


def _advance(src: str, i: int, n: int) -> int:
    """跳过一个非字符串记号（注释、反引号 shell 执行、普通字符）。"""
    c = src[i]
    # 行注释：// 与 #
    if (c == "/" and i + 1 < n and src[i + 1] == "/") or c == "#":
        j = src.find("\n", i)
        return n if j < 0 else j + 1
    # 块注释
    if c == "/" and i + 1 < n and src[i + 1] == "*":
        j = src.find("*/", i + 2)
        return n if j < 0 else j + 2
    # PHP 的反引号运算符是执行 shell 命令，不是 SQL。它位于字符串之外，
    # 这里整体跳过，后面只在字符串内部找标识符反引号，两者天然不会混淆。
    if c == "`":
        j = i + 1
        while j < n:
            if src[j] == "\\":
                j += 2
                continue
            if src[j] == "`":
                return j + 1
            j += 1
        return n
    return i + 1


def lex_php_strings(src: str) -> List[PhpString]:
    """提取 PHP 源码里的全部字符串字面量。"""
    out: List[PhpString] = []
    i, n = 0, len(src)
    while i < n:
        c = src[i]

        if c in ("'", '"'):
            start = i
            j = i + 1
            while j < n:
                if src[j] == "\\":
                    j += 2
                    continue
                if src[j] == c:
                    break
                j += 1
            content_end = min(j, n)
            end = min(j + 1, n)
            out.append(PhpString(
                start=start, end=end,
                content_start=start + 1, content_end=content_end,
                interpolating=(c == '"'),
                kind="double" if c == '"' else "single",
                line=src.count("\n", 0, start) + 1,
            ))
            i = end
            continue

        # heredoc / nowdoc
        if src.startswith("<<<", i):
            m = _HEREDOC_OPEN.match(src, i)
            if m:
                tag = m.group(2)
                body_start = m.end()
                # 结束标记必须单独占一行（PHP 7.3 起允许前置缩进）
                close = re.compile(r"^[ \t]*" + re.escape(tag) + r"\b", re.M)
                cm = close.search(src, body_start)
                if cm:
                    content_end = cm.start()
                    end = cm.end()
                    out.append(PhpString(
                        start=i, end=end,
                        content_start=body_start, content_end=content_end,
                        interpolating=(m.group(1) != "'"),
                        kind="nowdoc" if m.group(1) == "'" else "heredoc",
                        line=src.count("\n", 0, i) + 1,
                    ))
                    i = end
                    continue

        i = _advance(src, i, n)

    return out


# ────────────────────────────── SQL 分析 ──────────────────────────────

# 反引号包裹的标识符：MySQL 特有，PostgreSQL 不支持
BACKTICK_IDENT = re.compile(r"`([^`\n]+)`")

# 判断一段文本是不是 SQL。裸标识符检查用它降低误报；
# 反引号检查不走这个判断——反引号里的词能对上 schema 就已经足够精确了。
_SQL_STRONG = re.compile(r"\b(SELECT|INSERT|UPDATE|DELETE|REPLACE)\b", re.I)
_SQL_WEAK = re.compile(r"\b(FROM|WHERE|JOIN|ORDER\s+BY|GROUP\s+BY|VALUES|SET|LIMIT)\b", re.I)


def looks_like_sql(text: str) -> bool:
    if _SQL_STRONG.search(text):
        return True
    return len(_SQL_WEAK.findall(text)) >= 2


# 合法的 MySQL 标识符（反引号内部）。用它挡掉正则片段和运算符类的误报——
# 典型例子是安全扫描代码里的 preg_match('/`[^`]{3,}`/')，反引号内容是 "[^"，
# 一旦被当成标识符改写，会把正则本身改坏，等于破坏安全检查。
_VALID_IDENT = re.compile(r"^[A-Za-z_-￿][A-Za-z0-9_$-￿]*$")

# PHP 正则字面量：preg_* 的参数形如 /.../ 、#...# 等，首尾同字符
_REGEX_LIKE = re.compile(r"^([/#~%!@]).*\1[imsxuADSUXJ]*$", re.S)

# PHP 数组字面量的键：'Name' => ...  —— CodeIgniter 的 insert/update 拿它当列名
_ARRAY_LITERAL_KEY = re.compile(r"""(['"])([A-Za-z_][A-Za-z0-9_]*)\1\s*=>""")
# PHP 数组取值：$row['Name']  —— 结果集的键跟着目标列名的大小写走
_ARRAY_ACCESS = re.compile(r"""\[\s*(['"])([A-Za-z_][A-Za-z0-9_]*)\1\s*\]""")

# Smarty 模板里的属性取值：{$v.Code}、{if $v.Code eq 'x'}、$obj->Code
# . 编译成 PHP 的 $v['Code']，-> 编译成对象属性 $obj->Code，两者大小写都敏感。
_SMARTY_ACCESS = re.compile(
    r"\$([A-Za-z_][A-Za-z0-9_]*)((?:->[A-Za-z_][A-Za-z0-9_]*|\.[A-Za-z0-9_]+)+)")
# 点号段允许数字开头：$part1_1.0.Name 会编译成 $part1_1[0]['Name']，
# 要求每段以字母开头会在这里直接断掉，整条链都匹配不上。
_SMARTY_SEG = re.compile(r"->([A-Za-z_][A-Za-z0-9_]*)|\.([A-Za-z0-9_]+)")
# 这些扩展名按模板处理：里面的 $var.Key 是 Smarty 语法，不是 PHP 的字符串拼接
_TEMPLATE_EXTS = {".html", ".htm", ".tpl"}

# 把列名当字符串参数传入的方法（CodeIgniter 查询构造器等）。
# 这类参数既不在 SQL 字符串里、也不是数组键，前面的检测全都覆盖不到：
#     array_column($aList, 'SN')            取某一列
#     $this->db->select('SN, Name')         查询构造器
#     $this->db->order_by('SN DESC')
# 值是「第几个参数」的集合（从 0 开始）。
_COLUMN_ARG_FUNCS = {
    "array_column": {1, 2},
    "select": {0}, "select_max": {0}, "select_min": {0},
    "select_avg": {0}, "select_sum": {0}, "distinct": {0},
    "where": {0}, "or_where": {0},
    "order_by": {0}, "group_by": {0}, "having": {0}, "or_having": {0},
    "like": {0}, "or_like": {0}, "not_like": {0},
}

# 函数名后紧跟左括号
_CALL = re.compile(r"\b([A-Za-z_][A-Za-z0-9_]*)\s*\(")
# 方法调用：$obj->foo(...) / Class::foo(...)
#
# 自定义方法内部怎么用这些字符串参数，静态分析看不到——可能拼进 SQL、
# 当成数组键、也可能压根没用（死参数）。所以这类不做白名单，一律列出来。
# 中间那段是可选的：$this->content->getUserInfoBySN( 里 content 才是模型别名，
# $obj->method( 则没有中间段。只写 (?:->|::) 会漏掉前一种写法。
_METHOD_CALL = re.compile(
    r"\$([A-Za-z_][A-Za-z0-9_]*)\s*->\s*"
    r"(?:([A-Za-z_][A-Za-z0-9_]*)\s*->\s*)?"
    r"([A-Za-z_][A-Za-z0-9_]*)\s*\(")
# 实参里的字符串字面量
_LIT = re.compile(r"""(['"])((?:[^'"\\]|\\.)*?)\1""")
# 字面量内容里的词（'a.SN'、'SN, Name DESC' 都要能拆出来）
_WORD_IN_LIT = re.compile(r"[A-Za-z_][A-Za-z0-9_]*")


def _call_arg_spans(stripped: str, open_paren: int) -> List[Tuple[int, int]]:
    """把调用的实参切成 (起, 止) 偏移。

    只在顶层逗号处切：括号嵌套和字符串里的逗号都不算分隔符，
    否则 order_by('if(a,b)') 这类会被切错。
    """
    spans: List[Tuple[int, int]] = []
    depth, start = 0, open_paren + 1
    i, n = start, len(stripped)
    while i < n:
        c = stripped[i]
        if c in ("'", '"'):
            j = i + 1
            while j < n:
                if stripped[j] == "\\":
                    j += 2
                    continue
                if stripped[j] == c:
                    break
                j += 1
            i = j + 1
            continue
        if c in "([{":
            depth += 1
        elif c in ")]}":
            if depth == 0:
                spans.append((start, i))
                return spans
            depth -= 1
        elif c == "," and depth == 0:
            spans.append((start, i))
            start = i + 1
        i += 1
    return spans


def scan_column_args(schema: Schema, src: str, path: Path,
                     stats: Optional[Dict[str, int]] = None,
                     model_map: Optional[Dict[str, Path]] = None,
                     root: Optional[Path] = None) -> List[Finding]:
    """扫描「把列名当字符串参数传进去」的调用。

    array_column($rows, 'SN') 这类不会报错——键取不到时静默返回空数组，
    比抛异常更难发现，所以必须静态找出来。

    方法调用会顺着 $this->load->model() 建立的别名跳到模型实现，
    判断该参数在函数体内到底有没有被用到（详见 param_note）。
    """
    stripped = strip_php_comments(src)
    out: List[Finding] = []
    seen: Set[Tuple[int, int]] = set()

    def check(s0: int, s1: int, label: str, extra: str = "") -> None:
        """检查一个实参范围内的字符串字面量，命中列名的记进 out。"""
        for lit in _LIT.finditer(stripped, s0, s1):
            for w in _WORD_IN_LIT.findall(lit.group(2)):
                actual, verdict = schema.resolve_identifier(w)
                if verdict != "resolved" or actual == w:
                    continue
                start = lit.start(2) + lit.group(2).index(w)
                end = start + len(w)
                if (start, end) in seen:
                    continue
                seen.add((start, end))
                out.append(Finding(
                    path=path, line=src.count("\n", 0, start) + 1,
                    kind="array_key", severity="warn", change="column_arg",
                    original=w, suggestion=actual, in_sql=True,
                    context=context_snippet(src, start, end),
                    note="%s 的参数按列名解析，目标列名为 %s%s"
                         % (label, actual, extra),
                    start=start, end=end,
                ))

    def param_note(alias: str, method: str, idx: int) -> str:
        """跳到模型实现里，看这第 idx 个参数到底有没有被用到。

        这是整条链路里唯一能确定的部分：参数在函数体内出现 0 次，
        就说明调用处传什么都不会影响结果——那种情况下这一处根本不用改。
        """
        if not model_map or root is None:
            return ""
        mf = model_map.get(alias.lower())
        if mf is None or not mf.is_file():
            return "（未能定位 %s 的实现，调用处自行判断）" % alias
        info = method_param_usage(mf, method)
        if info is None:
            return "（%s 里没找到 %s 方法）" % (rel_path(mf, root), method)
        defline, used = info
        names = list(used)
        if idx >= len(names):
            return ""
        pname, n = names[idx], used[names[idx]]
        where = "%s:%d" % (rel_path(mf, root), defline)
        if n == 0:
            return "；实现在 %s，**$%s 在函数体内未被使用**——此处传什么都不影响" % (where, pname)
        return "；实现在 %s，$%s 在函数体内被引用 %d 次" % (where, pname, n)

    # ① 已知的全局函数 / 查询构造器方法：只查指定下标的参数
    for m in _CALL.finditer(stripped):
        fn = m.group(1)
        arg_idxs = _COLUMN_ARG_FUNCS.get(fn)
        if arg_idxs is None:
            continue
        spans = _call_arg_spans(stripped, m.end() - 1)
        for idx, (s0, s1) in enumerate(spans):
            if idx in arg_idxs:
                check(s0, s1, "%s()" % fn)

    # ② 方法调用（$this->model->foo(...)）：参数一律检查，并跳到实现里看用法
    for m in _METHOD_CALL.finditer(stripped):
        receiver, mid, method = m.group(1), m.group(2), m.group(3)
        alias = mid or receiver          # $this->content->foo 时别名是 content
        spans = _call_arg_spans(stripped, m.end() - 1)
        for idx, (s0, s1) in enumerate(spans):
            check(s0, s1, "$%s->%s()" % (receiver, method),
                  param_note(alias, method, idx))

    return out


# 超全局数组的键来自外部（表单、URL、会话、Cookie），改了就断。
# 它们恰好和列名重名的情况很常见（$_POST['Name'] 对应 Name 列），必须排除。
_SUPERGLOBALS = ("$_POST", "$_GET", "$_REQUEST", "$_SESSION", "$_COOKIE",
                 "$_FILES", "$_SERVER", "$_ENV", "$GLOBALS")


def _after_superglobal(stripped: str, bracket_pos: int) -> bool:
    """判断 [ 是否属于超全局数组的链式取值。

    不能只看 [ 前面紧邻的字符：$_SESSION['bsh']['nameType'] 里，
    第二层 ['nameType'] 前面是 ] 而不是 $_SESSION，只看一层会漏掉它，
    结果就是"会话写入被改了、读取没改"，会话直接断掉。
    所以这里往前逐层跳过 [ ... ] 分组，找到链式取值的根变量再判断。
    """
    j = bracket_pos
    while j > 0 and stripped[j - 1] == "]":
        depth, k = 1, j - 2
        while k >= 0 and depth > 0:
            if stripped[k] == "]":
                depth += 1
            elif stripped[k] == "[":
                depth -= 1
            k -= 1
        j = k + 1

    for g in _SUPERGLOBALS:
        if j - len(g) >= 0 and stripped[j - len(g):j] == g:
            return True
    return False


def strip_php_comments(src: str) -> str:
    """把注释内容替换成空格（保持偏移不变），字符串字面量原样保留。

    数组键检测要在整份源码上做正则，注释里的 'Name' => 是死代码，
    不改的话报告会被注释噪音淹没（用户代码里就有这种注释掉的行）。
    """
    out = list(src)
    i, n = 0, len(src)
    while i < n:
        c = src[i]
        if c in ("'", '"'):
            j = i + 1
            while j < n:
                if src[j] == "\\":
                    j += 2
                    continue
                if src[j] == c:
                    break
                j += 1
            i = j + 1
            continue
        if (c == "/" and i + 1 < n and src[i + 1] == "/") or c == "#":
            j = src.find("\n", i)
            j = n if j < 0 else j
            for k in range(i, j):
                out[k] = " "
            i = j
            continue
        if c == "/" and i + 1 < n and src[i + 1] == "*":
            j = src.find("*/", i + 2)
            j = n if j < 0 else j + 2
            for k in range(i, j):
                if out[k] != "\n":
                    out[k] = " "
            i = j
            continue
        i += 1
    return "".join(out)


def is_plausible_identifier(name: str) -> bool:
    """反引号里的内容是否像数据库标识符。"""
    return bool(_VALID_IDENT.match(name))


def looks_like_regex(text: str) -> bool:
    """整个字符串是否像一个 PHP 正则字面量。

    preg_* 的参数就是这个形态。这类字符串里的反引号属于正则语法，
    不是 SQL 标识符，整段跳过。
    """
    return bool(_REGEX_LIKE.match(text.strip()))


def context_snippet(src: str, abs_start: int, abs_end: int) -> str:
    """命中所在的整行源码，命中处用 >>> <<< 标出。

    报告里必须带上下文：`EducationSchool` 既可能是 SQL 里的列名，
    也可能是 PHP 数组键 $val['EducationSchool']——光看标识符分不出来。
    保留整行不截断，展示时放在代码块里，不受表格宽度限制。
    """
    line_start = src.rfind("\n", 0, abs_start) + 1
    line_end = src.find("\n", abs_start)
    if line_end < 0:
        line_end = len(src)
    rel_s = abs_start - line_start
    rel_e = abs_end - line_start
    line = src[line_start:line_end]
    return (line[:rel_s] + ">>>" + line[rel_s:rel_e] + "<<<" + line[rel_e:]).strip()


def sql_ident(kind: str, name: str) -> str:
    """按 PHP 字符串种类生成 PostgreSQL 的双引号标识符。

    关键：替换文本要同时满足 SQL 和 PHP 两层语法。

      double  —— PHP 双引号字符串，裸 " 会提前结束字符串，必须写成 \\"
      heredoc —— 行为同双引号字符串，但引号不需要转义，裸 " 就是普通字符
      nowdoc / single —— 行为同单引号字符串，" 是普通字符

    漏掉这一层会直接把 PHP 源码改成语法错误。
    """
    if kind == "double":
        return '\\"%s\\"' % name
    return '"%s"' % name


@dataclass
class Finding:
    path: Path
    line: int
    kind: str        # backtick / bare_case
    severity: str    # error / warn
    original: str
    suggestion: str
    note: str
    # 变化层次，用来区分「需要核对」和「机械替换」：
    #   quote_only      只换引号形态，名字本身没变
    #   case_and_quote  引号形态 + 名称大小写都要动 —— 真正需要核对的一类
    #   name_only       只调大小写（裸标识符加引号）
    #   unknown         schema 未命中，需人工判断
    change: str = ""
    # 命中处的源码上下文（含 >>>命中<<< 标记）。
    # 只看标识符本身分不清它是 SQL 列名还是 PHP 数组键（$val['EducationSchool']），
    # 必须把所在那一行带出来才判断得了。
    context: str = ""
    in_sql: bool = False   # 所在字符串是否被判定为 SQL 语句
    start: int = -1  # 文件内绝对偏移，仅 backtick 类可自动改写
    end: int = -1


def analyse_string(
    schema: Schema, s: PhpString, text: str, path: Path, src: str, findings: List[Finding]
) -> None:
    """分析单个字符串字面量，把问题追加到 findings。

    行号按每个匹配的绝对偏移实时计算——heredoc 的 s.line 是 <<< 标记所在行，
    内容通常在其后若干行，直接沿用会定位不准。
    """

    def line_of(abs_offset: int) -> int:
        return src.count("\n", 0, abs_offset) + 1

    is_sql = looks_like_sql(text)

    # ── 1. 反引号标识符（高置信，可自动改）──
    # 整段是正则字面量时，里面的反引号属于正则语法，直接跳过。
    # 仍然记一条：万一整段 SQL 被误判成正则，就是静默漏掉，那比误报危险得多。
    if looks_like_regex(text):
        if "`" in text:
            snippet = text.strip()[:60]
            findings.append(Finding(
                path=path, line=s.line, kind="backtick",
                severity="info", change="skipped",
                original=snippet, suggestion=snippet, in_sql=False,
                context="（整段都是正则字面量）",
                note="整段疑似 PHP 正则字面量，其中的反引号已跳过不处理",
            ))
        return

    for m in BACKTICK_IDENT.finditer(text):
        raw = m.group(1)
        start = s.content_start + m.start()
        end = s.content_start + m.end()
        ctx = context_snippet(src, start, end)

        if not is_plausible_identifier(raw):
            # 内容含 [ ^ ( ) 等字符，不可能是数据库标识符，多半是正则或运算符
            # 片段。不自动改，但记进报告，万一判断错了也看得见。
            findings.append(Finding(
                path=path, line=line_of(start), kind="backtick",
                severity="info", change="skipped",
                original=m.group(0), suggestion=m.group(0),
                context=ctx, in_sql=is_sql,
                note="内容不像数据库标识符，已跳过（可能是正则或运算符片段）",
            ))
            continue
        # `db`.`tbl`.`col` 这类写法逐段判断
        resolved, verdict = schema.resolve_identifier(raw)
        if verdict == "unknown":
            # schema 里没有，可能是别名。仍然要改引号（PG 不认反引号），
            # 但保留原样大小写，避免猜错
            suggestion = sql_ident(s.kind, raw)
            note = "未在 schema 中找到，保留原大小写，请人工确认"
            change = "unknown"
        elif verdict == "ambiguous":
            suggestion = sql_ident(s.kind, raw)
            note = "schema 中有多个大小写不同的同名对象，无法确定，请人工确认"
            change = "unknown"
        else:
            suggestion = sql_ident(s.kind, resolved)
            note = "解析为 %s" % resolved
            # 名称一致 → 纯机械替换引号形态；不一致 → 才需要人工核对
            change = "quote_only" if resolved == raw else "case_and_quote"

        findings.append(Finding(
            path=path, line=line_of(start), kind="backtick", severity="error",
            original=m.group(0), suggestion=suggestion, note=note, change=change,
            context=ctx, in_sql=is_sql,
            start=start, end=end,
        ))

    # ── 2. 裸标识符大小写（中置信，只报告）──
    if not is_sql:
        return
    # 双引号/heredoc 里可能有变量插值，静态判断不可靠
    if s.interpolating and "$" in text:
        return

    for word, wstart in _iter_bare_words(text):
        actual, verdict = schema.resolve_identifier(word)
        if verdict != "resolved":
            continue
        if schema.bare_identifier_ok(actual):
            continue  # 真实列名是全小写，裸写能正确折叠，无问题
        bstart = s.content_start + wstart
        findings.append(Finding(
            path=path, line=line_of(bstart),
            kind="bare_case", severity="warn", change="name_only",
            original=word, suggestion=sql_ident(s.kind, actual),
            context=context_snippet(src, bstart, bstart + len(word)),
            in_sql=True,
            note="目标列名为 %s，未加引号会被折叠成 %s" % (actual, word.lower()),
        ))


_WORD = re.compile(r"[A-Za-z_][A-Za-z0-9_]*")
# 这些词不是列引用，直接跳过，减少噪音
_SKIP_WORDS = {
    "select", "insert", "update", "delete", "replace", "from", "where", "join",
    "left", "right", "inner", "outer", "cross", "on", "and", "or", "not", "null",
    "order", "group", "by", "having", "limit", "offset", "values", "set", "into",
    "as", "asc", "desc", "union", "all", "distinct", "like", "in", "is", "between",
    "count", "sum", "max", "min", "avg", "now", "curdate", "ifnull", "coalesce",
    "case", "when", "then", "else", "end", "cast", "convert", "concat", "table",
    "insert", "duplicate", "key", "ignore", "exists", "default", "primary",
    "database", "true", "false", "int", "varchar", "char", "text", "date",
}


def _iter_bare_words(text: str):
    """产出不在引号/反引号内的裸词及其在 text 中的偏移。"""
    i, n = 0, len(text)
    while i < n:
        c = text[i]
        if c in ("'", '"', "`"):
            j = i + 1
            while j < n:
                if text[j] == "\\":
                    j += 2
                    continue
                if text[j] == c:
                    break
                j += 1
            i = j + 1
            continue
        m = _WORD.match(text, i)
        if m:
            word = m.group(0)
            if word.lower() not in _SKIP_WORDS:
                yield word, m.start()
            i = m.end()
            continue
        i += 1


# ────────────────────────────── 主流程 ──────────────────────────────


def scan_array_keys(schema: Schema, src: str, path: Path,
                    stats: Optional[Dict[str, int]] = None) -> List[Finding]:
    """扫描 PHP 数组键与数组取值里引用列名的地方。

    两类都对应数据库列名，但都不在 SQL 字符串里，所以看不到反引号：

        'Name' => strip_tags($Name)   写入 —— CodeIgniter 的 insert/update 拿数组键当列名
        $row['Name']                  读取 —— 结果集的键跟着目标列名的大小写走

    超全局数组的键来自外部（表单/URL/会话/Cookie），名字由外部决定、不随数据库变，
    一律跳过。跳过数量记进 stats，避免"以为全覆盖了"。
    """
    stripped = strip_php_comments(src)
    out: List[Finding] = []
    seen: Set[Tuple[int, int]] = set()

    for pat in (_ARRAY_LITERAL_KEY, _ARRAY_ACCESS):
        for m in pat.finditer(stripped):
            name = m.group(2)
            actual, verdict = schema.resolve_identifier(name)
            # 大小写已经一致就不用动；schema 里没有的更不该猜
            if verdict != "resolved" or actual == name:
                continue
            # $_POST['Name'] 这类：键名由表单决定，改成小写就取不到了
            if pat is _ARRAY_ACCESS and _after_superglobal(stripped, m.start()):
                if stats is not None:
                    stats["superglobal"] = stats.get("superglobal", 0) + 1
                continue
            start, end = m.start(2), m.end(2)
            if (start, end) in seen:
                continue
            seen.add((start, end))
            out.append(Finding(
                path=path, line=src.count("\n", 0, start) + 1,
                kind="array_key", severity="warn", change="array_key",
                original=name, suggestion=actual, in_sql=False,
                context=context_snippet(src, start, end),
                note="目标列名为 %s" % actual,
                start=start, end=end,
            ))
    return out


def scan_smarty_keys(schema: Schema, src: str, path: Path,
                     stats: Optional[Dict[str, int]] = None) -> List[Finding]:
    """扫描 Smarty 模板里的属性取值。

    Smarty 把 {$v.Code} 编译成 PHP 的 $v['Code']，所以它和 $row['Name'] 是
    同一类问题，只是写法完全不同——只认 PHP 数组语法会整片漏掉。

    只在模板扩展名上启用：同样是 $var.Key，在 .php 里是字符串拼接，不是取值。
    """
    out: List[Finding] = []
    seen: Set[Tuple[int, int]] = set()

    for m in _SMARTY_ACCESS.finditer(src):
        var = m.group(1)
        # $smarty.session.xxx 读的是 PHP 会话（编译成 $_SESSION['xxx']），不是数据库
        # 结果集。会话键由应用自己维护，本来就不需要跟数据库对齐；改了反而会和
        # 写入端（$_SESSION[...] = ...）对不上，会话直接读不到值。
        if var == "smarty":
            if stats is not None:
                stats["smarty_session"] = stats.get("smarty_session", 0) + 1
            continue
        base = m.start(2)
        # 逐段检查 $v.a.b->c 里的每一段，命中就改那一段的偏移。
        # 分隔符可能是 . （数组键）或 -> （对象属性），偏移要各自算准。
        for seg_m in _SMARTY_SEG.finditer(m.group(2)):
            # 分组 1 是 -> 形式（对象属性），分组 2 是 . 形式（数组键）
            obj, arr = seg_m.group(1), seg_m.group(2)
            seg = obj if obj is not None else arr
            start = base + seg_m.start() + (2 if obj is not None else 1)
            actual, verdict = schema.resolve_identifier(seg)
            if verdict != "resolved" or actual == seg:
                continue
            end = start + len(seg)
            if (start, end) in seen:
                continue
            seen.add((start, end))
            access = ("%s->%s" % (var, seg) if obj is not None
                      else "%s['%s']" % (var, seg))
            out.append(Finding(
                path=path, line=src.count("\n", 0, start) + 1,
                kind="array_key", severity="warn", change="smarty_key",
                original=seg, suggestion=actual, in_sql=False,
                context=context_snippet(src, start, end),
                note="Smarty 编译成 %s，目标列名为 %s" % (access, actual),
                start=start, end=end,
            ))
    return out


def scan_file(schema: Schema, path: Path,
              stats: Optional[Dict[str, int]] = None,
              model_map: Optional[Dict[str, Path]] = None,
              root: Optional[Path] = None) -> List[Finding]:
    try:
        src = path.read_text(encoding="utf-8", errors="replace")
    except OSError as e:
        print("warn: cannot read %s: %s" % (path, e), file=sys.stderr)
        return []

    findings: List[Finding] = []
    for s in lex_php_strings(src):
        text = src[s.content_start:s.content_end]
        analyse_string(schema, s, text, path, src, findings)
    findings.extend(scan_array_keys(schema, src, path, stats))
    findings.extend(scan_column_args(schema, src, path, stats, model_map, root))
    if path.suffix.lower() in _TEMPLATE_EXTS:
        findings.extend(scan_smarty_keys(schema, src, path, stats))
    return findings


def collect_files(root: Path, exts: Set[str], skip_dirs: Set[str]):
    for p in sorted(root.rglob("*")):
        if not p.is_file():
            continue
        if p.suffix.lower() not in exts:
            continue
        if any(part in skip_dirs for part in p.parts):
            continue
        yield p


def php_lint(path: Path) -> Optional[str]:
    """用 php -l 做语法检查。

    返回 None 表示环境里没有 php、跳过检查；返回空串表示通过；
    返回非空字符串表示语法错误信息。
    """
    exe = shutil.which("php")
    if not exe:
        return None
    try:
        r = subprocess.run([exe, "-l", str(path)],
                           capture_output=True, text=True, timeout=60)
    except (OSError, subprocess.SubprocessError) as e:
        return "php -l 执行失败: %s" % e
    if r.returncode != 0:
        return (r.stdout + r.stderr).strip()
    return ""


def backup_path(path: Path, root: Path, backup_root: Path) -> Path:
    """备份文件的目标路径：在备份目录下镜像原目录结构。

    不放在源码旁边，是为了不污染工程目录——几十个 .bak 混在业务文件里，
    既影响代码检索，也容易被误提交。镜像结构则便于对照和整体回滚。
    """
    try:
        rel = path.relative_to(root)
    except ValueError:
        rel = Path(path.name)
    return backup_root / rel


def restore_from_backup(backup_root: Path, root: Path) -> int:
    """把备份目录里的文件整体恢复回原位置，返回恢复的文件数。"""
    if not backup_root.is_dir():
        print("备份目录不存在: %s" % backup_root, file=sys.stderr)
        return 0
    n = 0
    for p in sorted(backup_root.rglob("*")):
        if not p.is_file():
            continue
        target = root / p.relative_to(backup_root)
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(p, target)
        n += 1
    return n


def apply_findings(findings: List[Finding], kinds: Set[str],
                   root: Path, backup_root: Path) -> Tuple[int, int, int]:
    """按偏移从后往前改写，避免前面的替换影响后面的偏移。

    kinds 决定改哪几类：反引号和数组键是两个独立开关，方便分批进行、
    分批验证，出问题也好定位是哪一批引入的。
    """
    editable = [f for f in findings if f.kind in kinds and f.start >= 0]
    by_file: Dict[Path, List[Finding]] = {}
    for f in editable:
        by_file.setdefault(f.path, []).append(f)

    files_changed = replacements = reverted = 0
    for path, items in by_file.items():
        src = path.read_text(encoding="utf-8", errors="replace")
        # 同一位置只保留一条，并按偏移倒序应用，
        # 避免前面的替换改变后面匹配的偏移
        uniq: Dict[int, Finding] = {}
        for f in items:
            uniq[f.start] = f
        ordered = sorted(uniq.values(), key=lambda x: x.start, reverse=True)

        out = src
        for f in ordered:
            # 偏移对不上说明源码在这一轮之外被改过，宁可跳过也不要错改
            if out[f.start:f.end] != f.original:
                print("跳过（源码已变化）: %s:%d" % (path, f.line), file=sys.stderr)
                continue
            out = out[:f.start] + f.suggestion + out[f.end:]
            replacements += 1
        if out == src:
            continue

        dest = backup_path(path, root, backup_root)
        dest.parent.mkdir(parents=True, exist_ok=True)
        # 备份只写一次。分批改写时第二遍要保住的是「最初的样子」，
        # 不是上一遍改完的样子——否则回滚会退到中间状态。
        if not dest.exists():
            shutil.copy2(path, dest)
        path.write_text(out, encoding="utf-8")

        # 语法检查不通过就回滚。自动改写最怕的是把源码改成语法错误——
        # 这类错误未必立刻暴露，可能等到某个分支被执行时才炸，代价很高。
        lint = php_lint(path)
        if lint:
            shutil.copy2(dest, path)
            print("语法检查未通过，已回滚 %s\n%s" % (path, lint), file=sys.stderr)
            reverted += 1
            continue
        files_changed += 1

    return files_changed, replacements, reverted


# 分组顺序 = 需要人工核对的程度，从高到低
_SECTIONS = [
    ("case_and_quote", "A. SQL 里的名称大小写不一致",
     "源码里写的名字与目标库的列名不同，**需要核对**后再改"),
    ("unknown", "B. schema 未命中",
     "可能是表别名或 schema 里没有的对象，**需要人工判断**"),
    ("array_key", "C. PHP 数组键引用了列名",
     "不在 SQL 字符串里，但一样会失效：写入时框架拿数组键当列名"
     "（'Name' => ... 会生成 INSERT INTO ...(\"Name\")），"
     "读取时结果集的键跟着目标列名的大小写走（$row['Name']）。"
     "**需要人工确认**——同一个键也可能是与数据库无关的内部数组"),
    ("column_arg", "D. 列名作为字符串参数传给了函数",
     "`array_column($rows, 'SN')`、`$this->db->select('SN')` 这类。"
     "参数不在 SQL 字符串里，取不到值时**静默返回空**、不报错，比抛异常更难发现。"
     "**需要人工确认**"),
    ("smarty_key", "E. Smarty 模板属性引用了列名",
     "模板里的 `{$v.Code}` 会被 Smarty 编译成 `$v['Code']`，"
     "和 PHP 数组键是同一类问题，只是写法不同。**需要人工确认**"),
    ("name_only", "F. 裸标识符大小写",
     "未加引号会被折叠成小写，是否要改取决于 identifierCase"),
    ("quote_only", "G. 仅换引号形态",
     "名称完全一致，纯粹去掉 MySQL 反引号，机械替换，可略过"),
    ("skipped", "H. 已跳过（疑似非标识符）",
     "内容不像数据库标识符（如正则片段），工具**不会改动**它们；"
     "若发现其中有真实列名被误判，请手工处理"),
]

# 需要逐条人工核对的分组。--report-case-only 只输出这些。
_REVIEW = ("case_and_quote", "unknown", "array_key", "column_arg", "smarty_key")


# ── 模型方法调用解析（跨文件） ─────────────────────────────────────
#
# CodeIgniter 里 $this->load->model('moe/Model_content', 'content') 会把模型
# 加载成 $this->content，之后 $this->content->foo(...) 调的就是
# models/moe/Model_content.php 里的 foo()。
#
# 只看调用处无法判断参数会怎么用——getUserInfoBySN 的第三个参数传了
# " order by startTime asc"，但函数体里压根没拼进 SQL。这类必须跳到实现里看。
#
# 这里做的判断很有限但很确定：**该参数在方法体内有没有被用到**。
# 未被用到 = 死参数，调用处传什么都无关紧要。

_LOAD_MODEL = re.compile(
    r"""\$this\s*->\s*load\s*->\s*model\s*\(\s*(['"])([^'"]+)\1"""
    r"""(?:\s*,\s*(['"])([^'"]+)\3)?""")
_MODEL_CALL = re.compile(
    r"\$this\s*->\s*([A-Za-z_][A-Za-z0-9_]*)\s*->\s*([A-Za-z_][A-Za-z0-9_]*)\s*\(")
_FUNC_DEF = re.compile(r"\bfunction\s+([A-Za-z_][A-Za-z0-9_]*)\s*\(([^)]*)\)")


def build_model_map(files: List[Path], root: Path) -> Dict[str, Path]:
    """扫描所有文件，建立「模型别名 -> 模型文件」的映射。

        $this->load->model('moe/Model_content', 'content')   →  content: models/moe/Model_content.php
        $this->load->model('Model_pub')                      →  model_pub: models/Model_pub.php
    """
    out: Dict[str, Path] = {}
    for p in files:
        try:
            t = p.read_text(encoding="utf-8", errors="replace")
        except OSError:
            continue
        for m in _LOAD_MODEL.finditer(strip_php_comments(t)):
            rel, alias = m.group(2), m.group(4)
            if not alias:
                # 不给别名时 CodeIgniter 用类名小写作为属性名
                alias = Path(rel).name.lower()
            out[alias.lower()] = root / "models" / (rel + ".php")
    return out


def method_param_usage(model_file: Path, method: str
                       ) -> Optional[Tuple[int, Dict[str, int]]]:
    """找方法定义，返回 (定义所在行, {参数名: 在函数体内出现次数})。

    方法不存在或文件读不到返回 None。次数为 0 的参数就是死参数。
    """
    try:
        src = model_file.read_text(encoding="utf-8", errors="replace")
    except OSError:
        return None
    st = strip_php_comments(src)

    for m in _FUNC_DEF.finditer(st):
        if m.group(1) != method:
            continue
        params = re.findall(r"\$([A-Za-z_][A-Za-z0-9_]*)", m.group(2))
        brace = st.find("{", m.end())
        if brace < 0:
            return None
        depth = 0
        for i in range(brace, len(st)):
            if st[i] == "{":
                depth += 1
            elif st[i] == "}":
                depth -= 1
                if depth == 0:
                    body = st[brace + 1:i]
                    used = {p: len(re.findall(r"\$" + re.escape(p) + r"\b", body))
                            for p in params}
                    return src.count("\n", 0, m.start()) + 1, used
        return None
    return None


# ── 前后端命名统一 ────────────────────────────────────────────────
#
# 这一层不解决数据库问题——数据库相关的改名早已由 A~E 组处理完。
# 它解决的是「同一个字段，库里叫 enname、表单里叫 EnName」这种跨层不一致。
#
# 改动必须成组联动，漏一处就静默失效：
#   HTML name/id  ←→  JS 的 $('#X') / getElementById('X')
#   HTML name     ←→  extract() 解出的 PHP 变量名
# 会话键是纯内部的，改了没有联动风险。

_UNIFY_Q = r'["\']'


def _unify_patterns(alt: str) -> Dict[str, "re.Pattern"]:
    return {
        "HTML name/id": re.compile(r'((?:name|id)\s*=\s*' + _UNIFY_Q + r')(' + alt + r')(' + _UNIFY_Q + r')'),
        "JS id": re.compile(r"(\$\(\s*" + _UNIFY_Q + r'#)(' + alt + r')(' + _UNIFY_Q + r'\s*\))'),
        "JS getElementById": re.compile(r'(getElementById\(\s*' + _UNIFY_Q + r')(' + alt + r')(' + _UNIFY_Q + r'\s*\))'),
        "PHP 会话键": re.compile(r'(\$_SESSION\s*\[[^]]*\]\s*\[\s*' + _UNIFY_Q + r')(' + alt + r')(' + _UNIFY_Q + r'\s*\])'),
        "Smarty 会话": re.compile(r'(\$smarty\.session(?:\.|->)[A-Za-z_][A-Za-z0-9_]*(?:\.|->))(' + alt + r')\b'),
    }


def collect_unify_edits(files: List[Path], root: Path,
                        lower_cols: Set[str]) -> Dict[Path, List[Tuple[int, int, str, str]]]:
    """收集命名统一所需的替换。

    返回 {文件: [(起, 止, 原名, 新名), ...]}，按偏移倒序排好，可直接应用。

    只处理「前后端联动」的四类：HTML name/id、JS 取值、会话键、Smarty 会话。
    被赋过值的 PHP 变量不动——它们存的是值不是列名，改名对迁移无用，
    而且已有文件同时存在两种大小写的同名变量，合并会改变行为。
    """
    edits: Dict[Path, List[Tuple[int, int, str, str]]] = {}

    # 先找出候选字段名：混合大小写，且小写后能对上目标库的列
    cand: Set[str] = set()
    texts: Dict[Path, str] = {}
    for p in files:
        try:
            t = p.read_text(encoding="utf-8", errors="replace")
        except OSError:
            continue
        texts[p] = t
        for w in re.findall(r"[A-Za-z_][A-Za-z0-9_]*", t):
            if w != w.lower() and w.lower() in lower_cols:
                cand.add(w)
    if not cand:
        return {}

    alt = "|".join(sorted(cand, key=len, reverse=True))
    patterns = _unify_patterns(alt)

    for p, t in texts.items():
        st = strip_php_comments(t)
        found: List[Tuple[int, int, str, str]] = []
        seen: Set[Tuple[int, int]] = set()
        is_php = p.suffix.lower() == ".php"

        for key, rx in patterns.items():
            # JS 取值只在模板里出现；PHP 文件里没有 $('...') 这种写法
            if key.startswith("JS") and is_php:
                continue
            if key == "PHP 会话键" and not is_php:
                continue
            for m in rx.finditer(st):
                name = m.group(2)
                if name == name.lower():
                    continue
                start, end = m.start(2), m.end(2)
                if (start, end) in seen:
                    continue
                seen.add((start, end))
                found.append((start, end, name, name.lower()))

        # PHP 里来自 extract() 的表单变量：本文件从未赋值、只被使用的
        if is_php:
            used = set(re.findall(r'\$([A-Za-z_][A-Za-z0-9_]*)', st))
            for v in sorted(used):
                if v == v.lower() or v.lower() not in lower_cols:
                    continue
                if re.search(r"\$" + re.escape(v) + r"\s*(?:=[^=]|\.=|[\+\-*/%]=)", st):
                    continue  # 被赋过值 → 值变量，不动
                for m in re.finditer(r"\$" + re.escape(v) + r"\b", st):
                    start, end = m.start() + 1, m.end()
                    if (start, end) not in seen:
                        seen.add((start, end))
                        found.append((start, end, v, v.lower()))

        if found:
            edits[p] = sorted(found, key=lambda e: e[0], reverse=True)

    return edits


def apply_unify_edits(edits, root: Path, backup_root: Path,
                      assume_yes: bool = False) -> Tuple[int, int, int]:
    """直接把命名统一落盘。

    复用与其它批次一致的备份 + 语法检查 + 回滚机制。之所以不依赖 git apply：
    工程未必是 git 仓库，而且 git 那条路没有备份和 php -l 这两道保险。
    """
    by_file = {p: items for p, items in edits.items() if items}
    if not by_file:
        return 0, 0, 0

    total = sum(len(v) for v in by_file.values())
    print("=" * 72)
    print("即将统一命名（%d 个文件 / %d 处）：" % (len(by_file), total))
    print("=" * 72)
    for p in sorted(by_file, key=lambda x: rel_path(x, root)):
        print("  %-54s %d 处" % (rel_path(p, root), len(by_file[p])))
    print()
    print("原文件会备份到 %s（按原目录结构镜像）" % backup_root)
    print("回滚：给同样的参数再加 --restore")
    if shutil.which("php"):
        print("将使用 php -l 做语法检查，不通过的文件会自动回滚。")
    else:
        print("未找到 php 命令，跳过语法检查。")

    if not assume_yes:
        try:
            answer = input("确认执行？输入 yes 继续: ").strip().lower()
        except EOFError:
            answer = ""
        if answer != "yes":
            print("已取消，未修改任何文件。")
            return 0, 0, 0

    files_changed = edits_done = reverted = 0
    for p, items in sorted(by_file.items(), key=lambda kv: rel_path(kv[0], root)):
        src = p.read_text(encoding="utf-8", errors="replace")
        out = src
        for start, end, old, new in items:      # 已按偏移倒序
            if out[start:end] != old:           # 偏移对不上说明源码变过，宁可跳过
                continue
            out = out[:start] + new + out[end:]
        if out == src:
            continue

        dest = backup_path(p, root, backup_root)
        dest.parent.mkdir(parents=True, exist_ok=True)
        if not dest.exists():
            shutil.copy2(p, dest)
        p.write_text(out, encoding="utf-8")

        # 模板文件 php -l 会报错，只对 .php 做语法检查
        lint = php_lint(p) if p.suffix.lower() == ".php" else ""
        if lint:
            shutil.copy2(dest, p)
            print("语法检查未通过，已回滚 %s\n%s" % (p, lint), file=sys.stderr)
            reverted += 1
            continue
        files_changed += 1
        edits_done += len(items)

    print()
    print("完成：改写 %d 个文件 / %d 处。" % (files_changed, edits_done))
    if reverted:
        print("已回滚 %d 个文件（语法检查未通过）。" % reverted)
    print("原始版本存于 %s，--restore 可整体回滚。" % backup_root)
    print()
    print("⚠️ 这是一组联动改名，请接着做两件事：")
    print("   1. 清 Smarty 缓存：rm -rf cache/templates_c/*")
    print("   2. 跑一遍完整表单提交流程（新增 / 编辑 / 列表查询）")
    return files_changed, edits_done, reverted


def write_unify_patch(edits, root: Path, out_dir: Path) -> Tuple[int, int]:
    """把命名统一的改动渲染成 unified diff，只出补丁，不改任何源文件。"""
    out_dir.mkdir(parents=True, exist_ok=True)
    patch_path = out_dir / "unify_names.patch"
    total_files = total_edits = 0

    with patch_path.open("w", encoding="utf-8") as fh:
        for p in sorted(edits, key=lambda x: rel_path(x, root)):
            items = edits[p]
            src = p.read_text(encoding="utf-8", errors="replace")
            out = src
            for start, end, _old, new in items:      # 已按偏移倒序
                out = out[:start] + new + out[end:]
            if out == src:
                continue
            total_files += 1
            total_edits += len(items)
            rel = rel_path(p, root).replace(os.sep, "/")
            fh.writelines(difflib.unified_diff(
                src.splitlines(keepends=True), out.splitlines(keepends=True),
                fromfile="a/" + rel, tofile="b/" + rel))

    if total_files == 0:
        try:
            patch_path.unlink()
        except OSError:
            pass
    return total_files, total_edits


def rel_path(path: Path, root: Path) -> str:
    """相对工程根目录的路径。报告里用绝对路径只会占满屏，看不清重点。"""
    try:
        return str(path.relative_to(root))
    except ValueError:
        return os.path.relpath(str(path), str(root))


def fmt_ident(text: str) -> str:
    """把标识符渲染成行内代码。

    两处刻意简化，都是为了让报告好读：
      - 去掉外层的反引号：整份报告都是「反引号」主题，标识符再套一层只是噪音，
        原始形态在上面的代码块里已经能直接看到；
      - 还原 PHP 转义：双引号字符串里的 \\" 显示成 "，
        报告看的是 SQL 语义，PHP 那层转义由工具自动处理（报告头已注明）。

    内容里若还有反引号（例如被跳过的正则片段），就不做行内代码，
    免得破坏 markdown 结构。
    """
    stripped = text.replace('\\"', '"').strip("`")
    if not stripped or "`" in stripped:
        return stripped
    return "`%s`" % stripped


def write_reports(findings: List[Finding], out_dir: Path, root: Path,
                  case_only: bool = False) -> None:
    """输出报告。

    case_only 为真时只保留需要核对的两类（A/B）——
    但它们仍然要改，只是不需要逐个看。--apply 不受此参数影响。
    """
    out_dir.mkdir(parents=True, exist_ok=True)
    groups = {k: [f for f in findings if f.change == k] for k, _, _ in _SECTIONS}

    def kept(f: Finding) -> bool:
        return not case_only or f.change in _REVIEW

    shown = [f for f in findings if kept(f)]

    csv_path = out_dir / "php_schema_align.csv"
    with csv_path.open("w", newline="", encoding="utf-8-sig") as fh:
        w = csv.writer(fh)
        w.writerow(["change", "severity", "kind", "file", "line",
                    "original", "suggestion", "in_sql", "context", "note"])
        for f in sorted(shown, key=lambda x: (x.change, rel_path(x.path, root), x.line)):
            w.writerow([f.change, f.severity, f.kind, rel_path(f.path, root), f.line,
                        f.original, f.suggestion,
                        "Y" if f.in_sql else "N", f.context, f.note])

    md_path = out_dir / "php_schema_align.md"
    with md_path.open("w", encoding="utf-8") as fh:
        fh.write("# PHP 字段对齐扫描报告\n\n")
        fh.write("PostgreSQL 不支持反引号，**所有反引号标识符都必须处理，没有可以原样保留的**。\n")
        fh.write("分组按「需要人工核对的程度」排序：A/B 需要逐条看，C/D/E 可以略过。\n\n")

        fh.write("| 分组 | 数量 | 是否需逐条核对 |\n| --- | --- | --- |\n")
        for key, title, _ in _SECTIONS:
            need = "**需要**" if key in _REVIEW else "可略过"
            fh.write("| %s | %d | %s |\n" % (title, len(groups[key]), need))

        fh.write("\n代码块里 `>>>` 与 `<<<` 之间是命中的标识符；文件路径相对工程根目录。\n")
        fh.write("「→」右边是替换后的 SQL 写法，PHP 字符串内的转义由工具自动处理。\n\n")

        for key, title, desc in _SECTIONS:
            items = groups[key]
            if not items or (case_only and key not in _REVIEW):
                continue
            fh.write("---\n\n## %s（%d 处）\n\n%s\n\n" % (title, len(items), desc))

            # 按 文件 → 行 两级分组：路径只出现一次，同一行的多个命中并在一起看
            by_file: Dict[Path, Dict[int, List[Finding]]] = {}
            for f in items:
                by_file.setdefault(f.path, {}).setdefault(f.line, []).append(f)

            for path in sorted(by_file, key=lambda p: rel_path(p, root)):
                fh.write("### %s\n\n" % rel_path(path, root))
                for line in sorted(by_file[path]):
                    rows = by_file[path][line]
                    fh.write("**第 %d 行**\n\n" % line)
                    fh.write("```text\n%s\n```\n\n" % rows[0].context)
                    for f in rows:
                        fh.write("- %s → %s" % (fmt_ident(f.original),
                                                fmt_ident(f.suggestion)))
                        if f.note:
                            fh.write("（%s）" % f.note)
                        fh.write("\n")
                    fh.write("\n")

    print("报告已写入: %s" % csv_path)
    print("报告已写入: %s" % md_path)


def main() -> int:
    # Windows 控制台默认编码不是 UTF-8，中文提示会显示成乱码。
    # 老版本控制台若仍显示异常，可先执行 chcp 65001。
    for stream in (sys.stdout, sys.stderr):
        try:
            stream.reconfigure(encoding="utf-8")
        except (AttributeError, OSError):
            pass

    ap = argparse.ArgumentParser(description="扫描 PHP 工程中与目标库字段不一致的 SQL 引用")
    ap.add_argument("--schema", required=True, help="gomysql2pg dumpSchema 生成的 JSON")
    ap.add_argument("--src", required=True, help="PHP 工程根目录")
    # 模板文件常以 .html / .phtml 存放，里面可能嵌着 <?php 和 SQL，默认一并扫描
    ap.add_argument("--ext", default=".php,.phtml,.html,.htm",
                    help="扫描的扩展名，逗号分隔（默认 .php,.phtml,.html,.htm）")
    ap.add_argument("--dirs", default="",
                    help="只扫描这些子目录（相对 --src，逗号分隔），例如 models,controllers,app；"
                         "留空则扫描 --src 下的全部")
    # 默认跳过第三方库、编译产物和工具类目录：这些地方要么不含业务 SQL，
    # 要么是生成的文件（改了会被覆盖）。要扫描它们就显式写进 --dirs。
    ap.add_argument("--skip-dir",
                    default="vendor,node_modules,.git,runtime,cache,"
                            "helpers,libraries,plugin,third_party,bower_components",
                    help="跳过的目录名，逗号分隔（在未指定 --dirs 时生效）")
    ap.add_argument("--out-dir", default="php_align_report", help="报告输出目录")
    ap.add_argument("--report-case-only", action="store_true",
                    help="报告只列需要逐条核对的项（名称大小写不一致 / schema 未命中），"
                         "略过纯机械替换；注意 --apply 仍会处理全部反引号")
    ap.add_argument("--apply", action="store_true",
                    help="确认后改写源文件里的反引号（自动备份为 .bak）；默认只出报告")
    ap.add_argument("--apply-array-keys", action="store_true",
                    help="确认后改写 PHP 数组键里的大小写；与 --apply 独立，"
                         "建议先只跑 --apply、验证通过后再跑这个")
    ap.add_argument("--backup-dir", default="php_align_backup",
                    help="改写前的备份目录，按原目录结构镜像存放（默认 php_align_backup）。"
                         "不放在源码旁边，避免几十个 .bak 混进工程目录")
    ap.add_argument("--unify-names", action="store_true",
                    help="生成「前后端命名统一」补丁：HTML name/id、JS 取值、会话键、"
                         "以及 extract() 解出的表单变量，全部转小写。"
                         "只出 unify_names.patch，不改任何源文件")
    ap.add_argument("--unify-apply", action="store_true",
                    help="直接应用命名统一（自动备份 + php -l 检查，不依赖 git）。"
                         "建议先用 --unify-names 出补丁看一遍再执行这个")
    ap.add_argument("--restore", action="store_true",
                    help="把 --backup-dir 里的文件整体恢复回原位置，不扫描、不改写")
    ap.add_argument("--yes", action="store_true", help="跳过交互确认，配合 --apply 使用")
    args = ap.parse_args()

    manifest_path = Path(args.schema)
    if not manifest_path.is_file():
        print("schema 文件不存在: %s" % manifest_path, file=sys.stderr)
        return 2
    schema = Schema(json.loads(manifest_path.read_text(encoding="utf-8")))

    root = Path(args.src)
    if not root.is_dir():
        print("源码目录不存在: %s" % root, file=sys.stderr)
        return 2

    backup_root = Path(args.backup_dir)
    if args.restore:
        n = restore_from_backup(backup_root, root)
        print("已从 %s 恢复 %d 个文件" % (backup_root, n))
        return 0

    exts = {"." + e.strip().lstrip(".") for e in args.ext.split(",") if e.strip()}
    skip = {d.strip() for d in args.skip_dir.split(",") if d.strip()}
    # 备份目录若正好在工程内，扫它没有意义，而且会把备份文件也当成待改的源码
    if backup_root.is_dir() or backup_root.parent == Path("."):
        skip.add(backup_root.name)

    # --dirs 指定时只扫这些子目录，否则扫 --src 下的全部
    roots: List[Path] = []
    for d in (args.dirs.split(",") if args.dirs.strip() else []):
        d = d.strip()
        if not d:
            continue
        sub = Path(d)
        if not sub.is_absolute():
            sub = root / sub
        if not sub.is_dir():
            print("warn: 目录不存在，已跳过: %s" % sub, file=sys.stderr)
            continue
        roots.append(sub)
    if not roots:
        if args.dirs.strip():
            print("错误：--dirs 指定的目录都不存在", file=sys.stderr)
            return 2
        roots = [root]

    # --dirs 之间可能互相嵌套，按真实路径去重，避免同一文件扫两遍
    seen: Set[Path] = set()
    files: List[Path] = []
    for r in roots:
        for p in collect_files(r, exts, skip):
            rp = p.resolve()
            if rp not in seen:
                seen.add(rp)
                files.append(p)

    print("目标 schema      : %s" % schema.schema)
    print("identifierCase   : %s" % schema.identifier_case)
    print("表/视图数        : %d" % len(schema.tables))
    print("工程根目录       : %s" % root)
    if args.dirs.strip():
        print("限定子目录       : %s" % ", ".join(str(r) for r in roots))
    else:
        print("扫描范围         : 全部（未指定 --dirs，只受 --skip-dir 限制）")
    print("跳过目录名       : %s" % ", ".join(sorted(skip)))

    # 把实际扫到的顶层目录列出来。范围不透明是很容易出错的：
    # 跑完只看到"扫了 566 个文件"，根本不知道里面有没有混进第三方代码。
    tops: Dict[str, int] = {}
    for p in files:
        top = re.split(r"[\\/]", rel_path(p, root))[0]
        tops[top] = tops.get(top, 0) + 1
    print("实际扫描         : %s" % ", ".join(
        "%s(%d)" % (k, v) for k, v in sorted(tops.items(), key=lambda kv: -kv[1])))
    print()

    # 命名统一是独立的一次性任务，不参与下面的扫描/改写
    if args.unify_names or args.unify_apply:
        edits = collect_unify_edits(files, root, schema.lower_names())
        print()
        if not edits:
            print("没有需要统一的命名。")
            return 0
        if args.unify_apply:
            apply_unify_edits(edits, root, backup_root, assume_yes=args.yes)
            return 0

        nf, ne = write_unify_patch(edits, root, Path(args.out_dir))
        if nf == 0:
            print("没有需要统一的命名。")
            return 0
        print("前后端命名统一：%d 个文件 / %d 处" % (nf, ne))
        print("补丁已写入: %s" % (Path(args.out_dir) / "unify_names.patch"))
        print()
        print("**源文件没有被修改**，这是补丁。两种应用方式：")
        print("    --unify-apply   直接应用（自带备份 + php -l，不依赖 git）")
        print("    patch -p1 < 补丁  用 GNU patch（本项目不是 git 仓库，用不了 git apply）")
        print()
        print("注意这是一组联动改名：HTML 的 name/id、JS 的取值、以及 extract()")
        print("解出的 PHP 变量必须同时生效。应用后务必跑一遍表单提交流程验证。")
        return 0

    # 先建模型别名表，后面每个文件的方法调用都要用它跳到实现
    model_map = build_model_map(files, root)
    if model_map:
        print("模型别名 : %s" % ", ".join(sorted(model_map)[:8])
              + (" …" if len(model_map) > 8 else ""))
        print()

    findings: List[Finding] = []
    stats: Dict[str, int] = {}
    file_count = 0
    for p in files:
        file_count += 1
        findings.extend(scan_file(schema, p, stats, model_map, root))

    errors = [f for f in findings if f.severity == "error"]
    counts: Dict[str, int] = {}
    for f in findings:
        counts[f.change] = counts.get(f.change, 0) + 1

    def cnt(key: str) -> int:
        return counts.get(key, 0)

    print("扫描文件: %d" % file_count)
    print()
    backticks = cnt("case_and_quote") + cnt("unknown") + cnt("quote_only")
    if backticks:
        # 反引号在 PostgreSQL 里是语法错误，全部都要处理，没有例外。
        # 但只有 A/B 需要逐条核对，E 类是纯机械替换。
        print("SQL 反引号 %d 处 —— PostgreSQL 不支持反引号，全部需要处理：" % backticks)
        print("   A. 名称大小写不一致  %4d   ← 建议逐条核对" % cnt("case_and_quote"))
        print("   B. schema 未命中     %4d   ← 需人工判断" % cnt("unknown"))
        print("   E. 仅换引号形态      %4d   ← 机械替换，可略过" % cnt("quote_only"))
    if cnt("array_key"):
        print()
        print("PHP 数组键引用列名 %d 处 —— 不在 SQL 字符串里，但一样会失效：" % cnt("array_key"))
        print("   C. 写入时框架用数组键当列名，读取时结果集的键跟着列名走")
        print("      需要人工确认哪些来自结果集、哪些只是内部数组")
    if cnt("column_arg"):
        print()
        print("列名传给了函数参数 %d 处 —— 如 array_column($rows,'SN')：" % cnt("column_arg"))
        print("   D. 取不到时是静默返回空，不会报错，必须静态找")
    if cnt("smarty_key"):
        print()
        print("Smarty 模板属性 %d 处 —— {$v.Code} 会编译成 $v['Code']：" % cnt("smarty_key"))
        print("   E. 和 PHP 数组键同类，只是写法不同")
    if cnt("name_only"):
        print()
        print("裸标识符大小写 %d 处 —— 视 identifierCase 决定是否要改" % cnt("name_only"))
    # 刻意跳过的东西也要报出来，否则"没报"和"漏扫"分不清
    skipped_groups = []
    if stats.get("superglobal"):
        skipped_groups.append(("$_POST['Name'] 这类超全局数组键",
                               stats["superglobal"],
                               "键名由表单/URL 决定，改了反而取不到"))
    if stats.get("smarty_session"):
        skipped_groups.append(("$smarty.session.* 会话取值",
                               stats["smarty_session"],
                               "读的是 $_SESSION，改了会和写入端对不上"))
    if skipped_groups:
        print()
        print("刻意跳过（不是漏扫，改这些会断）：")
        for name, n, why in skipped_groups:
            print("   %-32s %4d 处 —— %s" % (name, n, why))
    print()

    if not findings:
        print("未发现需要修改的引用。")
        return 0

    write_reports(findings, Path(args.out_dir), root, case_only=args.report_case_only)

    kinds: Set[str] = set()
    if args.apply:
        kinds.add("backtick")
    if args.apply_array_keys:
        kinds.add("array_key")
    if not kinds:
        print()
        print("默认不修改任何文件。确认报告后再执行：")
        print("  --apply             改写 SQL 反引号（机械替换，风险低）")
        print("  --apply-array-keys  改写 PHP 数组键大小写（建议先跑上面那个并验证）")
        return 0

    editable = [f for f in findings if f.kind in kinds and f.start >= 0]
    by_file: Dict[Path, Dict[str, int]] = {}
    for f in editable:
        d = by_file.setdefault(f.path, {})
        d[f.kind] = d.get(f.kind, 0) + 1

    # ── 改写 ──
    print()
    print("=" * 72)
    print("即将改写：")
    print("=" * 72)
    for p, d in sorted(by_file.items(), key=lambda kv: str(kv[0])):
        parts = []
        if d.get("backtick"):
            parts.append("反引号 %d" % d["backtick"])
        if d.get("array_key"):
            parts.append("数组键 %d" % d["array_key"])
        print("  %-54s %s" % (rel_path(p, root), " / ".join(parts)))
    print("  合计 %d 个文件 / %d 处" % (len(by_file), len(editable)))
    print()
    print("原文件会备份到 %s（按原目录结构镜像，源码目录不受污染）" % backup_root)
    print("回滚：给同样的参数再加 --restore")
    if shutil.which("php"):
        print("将使用 php -l 做语法检查，不通过的文件会自动回滚。")
    else:
        print("未找到 php 命令，跳过语法检查（建议装 php-cli 后重跑，多一层保险）。")

    if not args.yes:
        try:
            answer = input("确认执行改写？输入 yes 继续: ").strip().lower()
        except EOFError:
            answer = ""
        if answer != "yes":
            print("已取消，未修改任何文件。")
            return 0

    changed, replaced, reverted = apply_findings(findings, kinds, root, backup_root)
    print()
    print("完成：改写 %d 个文件，共 %d 处。" % (changed, replaced))
    if reverted:
        print("已回滚 %d 个文件（语法检查未通过，详见上面的错误输出）。" % reverted)
    print("涉及 %d 个文件的原始版本已存于 %s" % (changed, backup_root))
    print("如需整体回滚，给同样的参数再加 --restore 即可。")
    return 1 if reverted else 0


if __name__ == "__main__":
    sys.exit(main())
