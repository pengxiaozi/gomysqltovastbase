# PHP 字段对齐扫描

把 MySQL 迁移到 PostgreSQL 后，PHP 工程里引用列名的地方可能要跟着改。
这个工具负责**找出所有需要改的地方**，并支持分批自动改写。

## 一、要改什么

当目标库的列名大小写与源码不一致时（例如 `identifierCase: lower` 把
`SN` 变成了 `sn`），下面几类引用全部会失效：

| 类别 | 例子 | 为什么失效 |
|---|---|---|
| **SQL 反引号** | ``select `SN` from t`` | PostgreSQL 不支持反引号，直接语法错误 |
| **SQL 未加引号** | `select SN from t` | ✅ 不用改——PG 自动折叠成小写，正好匹配 |
| **PHP 数组键** | `$row['SN']`、`'SN' => $v` | 结果集的键跟着列名走；写入时框架拿数组键当列名 |
| **函数/方法字符串参数** | `array_column($rows,'SN')`、`$this->m('SN')` | 参数不在 SQL 里，取不到时**静默返回空**；自定义方法内部怎么用看不到 |
| **Smarty 模板属性** | `{$v.SN}`、`{$v->SN}`、`{$v.0.SN}` | 编译成 `$v['SN']` / `$v->SN`，同类问题 |

这些写法各不相同，**只按一种语法搜会整片漏掉**。尤其 Smarty——它和 PHP
数组语法完全不同，靠肉眼或单一正则都找不全。

## 二、用法

### 第一步：拿到字段清单

正常情况**不用单独操作**：跑一次迁移，清单会自动写进本次运行的日志目录
（`<logDir>/schema.json`）。迁移结束时会打印它的完整路径。

需要单独导出时（表建到了别的 schema、或迁移后手工改过结构）：

```bash
gomysql2pg --config configs/01_xxx.yml dumpSchema -o schema.json
gomysql2pg --config configs/01_xxx.yml dumpSchema --schema public -o schema.json
```

### 第二步：扫描出报告

```bash
python scan_php.py --schema schema.json --src /path/to/php_project \
  --dirs models,controllers,app
```

**默认只出报告，不改任何文件。** 输出到 `php_align_report/`：

- `php_schema_align.csv` —— 带 BOM，Excel 可直接打开
- `php_schema_align.md` —— 按「需要核对的程度」分组，带源码上下文

报告分组：

| 分组 | 说明 | 需逐条核对 |
|---|---|---|
| A | SQL 里的名称大小写不一致 | **需要** |
| B | schema 未命中（可能是别名或漏列） | **需要** |
| C | PHP 数组键引用了列名 | **需要** |
| D | 列名作为字符串参数传给了函数 / 方法 | **需要** |
| E | Smarty 模板属性引用了列名 | **需要** |
| F | 裸标识符大小写 | 视配置而定 |
| G | 仅换引号形态（名称一致） | 可略过 |
| H | 已跳过（疑似正则等非标识符） | 可略过 |

`--report-case-only` 只输出 A~E 五组。

### 第三步：确认后改写

反引号和其余几类是**两个独立开关**，建议分批进行：

```bash
# 第一批：SQL 反引号（机械替换，风险最低）
python scan_php.py --schema schema.json --src ... --dirs ... --apply

# 验证应用能跑之后，第二批：数组键 + 函数参数 + Smarty 属性
python scan_php.py --schema schema.json --src ... --dirs ... --apply-array-keys
```

每批都会：**先打印修改计划 → 等你输入 `yes` → 备份 → 改写 → `php -l` 语法检查**。

### 回滚

备份集中放在 `php_align_backup/`（默认），**按原目录结构镜像**，不污染源码目录：

```
php_align_backup/models/Model_pub.php        ← 原始版本
php_align_backup/app/templates/x.html        ← 原始版本
```

整体回滚：

```bash
python scan_php.py --schema schema.json --src ... --restore
```

备份**只在第一次改动时写入**，所以分批改写后回滚，退到的是**最初的样子**，
不是中间状态。

## 三、可选：前后端命名统一

上面几步只解决**数据库相关**的改名。做完之后可能还剩一种不一致：

```
数据库列 enname   ←→   HTML 表单 name="EnName"   ←→   JS $('#EnName')
```

分两步，**先出补丁看一遍，再应用**：

```bash
# ① 只出补丁，不改任何源文件
python scan_php.py --schema schema.json --src ... --dirs ... --unify-names
#   输出 php_align_report/unify_names.patch，在 IDE 里通读

# ② 确认后应用（自带备份 + php -l，不依赖 git）
python scan_php.py --schema schema.json --src ... --dirs ... --unify-apply
```

> **不要用 `git apply`**——被改的 PHP 工程未必是 git 仓库。用 `--unify-apply`
> 能同时获得备份（`php_align_backup/`）和 `php -l` 语法检查，比 `patch` 命令更安全。

