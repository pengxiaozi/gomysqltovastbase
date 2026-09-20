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

### 六、标识符大小写改为可配置

#### 16. 新增 `identifierCase` 配置项

原先是硬编码：列名强制小写、表名保留原始。现改为按 yml 配置统一处理。

```yaml
identifierCase: preserve   # preserve(默认) | lower | upper
```

| 取值 | 行为 |
|---|---|
| `preserve` | 保留原始大小写（**默认**），与 MySQL 源库一致 |
| `lower` | 全部转为小写 |
| `upper` | 全部转为大写 |

未配置或取值无法识别时按 `preserve` 处理。选择 `preserve` 作默认值是为了与
第四节修复后的行为一致，不改动已有配置文件的结果。

#### 实现：按双引号边界扫描

新增三个函数：

| 函数 | 用途 |
|---|---|
| `caseMode()` | 读取配置，返回 `lower` / `upper` / 空串(保留) |
| `applyCase(name)` | 转换单个标识符（不含引号），用于 `pq.CopyIn` 的入参 |
| `applyCaseToDDL(ddl)` | 转换 DDL 中**双引号包裹**的标识符 |

`applyCaseToDDL` 之所以按引号扫描而不是整句转换，是因为 **SQL 的字符串字面量用单引号**：

```sql
alter table "Sys_User" alter column "userID" set default nextval('seq_Sys_User_userID')
              ↑ 转换              ↑ 转换                        ↑ 必须原样保留
```

序列名以字面量形式出现在 `nextval()` 里，必须和 `create sequence` 生成的名字完全一致
（序列名生成时统一取小写且不加引号，PostgreSQL 会折叠成小写），一旦被改写就对不上。
未加引号的标识符（索引名、约束名）也不处理——它们本就会被 PostgreSQL 折叠成小写。

#### 作用点（共 15 处）

| 模块 | 转换对象 |
|---|---|
| `TableCreate` | 建表语句、删表语句、表注释、列注释 |
| `preMigData` | `truncate table` |
| `runMigration` | `pq.CopyIn` 的表名与列名入参 |
| `IdxCreate` | 索引 DDL、DISTRIBUTE BY 语句 |
| `FKCreate` | 外键 DDL |
| `SeqCreate` | `drop sequence` / `create sequence` / `alter table ... set default` |

**视图未纳入**：`ViewCreate` 的视图名不加引号，视图体内的列引用也已被去掉反引号。
若给视图名加引号而视图体不变，反而会破坏当前能正常工作的混合大小写视图
（目前视图名与视图体都折叠成小写，是自洽的）。因此视图仅在 `lower` 模式下可靠，
详见第四节「已知限制」。

### 七、连接健壮性

#### 17. 目标库连接池没有任何配置

`PrepareSrc` 设了三项，`PrepareDest` **一项都没有**，全用 `database/sql` 的默认值：

| 配置项 | 原实际值 | 后果 |
|---|---|---|
| `ConnMaxLifetime` | 0 = **连接永不过期** | 服务端或中间设备关掉连接后池子不知道，下次复用死连接就拿到 `ECONNRESET` |
| `MaxOpenConns` | 0 = 不限 | 连接数只受 `maxParallel` 间接约束 |
| `MaxIdleConns` | 默认 2 | 每轮并发干完关掉大部分连接、下一轮重新建，几千张表累计几万次建连/断连，易触发防火墙速率限制 |

现已补齐：

```go
destDb.SetConnMaxLifetime(30 * time.Minute)
destDb.SetConnMaxIdleTime(5 * time.Minute)
destDb.SetMaxIdleConns(maxParallel)
destDb.SetMaxOpenConns(maxParallel + 2)
```

> 典型症状就是 `tableCreateFailed.log` 里出现 `{"Op":"read","Net":"tcp",...,"Err":{"Syscall":"wsarecv","Err":10054}}`。
> 这是操作系统网络层的 `WSAECONNRESET`，**不是 SQL 错误**——日志里那条语句只是断连时恰好正在执行的，
> 并不代表语句有问题。

#### 18. 连接类错误自动重试

原先目标库执行失败直接 `failedCount += 1`，**没有任何重试**，一次网络抖动就永久丢掉一张表。

