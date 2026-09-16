# 变更记录

## v0.3.1 本地修复版

基于实际迁移运行中暴露的建表失败与数据迁移失败问题修复。

修复前基线：`tableCreateFailed.log` 中 **15 张表全部建表失败**，数据迁移因
「目标表不存在」被整段跳过（`cmd/root.go` 的 `tableNotExist` 分支）。

---

### 一、类型映射缺陷（建表报 `42601 syntax error`）

#### 1. MySQL `enum` / `set` 类型未转换

新增类型映射分支，统一转换为 `varchar(255)`。

PostgreSQL 没有内联的 `enum` 列类型（枚举需先 `CREATE TYPE` 再引用），
原代码的兜底分支把 `enum` 原样透传，语句无法解析。

```
修复前: "keywords" enum null
修复后: "keywords" varchar(255) null
```

#### 2. `timestamp` / `datetime` 默认值未加引号

新增 `quoteDefault()`，按列类型决定默认值是否需要单引号包裹。

MySQL 的 `information_schema.COLUMNS.column_default` 返回的是**裸值**，原代码
只对 `varchar` / `char` 补引号，时间类型直接输出：

```sql
-- PG 把 1990-02-02 11:00:00 当算术表达式解析（1990-02-02 = 1986），
-- 再撞上 "11" 报 syntax error at or near "11"
"createtime" timestamp not null default 1990-02-02 11:00:00
```

```
修复后: "createtime" timestamp not null default '1990-02-02 11:00:00'
```

表达式类默认值（`CURRENT_TIMESTAMP` 及其精度变体、`CURRENT_DATE`、
`CURRENT_TIME`、`LOCALTIME`、`LOCALTIMESTAMP`、`NULL`、`TRUE`、`FALSE`）
走白名单原样输出，否则会被引号包裹而退化成字符串字面量。

值内的单引号按 SQL 标准双写转义（`O'Brien` → `'O''Brien'`）。

#### 3. `enum` 默认值未加引号

同由 `quoteDefault()` 处理。MySQL 把 enum 默认值存成裸标识符：

```
修复前: "status" enum not null default Y
修复后: "status" varchar(255) not null default 'Y'
```

`temporary`、`auto`、`评审专家` 等取值同理，其中 `temporary` 还是 PG 保留字。

---

### 二、MySQL 零值日期（数据迁移报 `22007` / `23502`）

MySQL 允许 `0000-00-00 00:00:00`，PostgreSQL 无法表示年/月/日为 0 的时间值。
分三层处理：

#### 4. 列默认值是零值日期 → 建表失败

新增 `isZeroDatetime()`，命中则丢弃该默认值。

```
修复前: "created" timestamp null default 0000-00-00 00:00:00
修复后: "created" timestamp null
```

#### 5. 行数据是零值日期 → COPY 报 `invalid input syntax for type timestamp`

在 `runMigration` 的列值转换中接入 `isZeroDatetime()`，命中则置为 `NULL`，
与建表时的处理保持一致。

该函数会对每行的每列调用，因此先用 `0000-` 前缀做廉价短路，仅命中时才执行
`strings.ToLower`，避免逐行产生字符串分配。

#### 6. 置 NULL 后撞上 `not null` 约束 → 报 `23502`

新增 `isTimeType()`，**目标库的时间类型列（`timestamp` / `datetime` /
`date` / `time`）一律建为可空**。

> **注意**：此处曾有过一次修正。最初的做法是用「该列的默认值是不是零值日期」
> 来推断它会不会存零值日期，该代理指标不成立——MySQL 在非严格 SQL 模式下
> 允许把非法日期写进 `not null` 的时间列，所以这类列声明的 `not null`
> **整体不可信**，与默认值无关。例如 `sys_user.applytime` 的默认值是合法的
> `1990-02-02 11:00:00`，但列内仍存在零值日期。现改为整体放宽。

---

### 三、日志可观测性

#### 7. `failedTable.log` 缺少失败原因

新增 `errSummary()`，写入 `SQLSTATE 错误码 + 错误消息` 摘要。

lib/pq 的 `Error.Error()` 只返回 Message，摘要额外带上错误码便于归类排查：

```
修复后:
test_xingshen -- 23502 null value in column "opttime_content2" violates not-null constraint
sys_user -- 23502 null value in column "applytime" violates not-null constraint
```

常见错误码对照：