### 覆盖的四层（必须成组联动）

| 层 | 例子 | 漏改的后果 |
|---|---|---|
| HTML `name`/`id` | `name="EnName"` → `name="enname"` | 表单字段名与后端不匹配 |
| JS 取值 | `$('#EnName')` → `$('#enname')` | 前端取不到元素 |
| 会话键 | `$_SESSION['bsh']['nameType']` | 会话读写成对改，漏一端即失效 |
| Smarty 会话 | `{$smarty.session.bsh.nameType}` | 同上，与写入端必须同步 |

另外还包括 `extract(parseRequset())` 解出的**表单变量**——它们的名字由表单
字段名决定，改表单名就必须跟着改。

### 刻意不改：被赋过值的 PHP 变量

```php
$AppendType = "kyxm";                            // 存的是【值】
"... and AppendType='".$AppendType."' ..."       //     ↑列名      ↑值
```

`$AppendType` 和列 `AppendType` 同名只是历史巧合。改它：
- **对迁移毫无作用**（它不是列名）
- **有合并风险**——已有文件同时存在 `$AppendType` 和 `$appendtype`，合并会改变行为

### 应用后必做

1. **清 Smarty 缓存**：`rm -rf cache/templates_c/*`
2. **跑一遍完整的表单提交流程**——这是唯一能验证联动改名没漏的方式
3. 重点验证：**新增 / 编辑 / 列表查询**三条路径

## 四、安全设计

- 默认只出报告，不改任何文件
- `--apply` 与 `--apply-array-keys` 独立，可分批验证、分批定位问题
- `--unify-names` **只出补丁**，跨层改名必须人工过一遍 diff
- 备份集中存放且镜像目录结构，源码目录不受污染
- 写完若系统有 `php`，自动跑 `php -l`，**语法不通过的文件自动回滚**
- `--yes` 跳过交互确认（配合 CI，慎用）

## 五、刻意跳过的（改了会断，不是漏扫）