新增 `isRetryableConnErr()` 区分错误类型：

| 类别 | 例子 | 是否重试 |
|---|---|---|
| 连接类 | `ECONNRESET` / `EPIPE` / `EOF` / `ErrBadConn` / `57P01`(服务端关闭) | ✅ 重试 3 次，退避 1s、2s |
| SQL 层 | `23502` 非空冲突 / `42601` 语法 / `42703` 列不存在 | ❌ 不重试 |

**SQL 层错误绝不能重试**——重试多少次结果都一样，只会浪费时间并掩盖真正的问题。

两个执行路径都覆盖：

- `execDest()` 包裹目标库全部 DDL 执行点（建表、删表、注释、索引、外键、序列、视图、触发器、truncate），共 13 处
- `runMigration` 对 COPY 做**整页重试**——COPY 是流式的，中途断连无法从断点续传，只能整页重来；源库会重新查询，目标库上该页的事务已回滚，不会留下半截数据

`isRetryableConnErr` 的 SQLSTATE 判断退化为匹配消息文本，因为目标库可能由
`postgres` / `opengauss` / `highgo` 三种驱动之一连接，它们的 Error 类型各不相同。

#### 19. 顺带修复：并发信号量泄漏

原 `runMigration` 有三条 `return` 路径各自写 `<-ch`，其中 `srcDb.Query` 失败那条**漏写了**。
每次触发都会永久泄漏一个并发槽位，积累到 `maxParallel` 个之后主循环的
`ch <- struct{}{}` 会永久阻塞，**程序死锁**。

重构后 `runMigration` 拆为两层：

- `migratePage()` —— 只负责搬运与记日志，返回 `error`，**不碰通道与等待组**
- `runMigration()` —— 负责重试与并发记账，`<-ch` **只在函数末尾出现一次**

同时补上了缺失的事务回滚：`migratePage` 用 `defer txn.Rollback()` 兜底，
避免提前返回时连接被未结束的事务占住（提交后调用 `Rollback` 只返回 `ErrTxDone`，可安全忽略）。

### 八、应用代码对齐工具

#### 20. 新增 `dumpSchema` 子命令

```bash
gomysql2pg --config configs/01_xxx.yml dumpSchema -o schema.json
```

读取**目标库的真实结构**（`information_schema.columns` 关联 `information_schema.tables`），
输出 JSON 字段清单：表名/视图名 + 每个列名及其真实大小写。

之所以读目标库而不是从 MySQL 元数据推算：后者需要重放 `caseMode()`/`applyCaseToDDL()`
的转换逻辑，两边一旦有出入，下游工具就会拿着错误的列名去改应用代码。

清单里带上生成时的 `identifierCase` 取值，消费方据此判断裸标识符是否有问题。
只读，不创建任何对象。

**迁移结束时会自动导出一份**到本次运行的日志目录（`<logDir>/schema.json`），
不必再单独跑一次命令，也保证清单与这一次运行严格对应。导出失败只记 warning，
不影响迁移结果。

底层查 `pg_catalog` 而不是 `information_schema`，原因有两条：

- `information_schema` 按当前用户权限过滤，权限不足时"查不到"与"这个 schema
  里确实没表"返回相同的空集，无法区分
- `information_schema.tables` 不含物化视图，用它做 inner join 会把这类对象丢掉

查不到对象时会**列出目标库里真正含表的 schema 及对象数**——本工具建表时不带
schema 前缀，落点由目标库的 `search_path` 决定，与 `dest.username` 不一致是常见情况
（例如都落进了 `public`），单纯报一句"没找到"毫无帮助。

#### 21. 新增 `tools/php_schema_align/` PHP 代码对齐工具

迁移到 PostgreSQL 后，PHP 工程里引用列名的写法有五大类，**每类都要单独加一层检测**：

