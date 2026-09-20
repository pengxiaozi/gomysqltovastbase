# gomysql2pg

> 此代码原始基础为：<https://github.com/iverycd/gomysql2pg>

异构数据库迁移工具：**MySQL ⇄ PostgreSQL 内核数据库**双向迁移。

支持的目标/源数据库：`PostgreSQL`、`海量数据库 Vastbase`、`华为 GaussDB`、
`电信 TelePG`、`人大金仓 Kingbase V8R6`、`华高数据库 HighGo` 等。

---

## 目录

- [核心特性](#核心特性)
- [快速开始](#快速开始)
- [命令一览](#命令一览)
- [配置说明](#配置说明)
- [反向迁移：PG / Vastbase → MySQL](#反向迁移pg--vastbase--mysql)
- [配套工具](#配套工具)
- [文档索引](#文档索引)
- [常见问题](#常见问题)

---

## 核心特性

**开箱即用** —— 解压即可运行，单文件二进制，支持 Windows / Linux / macOS。

**并发迁移** —— 多个 goroutine 并行处理，充分利用多核；支持一次迁移上百对数据库。

**覆盖对象完整** —— 表结构、视图、索引、外键、自增列、注释、行数据。

| 方向 | 命令 | 状态 |
|---|---|---|
| MySQL → PG 内核库 | 默认命令 | 成熟 |
| PG 内核库 → MySQL | `pg2mysql` | 新增，见[下方说明](#反向迁移pg--vastbase--mysql) |

**为什么需要专用工具** —— MySQL 与 PG 内核数据库在表结构、列类型、自增列实现、
函数、存储过程等方面差异很大。用 SQL 备份文件导入是效率最低、最不可取的方式。
本工具从数据字典读取对象定义并适配到目标库，把人工成本降到最低。

**过程可追溯** —— 失败的对象按类型分开记录日志（建表失败、索引失败、需人工处理、
信息损失告警），不静默跳过。

---

## 快速开始

### 1. 准备配置文件

```yaml
src:                      # 源库（MySQL）
  host: 192.168.1.3
  port: 3306
  database: test
  username: root
  password: 11111

dest:                     # 目标库（PG 内核库）
  dbType: Gauss           # Gauss / HighGo，不填则按标准 PostgreSQL
  host: 192.168.1.200
  port: 5432
  database: test
  username: test
  password: 11111

pageSize: 100000          # 分页大小，仅全库迁移时生效
maxParallel: 30           # 并发协程数
charInLength: false       # varchar 是否使用「字符长度」语义
useNvarchar2: false       # 是否统一使用 nvarchar2（GaussDB 支持）
Distributed: false        # 分布式库是否按主键设置分布列
identifierCase: preserve  # 标识符大小写：preserve / lower / upper
notNullPolicy: time       # not null 放宽策略：time / all / keep
```

完整说明见[配置说明](#配置说明)。

### 2. 运行

```bash
# 全库迁移
./gomysql2pg --config example.yml

# 只迁移 yml 中 tables: 指定的表
./gomysql2pg --config example.yml -s
```

### 3. 查看结果

迁移结束打印摘要表：

```
Object      BeginTime              EndTime                FailedTotal  ElapsedTime
Table       ...                    ...                    0            2.3s
TableData   ...                    ...                    0            1h12m
Index       ...                    ...                    0            8.4s
```

日志目录下按失败类型分开记录：

| 文件 | 含义 |
|---|---|
| `tableCreateFailed.log` | 建表失败 |
| `idxCreateFailed.log` / `FkCreateFailed.log` | 索引 / 外键失败 |
| `viewCreateFailed.log` / `TriggerCreateFailed.log` | 视图 / 触发器失败 |
| `failedTable.log` | 数据迁移失败的表 + 错误摘要 |
| `errorTableData.log` | 失败行的具体数据 |
| `commentFailed.log` | 注释同步失败 |
| `invalidTableData.log` | 数据被清洗（警告级） |

批量迁移后用 `check_log.sh` / `check_log.ps1` 一键扫描：

```bash
bash check_log.sh              # 列出所有含失败日志的批次
```

---

## 命令一览

| 命令 | 用途 |
|---|---|
| *（默认）* | 全流程迁移：建表 → 灌数据 → 索引 → 外键 → 视图 → 触发器 |
| `createTable` | 只建表结构 |
| `seqOnly` / `idxOnly` / `viewOnly` | 只建序列 / 索引 / 视图 |
| `onlyData` | 只迁移数据行 |
| `dryRun` | 只读预检：连通性 + 目标 schema 检查，不创建任何对象 |
| `compareDb` | 逐表比对源库与目标库行数 |
| `dumpSchema` | 导出目标库字段清单（JSON），供应用代码适配使用 |
| `pg2mysql` | **反向迁移**：PG 内核库 → MySQL |
| `version` | 打印版本号 |

常用全局参数：

| 参数 | 说明 |
|---|---|
| `--config <path>` | 配置文件路径，默认 `$HOME/.gomysql2pg.yaml` |
| `-s, --selFromYml` | 只迁移 yml 中 `tables:` 列出的表和 SQL |
| `-t, --tableOnly` | 配合 `createTable` 使用，只建结构不导数据 |

---

## 配置说明

### 连接

| 配置项 | 说明 |
|---|---|
| `src.*` | MySQL 侧：`host` / `port` / `database` / `username` / `password` |
| `dest.*` | PG 侧：同上，外加 `dbType` |
| `dest.dbType` | `Gauss` → openGauss 驱动；`HighGo` → HighGo 驱动；留空 → 标准 PostgreSQL |

### 迁移行为

| 配置项 | 默认 | 说明 |
|---|---|---|
| `pageSize` | 100000 | 分页大小。越大越省内存，但单页失败重试代价越高 |
| `maxParallel` | 20 | 并发协程数，同时决定目标库连接池上限 |
| `exclude` | — | 排除的表名，支持 `*` 通配 |
| `tables` | — | 配合 `-s` 使用：指定表名与查询 SQL |

### 类型映射

| 配置项 | 默认 | 说明 |
|---|---|---|
| `charInLength` | false | 生成 `varchar(100 char)` 而非 `varchar(100)` |
| `useNvarchar2` | false | 统一用 `nvarchar2`，按字符而非字节计长 |
| `Distributed` | false | 分布式库按主键设置分布列 |

### 标识符与约束

| 配置项 | 默认 | 说明 |
|---|---|---|
| `identifierCase` | `preserve` | `preserve` 保留原始大小写；`lower` / `upper` 统一转换 |
| `notNullPolicy` | `time` | `time` 只放宽时间列；`all` 全部可空；`keep` 完全照搬源库 |

> **`identifierCase` 怎么选**：看应用代码怎么写 SQL。
> 未加引号的标识符在 PG 里会折叠成小写——代码里大量裸写列名就选 `lower`，
> 用 ORM 生成带引号 SQL 就选 `preserve`。**选对了应用代码可能一行都不用改。**

### 视图

| 配置项 | 说明 |
|---|---|
| `schemaMapping` | 视图定义中跨 schema 引用的映射：`源schema: 目标schema`，值为空串表示删除该前缀 |

---

## 反向迁移：PG / Vastbase → MySQL

```bash
gomysql2pg --config example.yml pg2mysql
gomysql2pg --config example.yml pg2mysql --batch 1000 --row-format COMPACT
```

**配置沿用同一份 yml，方向相反**：

| 配置段 | 正向迁移 | `pg2mysql` |
|---|---|---|
| `src:` | MySQL 源库 | **MySQL 目标库** |
| `dest:` | PG 目标库 | **PG 源库** |

### 迁移顺序

```
建表 → 灌数据 → 建索引 → 建外键 → 建视图
```

索引和外键放在数据之后——边灌边维护索引会慢一个数量级，外键也能避免表间顺序问题。

### 能力边界

| 能自动迁移 | 只报告、不自动迁移 |
|---|---|
| 表结构（列 / 类型 / 可空性 / 默认值 / 注释 / 主键） | 触发器 —— PG 绑定函数，MySQL 内联体，模型不同 |
| 数据行（批量 INSERT） | 分区表 / 继承子表 —— 建表方式完全不同 |
| btree 索引、唯一索引 | 表达式索引 / 部分索引 / gin·gist —— MySQL 无对应物 |
| 外键约束 | 数组类型 —— MySQL 装不下 |
| 视图（转换标识符引号） | |
| 自增列（序列 → `AUTO_INCREMENT`） | |

无法迁移的对象写入日志目录的 `pg2mysqlManual.log`，**不会静默跳过**。

### 已知限制

**命名冲突会中止迁移** —— PG 标识符大小写敏感（`"Name"` 和 `"name"` 是两个列），
MySQL 列名不区分大小写，两者无法共存。程序在建表前拦下并列出冲突项。

**类型装不下就报错，不降级** —— 数组等类型会让整张表建不出来，
而不是猜一个类型糊过去。静默降级会丢结构信息，事后极难发现。

---

## 配套工具

### PHP 代码字段对齐 `pythons/`

数据库迁移后，应用代码里引用的列名可能需要跟着改。三个 Python 脚本覆盖全过程：

| 脚本 | 用途 |
|---|---|
| `mysql_case_fields.py` | 迁移前：查源库有哪些字段是混合大小写 |
| `scan_php.py` | 分析代码里引用了哪些列，分类批量改写 |
| `replace_fields.py` | 按清单逐字段 / 逐处确认后替换 |

```bash
pip install pymysql                      # 仅 mysql_case_fields.py 需要

python pythons/mysql_case_fields.py --config example.yml
python pythons/scan_php.py --schema schema.json \
  --src /path/to/php --dirs models,controllers
python pythons/replace_fields.py \
  --fields mysql_case_fields.txt --src /path/to/php --dry-run
```

覆盖的引用写法：SQL 语句、PHP 数组键、函数字符串参数、Smarty 模板属性。
会识别并保护不该改的部分：表单字段名、会话数据、注释、正则字面量。

详见 [pythons/README.md](pythons/README.md)。

### Excel 转配置 `tools/xlsx2yml/`

从 Excel 批量生成迁移配置文件。

```bash
go run ./tools/xlsx2yml -f configs/example.xlsx -o configs
```

---

## 文档索引

| 文档 | 内容 |
|---|---|
| [readme_cn.md](readme_cn.md) | 详细使用指南：单库 / 多库批量迁移完整流程 |
| [BUILD.md](BUILD.md) | 编译说明：各平台构建命令、注意事项 |
| [CHANGELOG.md](CHANGELOG.md) | 变更记录：每项修复的问题、取舍与踩过的坑 |
| [pythons/README.md](pythons/README.md) | PHP 代码字段对齐工具 |

---

## 常见问题

**Q：迁移后应用读不到数据 / 列名对不上**

先确认 `identifierCase` 选对了。PG 里未加引号的标识符会折叠成小写，
若目标列是混合大小写而代码裸写列名，就会找不到列。
用 `dumpSchema` 导出目标库真实列名，配合 PHP 对齐工具排查。

**Q：建表报 `syntax error at or near "null"`**

源库使用了 PostgreSQL 没有的类型（如 MySQL 的 `enum`）或未经转换的默认值。
查看 `tableCreateFailed.log` 里的完整语句。若确认是类型映射遗漏，欢迎提 Issue。

**Q：数据迁移报 `invalid input syntax for type timestamp`**

源库存在零值日期（`0000-00-00`）。MySQL 允许，PostgreSQL 不允许——
工具会自动转为 `NULL`，若仍有残留请看 `errorTableData.log`。

**Q：重跑会重复迁移吗**

会，而且是**全量重建**：每张表都会 `DROP TABLE ... CASCADE` 后重建再灌数据。
**目标库若已有新数据写入，重跑会清除。** 只想补跑部分表时用 `exclude` 排除已成功的表——
不要用 `-s`，该模式会跳过索引、外键、视图、触发器的创建。

**Q：反向迁移后索引少了几个**

看 `pg2mysqlManual.log`。表达式索引、部分索引、gin/gist 索引 MySQL 没有对应物，
会明确列出。静默少建索引会让查询在迁移后突然变慢，且极难定位。

---

## 许可

见 [LICENSE](LICENSE)。
