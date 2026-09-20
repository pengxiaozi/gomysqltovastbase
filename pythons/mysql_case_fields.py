#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""列出 MySQL 源库里所有含大写字母的表名 / 列名。

用途：**迁移前**先摸清源库哪些字段是混合大小写的，用来评估 identifierCase
的选择和工作量。不需要先跑迁移，也不需要连目标库。

用法：

    # 直接读 gomysql2pg 的配置（只取 src 段）
    python mysql_case_fields.py --config ..\\..\\example.yml

    # 或手动指定连接参数
    python mysql_case_fields.py -H 127.0.0.1 -P 3306 -u root -p 123456 -d mydb

    # 只看列名，不看表名
    python mysql_case_fields.py --config example.yml --columns-only

依赖 MySQL 驱动，任选其一：

    pip install pymysql
    pip install mysql-connector-python

输出：
    mysql_case_fields.csv   —— 表名 / 列名 / 类型 / 序号
    mysql_case_fields.txt   —— 去重后的字段名清单（可直接拿去搜代码）
"""

from __future__ import annotations

import argparse
import csv
import re
import sys
from pathlib import Path
from typing import Dict, List, Optional, Tuple

# ── 配置读取 ──────────────────────────────────────────────────────


def load_src_from_yml(path: Path) -> Dict[str, str]:
    """从 gomysql2pg 的 yml 里读 src 段。

    只解析「src: 下两层 key: value」这种固定结构，不引 PyYAML——
    目标环境未必装了它，为一份格式已知的配置拉一个依赖不划算。
    """
    out: Dict[str, str] = {}
    in_src = False
    for raw in path.read_text(encoding="utf-8", errors="replace").splitlines():
        # 行尾注释：# 前面有空白才算注释（密码里可能带 #）
        line = re.sub(r"\s+#.*$", "", raw).rstrip()
        if not line.strip():
            continue
        if not line.startswith((" ", "\t")):        # 顶层的 key:
            in_src = line.strip().startswith("src:")
            continue
        if not in_src or ":" not in line:
            continue
        k, v = line.split(":", 1)
        out[k.strip()] = v.strip().strip('"').strip("'")
    return out


# ── 连接 ──────────────────────────────────────────────────────────


def connect(host: str, port: int, user: str, password: str, database: str):
    """按可用驱动建立连接。两个都装了就优先 pymysql（更轻）。"""
    try:
        import pymysql                                    # type: ignore
    except ImportError:
        pymysql = None

    if pymysql is not None:
        return pymysql.connect(host=host, port=port, user=user,
                               password=password, database=database,
                               charset="utf8mb4")

    try:
        import mysql.connector                            # type: ignore
    except ImportError:
        print("错误：没有可用的 MySQL 驱动。任选其一安装：", file=sys.stderr)
        print("    pip install pymysql", file=sys.stderr)
        print("    pip install mysql-connector-python", file=sys.stderr)
        sys.exit(2)

    return mysql.connector.connect(host=host, port=port, user=user,
                                   password=password, database=database,
                                   charset="utf8mb4")


# ── 查询 ──────────────────────────────────────────────────────────

# 用 CAST ... AS BINARY 强制二进制比较。
# MySQL 默认排序规则不区分大小写，直接写 column_name <> lower(column_name)
# 恒为假，一个都查不出来。
_COLUMNS_SQL = """
SELECT c.TABLE_NAME, c.COLUMN_NAME, c.DATA_TYPE, c.ORDINAL_POSITION
FROM information_schema.COLUMNS c
JOIN information_schema.TABLES t
  ON t.TABLE_SCHEMA = c.TABLE_SCHEMA AND t.TABLE_NAME = c.TABLE_NAME
WHERE c.TABLE_SCHEMA = %s
  AND t.TABLE_TYPE = 'BASE TABLE'
  AND CAST(c.COLUMN_NAME AS BINARY) <> CAST(LOWER(c.COLUMN_NAME) AS BINARY)
ORDER BY c.TABLE_NAME, c.ORDINAL_POSITION
"""

_TABLES_SQL = """
SELECT TABLE_NAME, TABLE_ROWS
FROM information_schema.TABLES
WHERE TABLE_SCHEMA = %s
  AND TABLE_TYPE = 'BASE TABLE'
  AND CAST(TABLE_NAME AS BINARY) <> CAST(LOWER(TABLE_NAME) AS BINARY)