| 类别 | 例子 | 为何失效 |
|---|---|---|
| SQL 反引号 | ``select `SN` from t`` | PostgreSQL 不支持反引号，语法错误 |
| SQL 未加引号 | `select SN from t` | ✅ 不用改——PG 折叠成小写正好匹配 |
| PHP 数组键 | `$row['SN']`、`'SN' => $v` | 结果集键跟着列名走；写入时框架拿数组键当列名 |
| 函数 / 方法字符串参数 | `array_column($rows,'SN')`、`$this->m('SN')` | 取不到时**静默返回空**，不报错 |
| Smarty 属性 | `{$v.SN}`、`{$v->SN}`、`{$v.0.SN}` | 编译成 `$v['SN']` / `$v->SN` |

**这份清单是逐轮补出来的**，每发现一种新写法就加一层。中间的两次修正值得记：

- `$smarty.session.bsh.nameType` 最初被误判成列名——它读的是 `$_SESSION`，
  改成小写会和会话写入端对不上。同类误判还有内层 `$_SESSION['bsh']['x']`
  （第二层 `[` 前面是 `]` 不是 `$_SESSION`，只查一层会漏掉）
- Smarty 的 `$part1_1.0.Name` 最初匹配不上——`->` 和 `.` 的分段正则要求每段
  以字母开头，数字索引 `.0` 直接让整条链断掉

详见 `tools/php_schema_align/README.md`。

**关键实现点：替换文本要同时满足 SQL 和 PHP 两层语法。**

最初版本直接把反引号换成 `"`，结果在 PHP 双引号字符串里提前结束了字符串：

```php
// 错误：$sql 被截断成 "SELECT " ，后面全是语法错误
$sql = "SELECT "ID", "userName" FROM "sys_user"";
```

正确做法按 PHP 字符串种类分别处理：

| PHP 写法 | 反引号替换为 |
|---|---|
| 双引号串 | `\"userName\"`（必须转义） |
| 单引号串 / nowdoc | `"userName"` |
| heredoc | `"userName"`（同双引号串行为，但引号不需转义） |

**只处理字符串字面量内部**，因此 PHP 的反引号运算符（执行 shell 命令）和注释里的
反引号天然不会被误伤。

#### 21.1 可选：前后端命名统一（`--unify-names`）

上面几类只解决**数据库相关**的改名。做完之后可能还剩一种跨层不一致：

```
数据库列 enname  ←→  HTML 表单 name="EnName"  ←→  JS $('#EnName')
```

`--unify-names` 生成补丁消除这层差异，**只出 `unify_names.patch`，不改源文件**。
覆盖 HTML `name`/`id`、JS 取值、会话键、Smarty 会话，以及 `extract()` 解出的
表单变量——这几层**必须成组联动**，漏一处就静默失效。

**刻意不改：被赋过值的 PHP 变量。**

```php
$AppendType = "kyxm";                            // 存的是【值】
"... and AppendType='".$AppendType."' ..."       //     ↑列名      ↑值
```

`$AppendType` 与列 `AppendType` 同名只是历史巧合。改它对迁移毫无作用，却有
**合并风险**——扫描发现 24 个文件同时存在两种大小写的同名变量，合并会改变行为。

应用后必须**清 Smarty 缓存**并**跑一遍完整表单提交流程**——跨层改名无法靠读
报告验收，只能靠实际运行。

**安全设计**：默认只出报告；`--apply` 先打印计划等确认；写前备份 `.bak`；
写完若系统有 `php` 则跑 `php -l`，**语法检查不通过自动回滚**。

### 九、not null 约束改为可配置

#### 22. 新增 `notNullPolicy` 配置项

原先的放宽规则是硬编码的（`isNullable != "NO" || isTimeType(...)`），
现改为按 yml 配置决定。

```yaml
notNullPolicy: time   # time(默认) | all | keep
```

| 取值 | 行为 |
|---|---|
| `time` | 只放宽时间列（**默认**，与改动前一致） |
| `all` | 所有列都建为可空，彻底规避 `23502` 非空冲突 |
| `keep` | 完全按 MySQL 的 `is_nullable` 原样保留，不放宽任何列 |

未配置或取值无法识别时按 `time` 处理，不影响已有配置文件。

新增 `destNullableFor(isNullable, dataType)` 承载这段判断，可单元测试覆盖。