| 错误码 | 含义 |
|---|---|
| `23502` | 非空约束冲突 |
| `23503` | 外键约束冲突 |
| `22007` | 日期格式非法 |
| `22001` | 值超长 |
| `22021` | 非法 UTF-8 字节序列 |
| `42601` | 语法错误 |

---

### 四、标识符大小写（列名不再被强制小写）

原实现把**列名**改成小写，但**表名**保持原样。这个组合本身是自洽的：
建表用小写列名 + 索引/外键/序列里的列名**不加引号**（PostgreSQL 会把未加引号的
标识符折叠成小写），两边正好对得上。

但只要源库存在**大小写混合的列**（如 `userName`），目标库就会得到一个小写的
`username`，与应用程序期望的列名不符。改为全链路保留原始大小写后，
必须同时给所有列名引用加上双引号，否则引用会折叠成小写而找不到列。

共 6 处改动：

| # | 位置 | 改动 |
|---|---|---|
| 8 | `cmd/tablemeta.go` 建表查询 | `concat('"', lower(column_name), '"')` → 去掉 `lower()` |
| 9 | `cmd/root.go` `preMigData` | 去掉 `strings.ToLower(value)`，COPY 列名保留原始大小写 |
| 10 | `cmd/root.go` `prepareSqlStr` | 去掉 `strings.ToLower(sqlStr)`，不再对发给 MySQL 的分页语句整体小写 |
| 11 | `cmd/tablemeta.go` `IdxCreate` | `CONCAT('', COLUMN_NAME, '')` → `CONCAT('"', COLUMN_NAME, '"')`（8 处） |
| 12 | `cmd/tablemeta.go` `FKCreate` | `GROUP_CONCAT(COLUMN_NAME)` → `GROUP_CONCAT(CONCAT('"',COLUMN_NAME,'"'))`，被引用列同理 |
| 13 | `cmd/tablemeta.go` `SeqCreate` | `alter_default` 去掉外层 `lower()`，表名/列名加引号；序列名仍取 `lower(TABLE_NAME)_lower(COLUMN_NAME)` 以与 `drop`/`create sequence` 保持一致 |

第 10 项同时修复了另一个独立缺陷：原代码对**发给 MySQL 的整条分页语句**做了
小写转换，包括表名。在大小写敏感的 MySQL（Linux 默认 `lower_case_table_names=0`）上，
含大写的表名会变成「table doesn't exist」。且同函数的无主键分支本就不做转换，
两个分支行为不一致。

**对目标应用的约束**：列名一旦保留混合大小写，PostgreSQL 中引用它时
**必须加双引号且大小写完全一致**。未加引号的 `SELECT userName FROM t` 会被
折叠成 `username` 而报 `column does not exist`；`SELECT "userName" FROM t` 才正确。
列名全为小写的表不受影响。

**已知限制**：`ViewCreate` 取 MySQL 的 `VIEW_DEFINITION` 后会把反引号**删除**
（`replace(VIEW_DEFINITION,'`','')`），视图体内的混合大小写列引用会被折叠成小写
而创建失败，报错见 `viewCreateFailed.log`。彻底解决需改为把反引号转换成双引号，
但视图体里若出现字符串字面量中的反引号会被误转，尚未处理。

### 五、表注释与列注释未同步

#### 14. 补充 `COMMENT ON` 语句

**修复前**：目标库没有任何注释。原因是：

- **列注释**：代码从 `information_schema.COLUMNS.column_comment` 读取并扫描进了
  `Table.columnComment` 字段，但该字段**从未被使用**——建表语句只拼了
  名字/类型/可空性/默认值。
- **表注释**：全项目对 `information_schema.TABLES.TABLE_COMMENT` 的引用数为 0，
  压根没有查询。

**根因是语法差异**：MySQL 支持把注释内联在 DDL 里，PostgreSQL 不支持。

```sql
-- MySQL
CREATE TABLE t (c INT COMMENT '列注释') COMMENT='表注释';

-- PostgreSQL：注释必须用独立语句，且表要已存在
CREATE TABLE t (c INT);
COMMENT ON TABLE  t   IS '表注释';
COMMENT ON COLUMN t.c IS '列注释';
```

原代码的建表语句是照 MySQL 写法拼的，到 PostgreSQL 这边自然没有注释——
不是被丢弃，是从未生成。

**修复后**，`TableCreate` 在建表成功后追加：

```sql
comment on table  "sys_user" is '用户表';
comment on column "sys_user"."userName" is '用户名';
```