| 目标 | 例子 | 原因 |
|---|---|---|
| 超全局数组键 | `$_POST['Name']` | 键名由表单/URL 决定，不随数据库变 |
| 嵌套超全局 | `$_SESSION['bsh']['nameType']` | 第二层前面是 `]` 不是 `$_SESSION`，只看一层会漏判 |
| Smarty 会话 | `{$smarty.session.bsh.nameType}` | 读的是 `$_SESSION`，改了会和写入端对不上 |
| 被赋过值的 PHP 变量 | `$AppendType = "kyxm"` | 存的是值不是列名，改名无用且有合并风险 |
| 注释 | `// 'Name' => ...` | 死代码 |
| 正则字面量 | `preg_match('/`[^`]{3,}`/')` | 反引号属于正则语法，不是标识符 |

跳过数量会在输出里明确列出，避免"以为全覆盖了"。

## 六、支持的 PHP 字符串种类

替换文本要同时满足 SQL 和 PHP 两层语法：

| PHP 写法 | 反引号替换为 |
|---|---|
| 双引号串 | `\"name\"`（必须转义，否则提前结束 PHP 字符串） |
| 单引号串 / nowdoc | `"name"` |
| heredoc | `"name"`（行为同双引号串，但引号不需转义） |

扫描只在**字符串字面量内部**进行，所以 PHP 的反引号运算符（执行 shell 命令）
天然不会被误伤。

## 七、已知限制

- 只做**静态分析**。动态拼接的 SQL、来自 JSON/配置/第三方接口的数组键无法识别
- 函数参数扫描依赖**函数名白名单**（`array_column`、CodeIgniter 查询构造器等），
  自定义封装的方法识别不了
- 键名有多个大小写变体时标为 `ambiguous`，不自动改
- 依赖 `php-cli` 才有语法检查这道保险；Windows 控制台乱码时先 `chcp 65001`
- **改完 Smarty 模板必须清缓存**（`cache/templates_c/`），否则跑的还是旧编译产物

> 静态扫描的覆盖率**无法自证**——没法通过读报告判断有没有第六种写法被漏掉。
> 最终的验收方式是**跑一遍核心业务流程**，用 PostgreSQL 的报错反查。

## 八、配套：查看源库的混合大小写字段

迁移**之前**想知道源库有哪些字段是混合大小写的，用 `mysql_case_fields.py`。
它直接查 MySQL，不需要跑迁移、也不需要连目标库：

```bash
# 读 gomysql2pg 的配置（只取 src 段）
python mysql_case_fields.py --config ../../example.yml

# 或手动指定
python mysql_case_fields.py -H 127.0.0.1 -P 3306 -u root -p 密码 -d 库名
```

需要 MySQL 驱动：`pip install pymysql`

输出：

- `mysql_case_fields.csv` —— 表名 / 列名 / 类型 / 序号 / 小写形式，便于筛选
- `mysql_case_fields.txt` —— **去重后的字段名清单**，可直接拿去 grep 代码

> 一个容易踩的坑：MySQL 默认排序规则**不区分大小写**，直接写
> `column_name <> lower(column_name)` 恒为假、一个都查不出来。
> 脚本里用 `CAST(... AS BINARY)` 强制二进制比较。

## 九、配套：按清单逐项确认替换

`replace_fields.py` 是另一种工作方式：**不分析、不推断**，只拿一份已知的字段
清单，在指定目录里**严格大小写匹配**地找出来，逐项让你确认。

和 `scan_php.py` 的分工：

| | scan_php.py | replace_fields.py |
|---|---|---|
| 输入 | schema.json（目标库结构） | 字段清单（一行一个 `原名 -> 新名`） |
| 方式 | 分类分析 + 批量改写 | 逐个字段展示 + 逐项确认 |
| 适合 | 首次全量对齐 | 清单已明确、想自己掌控每一处 |

```bash
# 先看会动哪些地方
python replace_fields.py --fields mysql_case_fields.txt --src D:\\项目 --dirs models --dry-run

# 逐项确认
python replace_fields.py --fields mysql_case_fields.txt --src D:\\项目 --dirs models
```

**默认按字段确认**——把该字段在**所有文件**里的位置全列出来，确认一次全部替换：

```
══════════════════════════════════════════════════════════════════
字段  SN  →  sn
共 3 处，涉及 2 个文件：
    controllers\C.php                       1 处
    models\M.php                            2 处
──────────────────────────────────────────────────────────────────
    controllers\C.php:2
        $col = array_column($rows, '>>>SN<<<');
    models\M.php:2
        $sql = "select `>>>SN<<<`,`ChnName` from t";
    models\M.php:3
        $row['>>>SN<<<'] = 1;

替换上面 3 处 SN → sn ？[y=全部替换 / 回车=跳过 / a=剩余全部 / q=退出]:
```

命中位置一律用 `>>> <<<` 标出，不会出现"列表里看着是这个、实际改的是另一个"。

| 键 | 含义 |
|---|---|
| `y` | 替换（默认模式=该字段的全部命中） |
| 回车 / `n` | 跳过 |
| `a` | 剩余全部替换，不再询问 |
| `q` | 退出（已确认的仍会替换） |

**默认不截断显示**——确认一次就替换该字段所有处，那就得让你看到所有处。
`--limit N` 可以主动限长。

加 `--per-match` 改成**逐处确认**，适合同一个字段在不同地方含义不同的情况。

确认的改动**先攒着，最后一次性落盘**——这样每个文件只备份一次、只跑一次
`php -l`，按偏移倒序应用也保证前面的替换不影响后面的位置。

**严格大小写匹配**：用 `\b` 定词边界但**不加 `re.I`**，所以 `SN` 只匹配 `SN`，
不会碰 `sn` 或 `Sn`。注释里的内容不参与匹配，PHP 的反引号运算符也不会被误伤。

**表名默认不替换**——清单里「表名」那一段会被跳过，需要显式加 `--include-tables`。
改表名的影响面（索引、外键、视图、应用配置）比改列名大得多。

**注意**：严格匹配意味着 `$ChnName` 这种**变量名**也会被替换。这是预期的——
逐项确认就是让你在改之前看清楚每一处。

原文备份到 `php_align_backup/`（与 `scan_php.py` 同一个备份目录），
`.php` 文件改写后自动跑 `php -l`，不通过则回滚。

## 十、参数

| 参数 | 默认 | 说明 |
|---|---|---|
| `--schema` | 必填 | `dumpSchema` 生成的 JSON |
| `--src` | 必填 | 工程根目录 |
| `--dirs` | 空 | 只扫描这些子目录（相对 `--src`，逗号分隔）；留空扫全部 |
| `--ext` | `.php,.phtml,.html,.htm` | 扫描的扩展名 |
| `--skip-dir` | 见下 | 跳过的目录名 |
| `--out-dir` | `php_align_report` | 报告输出目录 |
| `--backup-dir` | `php_align_backup` | 备份目录（镜像原结构） |
| `--report-case-only` | 关 | 报告只输出需核对的 A~E |
| `--apply` | 关 | 改写 SQL 反引号 |
| `--apply-array-keys` | 关 | 改写数组键 + 函数参数 + Smarty 属性 |
| `--unify-names` | 关 | 生成前后端命名统一**补丁**（不改文件） |
| `--unify-apply` | 关 | **应用**命名统一（备份 + `php -l`，不依赖 git） |
| `--restore` | 关 | 从备份目录整体回滚 |
| `--yes` | 关 | 跳过交互确认 |

默认 `--skip-dir`：

```
vendor, node_modules, .git, runtime, cache,
helpers, libraries, plugin, third_party, bower_components
```

其中 `cache` 尤其重要——Smarty 的编译产物在那儿，改了会被覆盖。
