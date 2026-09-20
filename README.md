此代码原始基础为：https://github.com/iverycd/gomysql2pg

# 编译说明

## 环境要求

- Go 1.24 或更高（`go.mod` 声明 `go 1.24.0`）
- 首次编译需联网拉取依赖，本机 GOPROXY 为 `https://goproxy.cn,direct`

确认环境：

```bash
go version
go env GOPATH GOPROXY
```

## 一、最常用：编译到发布目录

把源码改完后，用它替换发布目录里的可执行文件。

**Git Bash**

```bash
cd /d/devsoftware/vastbases/gomysql2pg-master/gomysql2pg-master

CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags "-s -w" \
  -o "D:/devsoftware/vastbases/gomysql2pg-win-x64-v0.2.7/gomysql2pg.exe" .
```

**PowerShell**

```powershell
cd D:\devsoftware\vastbases\gomysql2pg-master\gomysql2pg-master

$env:CGO_ENABLED = "0"; $env:GOOS = "windows"; $env:GOARCH = "amd64"
go build -ldflags "-s -w" -o D:\devsoftware\vastbases\gomysql2pg-win-x64-v0.2.7\gomysql2pg.exe .
```

> PowerShell 没有 `VAR=value cmd` 这种行内环境变量语法，必须先 `$env:VAR = "..."`。
> 这些变量只对当前会话生效，用完可以 `Remove-Item Env:CGO_ENABLED` 清掉。

参数含义：

| 参数 | 作用 |
|---|---|
| `CGO_ENABLED=0` | 纯 Go 静态编译，不依赖系统 C 库 |
| `GOOS` / `GOARCH` | 目标平台，交叉编译时必填 |
| `-ldflags "-s -w"` | 去掉符号表和调试信息，二进制更小 |
| `-o` | 输出路径，可指向任意目录 |
| `.` | 编译当前目录（根包的 `main.go`） |

## 二、普通编译（编译到当前目录）

```bash
go build -o gomysql2pg.exe .
```

## 三、编译前自检

```bash
go build ./...                      # 编译全部包
go test -vet=off ./...              # 跑测试
```

> `-vet=off` 是必须的：`cmd/version.go:48` 和 `cmd/root.go:62` 存在两个既有的
> vet 告警（非常量格式串、无缓冲 signal channel），会让 `go test` 在编译阶段直接失败。
> 这两个问题与本项目功能无关，尚未修复。

## 四、验证编译结果

```bash
cd D:/devsoftware/vastbases/gomysql2pg-win-x64-v0.2.7
./gomysql2pg.exe version
```

正常输出当前版本号（如 `v0.3.1`）。

## 五、编译配套工具 xlsx2yml

从 Excel 批量生成 yml 配置的小工具，独立于主程序：

```bash
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -ldflags "-s -w" \
  -o "D:/devsoftware/vastbases/gomysql2pg-win-x64-v0.2.7/xlsx2yml.exe" ./tools/xlsx2yml
```

## 六、全平台发布包

`Makefile` 的 `release` 目标会一次性打出 MacOS / linux-arm64 / linux-x64 / win-x64 四个包：

```bash
make release VERSION=v0.3.1
```

打包内容：`gomysql2pg` 主程序、`xlsx2yml`、`example.yml`、`check_log.*`、`run_batch.*`、`configs/example.xlsx`。

## 注意事项

### 版本号是硬编码的，改 Makefile 传参没用

`cmd/version.go:12` 里版本号写死：

```go
var ver = "v0.3.1"
```

`Makefile` 的 `build` 目标传的是 `-ldflags "-X main.Version=${VERSION}"`，但：

1. 变量名叫 `ver` 不是 `Version`
2. 变量在 `cmd` 包里不是 `main` 包

两个都对不上，所以 **Go 会静默忽略这个参数**，版本号始终是源码里的值。要真正支持注入，得把 Makefile 改成：

```makefile
go build -ldflags "-X gomysql2pg/cmd.ver=${VERSION}" -o ${BINARY} .
```

或者干脆直接改 `cmd/version.go` 里的字面量。

### 输出目录要和发布目录一致

程序的日志目录、`configs/` 都是**相对当前工作目录**解析的，不是相对 exe 位置。所以运行时要先 `cd` 到发布目录：

```bash
cd D:/devsoftware/vastbases/gomysql2pg-win-x64-v0.2.7
./gomysql2pg.exe --config configs/01_xxx.yml
```

### 依赖缓存位置

本机 GOPATH 是 `D:\devsoftware\go\cahces`（注意目录名拼写是 `cahces`），模块缓存在其下的 `pkg/mod`。依赖拉不下来时先检查 `go env GOPROXY`。



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

### 变更文件

| 文件 | 说明 |
|---|---|
| `cmd/tablemeta.go` | 建表逻辑：类型映射、默认值、可空性 |
| `cmd/root.go` | 数据迁移逻辑、日志摘要 |
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


#### 运行注意事项

- **重跑是全量 `DROP TABLE ... CASCADE` + 重建 + 全量 COPY**，没有增量逻辑。
  目标库若已有新数据写入，重跑会清除。
- 只想补跑部分表时，用 `exclude` 排除已成功的表；**不要用 `-s`**，
  该模式会跳过序列 / 索引 / 外键 / 视图 / 触发器的创建。