> **关于「`not null` 改成 `default null`」**：PostgreSQL 里可空且无 `DEFAULT`
> 的列，插入时不带该列即为 `NULL`，`DEFAULT NULL` 是冗余写法，因此这里只输出
> 列级约束（`null` / `not null`），不额外拼 `DEFAULT NULL`。

### 十、反向迁移：PG / Vastbase → MySQL

#### 23. 新增 `pg2mysql` 子命令

```bash
gomysql2pg --config example.yml pg2mysql
gomysql2pg --config example.yml pg2mysql --batch 1000 --row-format COMPACT
```

配置沿用同一份 yml，**方向相反**：

| 配置段 | 正向迁移（默认命令） | 反向迁移（pg2mysql） |
|---|---|---|
| `src:` | MySQL 源库 | **MySQL 目标库** |
| `dest:` | PG 目标库 | **PG 源库**（PostgreSQL / Vastbase / GaussDB / HighGo） |

**为什么不是"加个 --reverse 参数"**：正向流程的六层都与方向绑死——
元数据查询用 MySQL 专有函数、类型映射单向、DDL 生成塞在 MySQL 的
`concat()` 里、数据搬运靠 PG 的 COPY 协议。所以这是一套独立实现，
只复用连接/日志/并发骨架与 PG 元数据读取范式。

**迁移顺序**：建表 → 灌数据 → 建索引 → 建外键 → 建视图。
索引和外键放在数据之后——边灌边维护索引会慢一个数量级，
外键也能避免表间先后顺序导致的失败。

| 能自动迁移 | 只报告、不自动迁移 |
|---|---|
| 列 / 类型映射 / 可空性 / 默认值 / 注释 / 主键 | 触发器（PG 绑定函数 vs MySQL 内联体，模型不同） |
| 数据行（批量 INSERT） | 分区表 / 继承子表（建表方式完全不同） |
| btree 索引、唯一索引 | 表达式索引 / 部分索引 / gin·gist（MySQL 无对应物） |
| 外键约束 | 数组类型（MySQL 装不下） |
| 视图（仅转换标识符引号） | |
| 自增列（序列 → `AUTO_INCREMENT`） | |

**自动化的边界**：PG 标识符大小写敏感，MySQL 列名不区分大小写，
因此源库若同时存在只差大小写的列名或表名，**MySQL 装不下**——
程序在建表前拦下并列出冲突项，而不是静默丢列。

**几个刻意的取舍**：

- MySQL 装不下的类型（数组）**直接报错让整张表建不出来**，不降级猜类型
- `nextval(...)` 默认值不再当默认值写，改判为自增列；
  **非主键的自增列会降级并告警**——MySQL 要求自增列必须是键
- 带 `::` 类型转换、或转不了的默认值一律丢弃并记入 `pg2mysqlWarnings.log`
  ——保留一个错误的默认值比没有它更危险
- 表达式索引、部分索引、gin/gist/hash 索引**明确跳过并给出原因**，
  静默少建一个索引会让查询在迁移后突然变慢，且极难定位
- 触发器与分区表**完全不尝试改写**，只列出名字写进 `pg2mysqlManual.log`

新增 `cmd/pgmeta.go`（PG 元数据读取 + 类型映射 + DDL 生成）、
`cmd/pg2mysql.go`（子命令与数据搬迁）、`cmd/pgmeta_test.go`（43 个用例）。

### 变更文件

| 文件 | 说明 |
|---|---|
| `cmd/pgmeta.go` | 新增，PG 元数据读取、PG→MySQL 类型映射、DDL 生成 |
| `cmd/pg2mysql.go` | 新增，反向迁移子命令与数据搬迁 |
| `cmd/dumpschema.go` | 新增，字段清单导出子命令 |
| `tools/php_schema_align/` | 新增，PHP 代码对齐扫描/改写工具 |
| `cmd/tablemeta.go` | 建表逻辑：类型映射、默认值、可空性、标识符引用、注释同步、大小写配置 |
| `cmd/root.go` | 数据迁移逻辑、日志摘要、分页语句构造、COPY 标识符大小写、连接错误重试 |
| `cmd/app.go` | 目标库连接池配置 |
| `example.yml` | 新增 `identifierCase` 配置项及说明 |
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