ORDER BY TABLE_NAME
"""


def fetch(conn, sql: str, database: str) -> List[Tuple]:
    cur = conn.cursor()
    try:
        cur.execute(sql, (database,))
        return list(cur.fetchall())
    finally:
        cur.close()


# ── 输出 ──────────────────────────────────────────────────────────


def write_reports(cols: List[Tuple], tabs: List[Tuple], out_dir: Path,
                  database: str) -> None:
    out_dir.mkdir(parents=True, exist_ok=True)

    csv_path = out_dir / "mysql_case_fields.csv"
    with csv_path.open("w", newline="", encoding="utf-8-sig") as fh:
        w = csv.writer(fh)
        w.writerow(["表名", "列名", "类型", "序号", "小写形式"])
        for t, c, dtype, pos in cols:
            w.writerow([t, c, dtype, pos, c.lower()])

    txt_path = out_dir / "mysql_case_fields.txt"
    # 去重后按名字排序：这份清单可以直接拿去 grep 代码
    names = sorted({c for _, c, _, _ in cols})
    with txt_path.open("w", encoding="utf-8") as fh:
        fh.write("# 源库 %s 中含大写字母的列名（去重，共 %d 个）\n" % (database, len(names)))
        fh.write("# 迁移到 identifierCase: lower 后，这些名字都会变成小写\n\n")
        for n in names:
            fh.write("%-32s -> %s\n" % (n, n.lower()))
        if tabs:
            fh.write("\n# 表名中含大写的（共 %d 个）\n\n" % len(tabs))
            for t, _rows in tabs:
                fh.write("%-32s -> %s\n" % (t, t.lower()))

    print("明细已写入: %s" % csv_path)
    print("清单已写入: %s" % txt_path)


def main() -> int:
    for stream in (sys.stdout, sys.stderr):
        try:
            stream.reconfigure(encoding="utf-8")
        except (AttributeError, OSError):
            pass

    ap = argparse.ArgumentParser(
        description="列出 MySQL 源库里含大写字母的表名 / 列名")
    ap.add_argument("--config", help="gomysql2pg 的 yml，只读取其中的 src 段")
    ap.add_argument("-H", "--host", default="", help="MySQL 主机")
    ap.add_argument("-P", "--port", type=int, default=3306, help="MySQL 端口")
    ap.add_argument("-u", "--user", default="", help="用户名")
    ap.add_argument("-p", "--password", default="", help="密码")
    ap.add_argument("-d", "--database", default="", help="库名")
    ap.add_argument("--columns-only", action="store_true", help="只查列名，不查表名")
    ap.add_argument("--out-dir", default=".", help="报告输出目录（默认当前目录）")
    args = ap.parse_args()

    host, port = args.host, args.port
    user, password, database = args.user, args.password, args.database

    if args.config:
        cfg_path = Path(args.config)
        if not cfg_path.is_file():
            print("配置文件不存在: %s" % cfg_path, file=sys.stderr)
            return 2
        src = load_src_from_yml(cfg_path)
        host = host or src.get("host", "")
        user = user or src.get("username", "")
        password = password or src.get("password", "")
        database = database or src.get("database", "")
        try:
            port = int(src.get("port") or port)
        except ValueError:
            pass
        print("从 %s 读取 src 段" % cfg_path)

    if not (host and user and database):
        print("错误：缺少连接信息。用 --config 指定 yml，"
              "或补上 -H/-u/-d 参数", file=sys.stderr)
        return 2

    print("连接 MySQL %s:%d/%s (user=%s)" % (host, port, database, user))
    try:
        conn = connect(host, port, user, password, database)
    except Exception as e:                                  # noqa: BLE001
        print("连接失败: %s" % e, file=sys.stderr)
        return 1

    try:
        cols = fetch(conn, _COLUMNS_SQL, database)
        tabs = [] if args.columns_only else fetch(conn, _TABLES_SQL, database)
    finally:
        conn.close()

    print()
    if not cols and not tabs:
        print("源库里没有含大写字母的表名或列名——全部已是小写。")
        print("（identifierCase 选 lower 时，PHP 侧不需要为大小写做任何改动）")
        return 0

    tables_affected = len({t for t, _, _, _ in cols})
    distinct = sorted({c for _, c, _, _ in cols})
    print("含大写字母的列: %d 处，分布在 %d 张表，去重后 %d 个名字"
          % (len(cols), tables_affected, len(distinct)))
    if tabs:
        print("含大写字母的表: %d 个" % len(tabs))
    print()

    # 控制台只列去重后的名字，明细进 CSV
    for n in distinct[:40]:
        print("  %-32s -> %s" % (n, n.lower()))
    if len(distinct) > 40:
        print("  ... 另有 %d 个，见清单文件" % (len(distinct) - 40))

    write_reports(cols, tabs, Path(args.out_dir), database)
    return 0


if __name__ == "__main__":
    sys.exit(main())