实现要点：

- 表注释通过 `select ifnull(table_comment,'') from information_schema.TABLES where table_schema=database() and table_name=?` 查询（带占位符，非字符串拼接）
- 列注释在建表循环中收集，**建表成功后**才执行——表不存在时 `COMMENT ON` 会报错
- 空注释（`''`）和 `ifnull` 哨兵值（字符串 `"null"`）都跳过，不产生无意义语句
- 注释失败**不计入建表失败数**：表和数据的迁移不受影响，只写 `commentFailed.log`，
  由 `check_log.sh` / `check_log.ps1` 的失败清单捕获（两个脚本已同步添加该项）

#### 15. 新增 `quoteLiteral()`

注释文本来自 MySQL 元数据，是任意字符串，必须转义后才能内联——
`COMMENT ON` 属于工具语句，**不支持绑定参数**，只能拼字符串。

```go
func quoteLiteral(s string) string {
	s = strings.ReplaceAll(s, "\x00", "")
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
```

- 单引号双写（SQL 标准转义）
- 剔除 NUL 字节——PostgreSQL 文本类型不接受 `\x00`
- 反斜杠不处理：假定目标库 `standard_conforming_strings` 为 on
  （PostgreSQL 9.1 起的默认值，也是 SQL 标准行为）

### 变更文件

| 文件 | 说明 |
|---|---|
| `cmd/tablemeta.go` | 建表逻辑：类型映射、默认值、可空性、标识符引用、注释同步 |
| `check_log.sh` / `check_log.ps1` | 失败清单新增 `commentFailed.log` |
| `cmd/root.go` | 数据迁移逻辑、日志摘要、分页语句构造 |
| `cmd/tablemeta_test.go` | 新增，71 个用例 |
| `cmd/root_test.go` | 新增，6 个用例 |
| `BUILD.md` | 新增，编译说明 |

### 新增函数

| 函数 | 位置 | 职责 |
|---|---|---|
| `isTimeType` | `cmd/tablemeta.go` | 时间类型判定，大小写不敏感 |
| `isZeroDatetime` | `cmd/tablemeta.go` | 零值日期判定，带前缀短路 |
| `quoteDefault` | `cmd/tablemeta.go` | 默认值引号与转义 |
| `errSummary` | `cmd/root.go` | 迁移错误单行摘要 |

### 测试

80 个用例全部通过，取值来自真实迁移日志
（`1990-02-02 11:00:00`、`0000-01-01 00:00:00`、`default N`、`评审专家` 等）。

```bash
go test -vet=off ./...
```

> `-vet=off` 是必须的，原因见下方遗留问题。

---

### 遗留问题

#### 安全问题（已审计，尚未修复）

| 项 | 位置 | 风险 |
|---|---|---|
| SQL 注入 | 源库元数据（表名 / 默认值 / 视图定义 / 触发器体）拼接进目标库 DDL | 高 —— 源库写权限可升级为目标库任意 SQL 执行 |
| 明文传输 | `cmd/app.go:68`、`cmd/dryrun.go:117`、`:129` 硬编码 `sslmode=disable` | 中 —— 数据库凭据与全量数据明文过网 |
| 日志权限 | `cmd/app.go` 的 `CreateDateDir` 目录 `0777`，日志文件含整行数据 | 中 —— 数据泄露、目录可被替换 |
| YAML 注入 | `tools/xlsx2yml/main.go:251` dest 字段用 `%s` 未转义 | 低 |

#### 功能问题

| 项 | 说明 |
|---|---|
| `year` 类型未映射 | MySQL `year` 列原样透传到 PG，报 `type "year" does not exist`，与 `enum` 同类问题 |
| 既有 vet 告警 | `cmd/version.go:48` 非常量格式串、`cmd/root.go:62` 无缓冲 signal channel，导致 `go test` 必须加 `-vet=off` |
| Makefile 版本注入失效 | `-ldflags "-X main.Version=..."` 符号不存在被静默忽略，版本号始终取 `cmd/version.go` 中的硬编码值 |

#### 运行注意事项

- **重跑是全量 `DROP TABLE ... CASCADE` + 重建 + 全量 COPY**，没有增量逻辑。
  目标库若已有新数据写入，重跑会清除。
- 只想补跑部分表时，用 `exclude` 排除已成功的表；**不要用 `-s`**，
  该模式会跳过序列 / 索引 / 外键 / 视图 / 触发器的创建。
