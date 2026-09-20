package cmd

import (
	"database/sql"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// PG(POSTGRESQL/VASTBASE/GAUSS) 侧的元数据读取与 PG→MYSQL 类型映射。
//
// 正向迁移（MySQL→PG）的元数据查询写满了 MySQL 专有函数（ifnull/concat/
// database()），反向一行都用不了，必须重写。这里刻意读 pg_catalog 而不是
// information_schema——后者按权限过滤，权限不足时"查不到"和"确实没有"
// 返回同样的空集，排查时无法区分。

// pgColumn 一个 PG 列的全部信息。
type pgColumn struct {
	Name     string // 原始列名（可能混合大小写）
	DataType string // format_type() 的输出，如 character varying(50)
	NotNull  bool
	Default  string // 默认值表达式，空串表示无
	Comment  string
	Position int
	EnumVals []string // 当 DataType 是自定义枚举类型时，枚举取值
	// 源列是序列自增（默认值形如 nextval('...')）。
	// MySQL 用 AUTO_INCREMENT 表达，不能再用 DEFAULT 写。
	AutoIncrement bool
}

// pgTable 一张 PG 表的结构。
type pgTable struct {
	Name     string
	Comment  string
	Columns  []pgColumn
	PK       []string // 主键列名，按 ordinal 排序
	RowCount int64
}

// pgIdent 把标识符包成 PG 的双引号形式。
func pgIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// myIdent 把标识符包成 MySQL 的反引号形式。
// 反引号在标识符里要双写，否则会提前结束引用。
func myIdent(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

// myLiteral 把文本转成 MySQL 的字符串字面量。
// 注释内容来自 PG 元数据，是任意文本，单引号必须双写。
// 反斜杠同样有转义含义（NO_BACKSLASH_ESCAPES 默认是关的），也要双写。
func myLiteral(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "'", "''")
	return "'" + s + "'"
}

// ── 类型映射 ──────────────────────────────────────────────────────

var (
	reVarchar = regexp.MustCompile(`^character varying\((\d+)\)$`)
	reChar    = regexp.MustCompile(`^character\((\d+)\)$`)
	reNumeric = regexp.MustCompile(`^numeric\((\d+),(\d+)\)$`)
	reBit     = regexp.MustCompile(`^bit\((\d+)\)$`)
	// array 类型在 format_type 里表现为 "integer[]"
	reArray = regexp.MustCompile(`\[\]$`)
)

// pgTypeToMySQL 把 PG 的类型名映射成 MySQL 的类型名。
//
// 返回的 error 不为空表示这个类型 MySQL 装不下，需要人工取舍——
// 那种情况下调用方会把整张表标为失败，而不是猜一个类型糊过去。
func pgTypeToMySQL(pgType string, enumVals []string) (string, error) {
	t := strings.TrimSpace(strings.ToLower(pgType))

	// 自定义枚举类型：用 pg_enum 查到的取值还原成 MySQL 的 ENUM(...)
	if len(enumVals) > 0 {
		parts := make([]string, 0, len(enumVals))
		for _, v := range enumVals {
			parts = append(parts, myLiteral(v))
		}
		return "enum(" + strings.Join(parts, ",") + ")", nil
	}

	// 带长度的先单独认，避免被下面的前缀规则误伤
	if m := reVarchar.FindStringSubmatch(t); m != nil {
		// MySQL 的 varchar 上限是 65535 字节，超出只能降级成 text
		if n, _ := strconv.Atoi(m[1]); n > 65535 {
			return "text", nil
		}
		return "varchar(" + m[1] + ")", nil
	}
	if m := reChar.FindStringSubmatch(t); m != nil {
		if n, _ := strconv.Atoi(m[1]); n > 255 {
			return "varchar(" + m[1] + ")", nil
		}
		return "char(" + m[1] + ")", nil
	}
	if m := reNumeric.FindStringSubmatch(t); m != nil {
		return "decimal(" + m[1] + "," + m[2] + ")", nil
	}
	if m := reBit.FindStringSubmatch(t); m != nil {
		return "bit(" + m[1] + ")", nil
	}

	// 数组类型 MySQL 没有对应物，直接报错让人来决定
	if reArray.MatchString(t) {
		return "", fmt.Errorf("数组类型 %q 在 MySQL 中没有对应类型，需要人工决定拆表还是转 JSON", pgType)
	}

	switch t {
	case "smallint", "int2":
		return "smallint", nil
	case "integer", "int", "int4":
		return "int", nil
	case "bigint", "int8", "oid":
		return "bigint", nil
	case "boolean", "bool":
		return "tinyint(1)", nil
	case "real", "float4":
		return "float", nil
	case "double precision", "float8":
		return "double", nil
	case "numeric", "decimal":
		// 没带精度声明的 numeric，MySQL 的 decimal 必须给精度
		return "decimal(65,30)", nil
	case "money":
		return "decimal(19,4)", nil

	case "text", "character varying", "character", "name", "citext":
		return "longtext", nil

	case "bytea":
		return "longblob", nil
	case "uuid":
		return "char(36)", nil
	case "json", "jsonb":
		return "json", nil
	case "xml":
		return "longtext", nil
	case "inet", "cidr", "macaddr", "macaddr8":
		return "varchar(45)", nil

	case "date":
		return "date", nil
	case "time without time zone", "time":
		return "time", nil
	case "time with time zone", "timetz":
		// MySQL 的 time 不带时区，偏移只能在应用层补
		return "time", nil
	case "timestamp without time zone", "timestamp":
		return "datetime", nil
	case "timestamp with time zone", "timestamptz":
		// 存的是绝对时刻，落到 MySQL 的 datetime 会丢掉时区信息，
		// 但值是按会话时区换算后的墙上时间，对同区应用是自洽的
		return "datetime", nil
	case "interval":
		return "bigint", nil
	}

	return "", fmt.Errorf("未识别的 PG 类型 %q，请补充映射规则", pgType)
}

// ── 元数据读取 ────────────────────────────────────────────────────

// readPGTables 读取 PG 源库中指定 schema 下所有基表的完整结构。
func readPGTables(db *sql.DB, schema string) ([]pgTable, error) {
	// 一次查出所有列，含注释与默认值；表注释单独一批。
	// 列类型用 format_type() 拿到带精度的完整写法，省得到 Go 里再拼。
	const colSQL = `SELECT c.relname,
       a.attname,
       format_type(a.atttypid, a.atttypmod),
       a.attnotnull,
       COALESCE(pg_get_expr(d.adbin, d.adrelid), ''),
       COALESCE(col_description(c.oid, a.attnum), ''),
       a.attnum,
       COALESCE(t.typtype::text, ''),
       COALESCE(bt.typname, '')
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
JOIN pg_catalog.pg_attribute a ON a.attrelid = c.oid
JOIN pg_catalog.pg_type t ON t.oid = a.atttypid
LEFT JOIN pg_catalog.pg_type bt ON bt.oid = t.typbasetype AND t.typtype = 'd'
LEFT JOIN pg_catalog.pg_attrdef d ON d.adrelid = c.oid AND d.adnum = a.attnum
WHERE n.nspname = $1
  AND c.relkind = 'r'
  AND a.attnum > 0
  AND NOT a.attisdropped
ORDER BY c.relname, a.attnum`

	rows, err := db.Query(colSQL, schema)
	if err != nil {
		return nil, fmt.Errorf("读取列信息失败: %w", err)
	}
	defer rows.Close()

	var tables []pgTable
	var cur *pgTable
	for rows.Next() {
		var tableName, colName, dataType, def, comment, typtype, baseType string
		var notNull bool
		var attnum int
		if err := rows.Scan(&tableName, &colName, &dataType, &notNull,
			&def, &comment, &attnum, &typtype, &baseType); err != nil {
			return nil, fmt.Errorf("扫描列信息失败: %w", err)
		}
		if cur == nil || cur.Name != tableName {
			tables = append(tables, pgTable{Name: tableName})
			cur = &tables[len(tables)-1]
		}
		col := pgColumn{
			Name: colName, DataType: dataType, NotNull: notNull,
			Default: def, Comment: comment, Position: attnum,
		}
		col.AutoIncrement = strings.HasPrefix(
			strings.ToUpper(strings.TrimSpace(def)), "NEXTVAL(")
		// 自定义枚举：记下类型名，稍后统一查取值
		if typtype == "e" {
			col.EnumVals = []string{"__PG_ENUM__:" + baseType}
		}
		cur.Columns = append(cur.Columns, col)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i := range tables {
		if err := fillPGTableExtra(db, schema, &tables[i]); err != nil {
			return nil, err
		}
	}
	return tables, nil
}

// fillPGTableExtra 补齐单张表的表注释、主键、枚举取值与行数估计。
func fillPGTableExtra(db *sql.DB, schema string, t *pgTable) error {
	// 表注释
	_ = db.QueryRow(`SELECT COALESCE(obj_description(c.oid, 'pg_class'), '')
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = $1 AND c.relname = $2`, schema, t.Name).Scan(&t.Comment)

	// 主键列（按列在索引里的顺序）
	pkRows, err := db.Query(`SELECT a.attname
FROM pg_catalog.pg_index i
JOIN pg_catalog.pg_class c ON c.oid = i.indrelid
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
JOIN pg_catalog.pg_attribute a ON a.attrelid = c.oid AND a.attnum = ANY(i.indkey)
WHERE n.nspname = $1 AND c.relname = $2 AND i.indisprimary
ORDER BY array_position(i.indkey, a.attnum)`, schema, t.Name)
	if err != nil {
		return fmt.Errorf("读取主键失败: %w", err)
	}
	for pkRows.Next() {
		var name string
		if err := pkRows.Scan(&name); err != nil {
			pkRows.Close()
			return err
		}
		t.PK = append(t.PK, name)
	}
	pkRows.Close()

	// 自定义枚举的取值
	for i := range t.Columns {
		if len(t.Columns[i].EnumVals) == 1 &&
			strings.HasPrefix(t.Columns[i].EnumVals[0], "__PG_ENUM__:") {
			typeName := strings.TrimPrefix(t.Columns[i].EnumVals[0], "__PG_ENUM__:")
			vals, err := readPGEnumValues(db, schema, typeName)
			if err != nil {
				return err
			}
			t.Columns[i].EnumVals = vals
		}
	}

	// 行数用 reltuples 估计：count(*) 在大表上代价太高，
	// 这里只是给个量级用于进度显示，不要求精确
	var est float64
	_ = db.QueryRow(`SELECT COALESCE(c.reltuples, 0)
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = $1 AND c.relname = $2`, schema, t.Name).Scan(&est)
	if est > 0 {
		t.RowCount = int64(est)
	}
	return nil
}

// readPGEnumValues 读出自定义枚举类型的取值，按定义顺序。
func readPGEnumValues(db *sql.DB, schema, typeName string) ([]string, error) {
	rows, err := db.Query(`SELECT e.enumlabel
FROM pg_catalog.pg_enum e
JOIN pg_catalog.pg_type t ON t.oid = e.enumtypid
JOIN pg_catalog.pg_namespace n ON n.oid = t.typnamespace
WHERE n.nspname = $1 AND t.typname = $2
ORDER BY e.enumsortorder`, schema, typeName)
	if err != nil {
		return nil, fmt.Errorf("读取枚举取值失败: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ── 索引 / 外键 / 视图 / 触发器 ────────────────────────────────────

// pgIndex 一个索引（不含主键，主键在建表语句里内联）。
type pgIndex struct {
	Table     string
	Name      string
	Unique    bool
	Primary   bool
	Method    string   // btree / hash / gin / gist ...
	Columns   []string // 普通列名；表达式列会在 Expr 里单独报出
	Expr      string   // 表达式索引的表达式（MySQL 8 之前不支持）
	Predicate string   // 部分索引的 WHERE（MySQL 不支持）
}

// pgFK 一个外键约束。
type pgFK struct {
	Table    string
	Name     string
	Def      string // pg_get_constraintdef() 的输出，PG 语法
	HasMatch bool   // 带 MATCH FULL/PARTIAL，MySQL 不支持
}

// pgView 一个视图。
type pgView struct {
	Name string
	Def  string // pg_get_viewdef() 输出，PG 语法
}

// readPGIndexes 读取 schema 下所有非主键索引。
//
// 表达式列（indkey 里为 0）单独记到 Expr——MySQL 8.0 之前不支持
// 函数索引，硬转会得到一个语义不同的普通索引。
func readPGIndexes(db *sql.DB, schema string) ([]pgIndex, error) {
	const q = `SELECT t.relname, i.relname, ix.indisunique, ix.indisprimary,
       am.amname, a.attname,
       COALESCE(pg_get_expr(ix.indpred, ix.indrelid), ''),
       ix.indkey::text
FROM pg_catalog.pg_index ix
JOIN pg_catalog.pg_class t ON t.oid = ix.indrelid
JOIN pg_catalog.pg_class i ON i.oid = ix.indexrelid
JOIN pg_catalog.pg_namespace n ON n.oid = t.relnamespace
JOIN pg_catalog.pg_am am ON am.oid = i.relam
LEFT JOIN pg_catalog.pg_attribute a
       ON a.attrelid = ix.indrelid AND a.attnum = ANY(ix.indkey)
WHERE n.nspname = $1
  AND t.relkind = 'r'
ORDER BY t.relname, i.relname, array_position(ix.indkey, a.attnum)`

	rows, err := db.Query(q, schema)
	if err != nil {
		return nil, fmt.Errorf("读取索引失败: %w", err)
	}
	defer rows.Close()

	byName := map[string]*pgIndex{}
	var order []string
	for rows.Next() {
		var table, name, method, pred, indkey string
		var unique, primary bool
		var col sql.NullString
		if err := rows.Scan(&table, &name, &unique, &primary, &method,
			&col, &pred, &indkey); err != nil {
			return nil, fmt.Errorf("扫描索引失败: %w", err)
		}
		key := table + "\x00" + name
		idx, ok := byName[key]
		if !ok {
			idx = &pgIndex{Table: table, Name: name, Unique: unique,
				Primary: primary, Method: method, Predicate: pred}
			byName[key] = idx
			order = append(order, key)
		}
		if col.Valid {
			idx.Columns = append(idx.Columns, col.String)
		} else if idx.Expr == "" {
			// indkey 里出现 0 表示该位是表达式而非列引用
			idx.Expr = indkey
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]pgIndex, 0, len(order))
	for _, k := range order {
		out = append(out, *byName[k])
	}
	return out, nil
}

// readPGForeignKeys 读取 schema 下所有外键约束。
func readPGForeignKeys(db *sql.DB, schema string) ([]pgFK, error) {
	const q = `SELECT t.relname, con.conname,
       pg_get_constraintdef(con.oid)
FROM pg_catalog.pg_constraint con
JOIN pg_catalog.pg_class t ON t.oid = con.conrelid
JOIN pg_catalog.pg_namespace n ON n.oid = t.relnamespace
WHERE n.nspname = $1 AND con.contype = 'f'
ORDER BY t.relname, con.conname`

	rows, err := db.Query(q, schema)
	if err != nil {
		return nil, fmt.Errorf("读取外键失败: %w", err)
	}
	defer rows.Close()

	var out []pgFK
	for rows.Next() {
		var fk pgFK
		if err := rows.Scan(&fk.Table, &fk.Name, &fk.Def); err != nil {
			return nil, err
		}
		fk.HasMatch = strings.Contains(strings.ToUpper(fk.Def), "MATCH ")
		out = append(out, fk)
	}
	return out, rows.Err()
}

// readPGViews 读取 schema 下所有视图定义。
func readPGViews(db *sql.DB, schema string) ([]pgView, error) {
	const q = `SELECT c.relname, pg_get_viewdef(c.oid, true)
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = $1 AND c.relkind = 'v'
ORDER BY c.relname`

	rows, err := db.Query(q, schema)
	if err != nil {
		return nil, fmt.Errorf("读取视图失败: %w", err)
	}
	defer rows.Close()

	var out []pgView
	for rows.Next() {
		var v pgView
		if err := rows.Scan(&v.Name, &v.Def); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// readPGTriggerNames 只取触发器名字用于报告。
//
// PG 的触发器是「绑定到函数」，函数体是 PL/pgSQL；MySQL 的触发器是
// 「BEGIN ... END 内联体」。两者不是语法差异而是模型差异，无法自动翻译——
// 所以这里只负责把它们找出来告诉用户，不尝试改写。
func readPGTriggerNames(db *sql.DB, schema string) ([]string, error) {
	const q = `SELECT t.relname || '.' || tg.tgname
FROM pg_catalog.pg_trigger tg
JOIN pg_catalog.pg_class t ON t.oid = tg.tgrelid
JOIN pg_catalog.pg_namespace n ON n.oid = t.relnamespace
WHERE n.nspname = $1 AND NOT tg.tgisinternal
ORDER BY t.relname, tg.tgname`

	rows, err := db.Query(q, schema)
	if err != nil {
		return nil, fmt.Errorf("读取触发器失败: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// readPGPartitioned 找出分区表与继承子表——这两类 MySQL 的对应物
// （RANGE/LIST 分区、JOIN 继承）建表方式完全不同，强行按普通表迁会丢结构。
func readPGPartitioned(db *sql.DB, schema string) ([]string, error) {
	const q = `SELECT c.relname ||
       CASE WHEN c.relkind = 'p' THEN ' (分区表)'
            WHEN EXISTS (SELECT 1 FROM pg_catalog.pg_inherits i WHERE i.inhrelid = c.oid)
              THEN ' (继承子表)'
            ELSE '' END
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = $1
  AND (c.relkind = 'p'
       OR (c.relkind = 'r'
           AND EXISTS (SELECT 1 FROM pg_catalog.pg_inherits i WHERE i.inhrelid = c.oid)))
ORDER BY c.relname`

	rows, err := db.Query(q, schema)
	if err != nil {
		return nil, fmt.Errorf("读取分区/继承表失败: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// ── MySQL DDL 生成 ────────────────────────────────────────────────

// buildMySQLCreateTable 生成 MySQL 的建表语句。
//
// 只处理列、类型、可空性、默认值、注释和主键——索引/外键/序列留到后续，
// 先把"能建出表并灌进数据"这条路走通。
// 返回值里的 warnings 记录"能建表但有信息损失"的情况，
// 与 err 区分开：err 表示整张表建不出来，warnings 表示建出来了但有取舍。
func buildMySQLCreateTable(t pgTable, caseMode string, rowFormat string) (string, []string, error) {
	var colDefs []string
	var warnings []string

	pkCols := map[string]bool{}
	for _, name := range t.PK {
		pkCols[strings.ToLower(name)] = true
	}

	for _, c := range t.Columns {
		mt, err := pgTypeToMySQL(c.DataType, c.EnumVals)
		if err != nil {
			return "", nil, fmt.Errorf("表 %s 列 %s: %w", t.Name, c.Name, err)
		}
		def := "  " + myIdent(c.Name) + " " + mt
		if c.NotNull {
			def += " NOT NULL"
		} else {
			def += " NULL"
		}
		switch {
		case c.AutoIncrement:
			// MySQL 要求 AUTO_INCREMENT 列必须是一个键，否则建表直接报错。
			// 主键里的直接写；不在主键里的只能放弃自增，并如实报出来。
			if pkCols[strings.ToLower(c.Name)] {
				def += " AUTO_INCREMENT"
			} else {
				warnings = append(warnings, fmt.Sprintf(
					"表 %s 列 %s 在源库是序列自增，但它不是主键——MySQL 要求自增列必须是键，已改为普通列，需要在应用层补生成逻辑",
					t.Name, c.Name))
			}
		case c.Default != "":
			if d, ok := myDefault(c.Default, c.DataType); ok {
				def += " DEFAULT " + d
			} else {
				warnings = append(warnings, fmt.Sprintf(
					"表 %s 列 %s 的默认值 %q 在 MySQL 里没有对应写法，已丢弃",
					t.Name, c.Name, c.Default))
			}
		}
		if c.Comment != "" {
			def += " COMMENT " + myLiteral(c.Comment)
		}
		colDefs = append(colDefs, def)
	}

	if len(t.PK) > 0 {
		cols := make([]string, 0, len(t.PK))
		for _, name := range t.PK {
			cols = append(cols, myIdent(name))
		}
		colDefs = append(colDefs, "  PRIMARY KEY ("+strings.Join(cols, ",")+")")
	}

	tableOpts := ""
	if t.Comment != "" {
		tableOpts += " COMMENT=" + myLiteral(t.Comment)
	}
	if rowFormat != "" {
		tableOpts += " ROW_FORMAT=" + rowFormat
	}

	return fmt.Sprintf("CREATE TABLE %s (\n%s\n)%s",
		myIdent(t.Name), strings.Join(colDefs, ",\n"), tableOpts), warnings, nil
}

// buildMySQLIndex 生成索引的建表后语句。
//
// 返回的第二个值是"跳过原因"，非空表示这个索引无法迁移——
// 调用方据此记录，而不是静默少建一个索引（那会让查询在迁移后突然变慢）。
func buildMySQLIndex(idx pgIndex) (string, string) {
	if idx.Primary {
		return "", "主键已在建表语句里创建"
	}
	if idx.Expr != "" {
		return "", "表达式索引——MySQL 8.0 之前不支持函数索引，需要人工改写"
	}
	if idx.Predicate != "" {
		return "", "部分索引(带 WHERE)——MySQL 不支持，需要人工改写"
	}
	if len(idx.Columns) == 0 {
		return "", "索引没有可用的列"
	}
	switch idx.Method {
	case "btree":
		// MySQL 的默认索引类型，直接用
	case "hash":
		// MySQL 的 HASH 只对 MEMORY/NDB 引擎生效，InnoDB 会忽略；
		// 建出来语义不同，如实报出
		return "", "hash 索引——InnoDB 不支持，MySQL 会静默按 btree 建，语义不同"
	default:
		return "", fmt.Sprintf("%s 索引——MySQL 没有对应类型", idx.Method)
	}

	cols := make([]string, 0, len(idx.Columns))
	for _, c := range idx.Columns {
		cols = append(cols, myIdent(c))
	}
	kind := "INDEX"
	if idx.Unique {
		kind = "UNIQUE INDEX"
	}
	return fmt.Sprintf("CREATE %s %s ON %s (%s)",
		kind, myIdent(idx.Name), myIdent(idx.Table), strings.Join(cols, ",")), ""
}

// buildMySQLFK 把 PG 的外键定义转成 MySQL 写法。
//
// pg_get_constraintdef() 输出的语法结构和 MySQL 基本一致
// （FOREIGN KEY (...) REFERENCES ... ON DELETE ...），
// 差别只在标识符引号——把双引号换成反引号即可。
func buildMySQLFK(fk pgFK) (string, string) {
	if fk.HasMatch {
		return "", "带 MATCH FULL/PARTIAL 的外键——MySQL 不支持 MATCH 子句"
	}
	def := strings.ReplaceAll(fk.Def, `"`, "`")
	return fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s %s",
		myIdent(fk.Table), myIdent(fk.Name), def), ""
}

// buildMySQLView 生成视图语句。
//
// PG 的视图体里常有 MySQL 没有的写法（LATERAL、DISTINCT ON、窗口函数、
// 数组、::类型转换……）。这里只做标识符引号的转换，其余原样交给 MySQL——
// 转不了的自然会建失败并记进日志，不会悄悄建出一个语义不同的视图。
func buildMySQLView(v pgView) string {
	def := strings.ReplaceAll(v.Def, `"`, "`")
	return "CREATE OR REPLACE VIEW " + myIdent(v.Name) + " AS " + def
}

// myDefault 把 PG 的默认值表达式转成 MySQL 的写法。
//
// 第二个返回值表示"转不了"，此时调用方应跳过这个默认值——
// 保留一个错误的默认值比不要它更危险。
func myDefault(pgDefault, pgType string) (string, bool) {
	d := strings.TrimSpace(pgDefault)
	up := strings.ToUpper(d)

	// 序列自增值：MySQL 用 AUTO_INCREMENT，不能写成 DEFAULT nextval(...)
	if strings.HasPrefix(up, "NEXTVAL(") {
		return "", false
	}
	// 时间函数改名
	switch {
	case up == "NOW()" || strings.HasPrefix(up, "NOW("):
		return "CURRENT_TIMESTAMP", true
	case up == "CURRENT_TIMESTAMP" || strings.HasPrefix(up, "CURRENT_TIMESTAMP("):
		return "CURRENT_TIMESTAMP", true
	case up == "CURRENT_DATE":
		return "(CURRENT_DATE)", true
	case up == "TRUE" || up == "FALSE":
		// PG 的布尔字面量在 MySQL 里是 1/0
		if up == "TRUE" {
			return "1", true
		}
		return "0", true
	}
	// PG 的类型转换写法在 MySQL 里没有对应，直接放弃
	if strings.Contains(d, "::") {
		return "", false
	}
	// 纯数字、带引号的字面量，原样可用
	if _, err := strconv.ParseFloat(d, 64); err == nil {
		return d, true
	}
	if strings.HasPrefix(d, "'") && strings.HasSuffix(d, "'") {
		return d, true
	}
	// 其余（now() 的变体、自定义函数调用等）一律不猜
	return "", false
}

// checkNameCollisions 检测 MySQL 装不下的标识符冲突。
//
// PostgreSQL 的标识符大小写敏感（"Name" 和 "name" 是两个不同的列），
// 而 MySQL 的列名不区分大小写——同名不同大小写的列在 MySQL 里无法共存。
// 这是反向迁移唯一无法自动绕过的限制，必须在建表前拦下来。
func checkNameCollisions(tables []pgTable) []string {
	var problems []string
	for _, t := range tables {
		seen := make(map[string]string, len(t.Columns))
		for _, c := range t.Columns {
			key := strings.ToLower(c.Name)
			if prev, ok := seen[key]; ok && prev != c.Name {
				problems = append(problems, fmt.Sprintf(
					"表 %s 同时有列 %q 和 %q——MySQL 列名不区分大小写，无法共存",
					t.Name, prev, c.Name))
				continue
			}
			seen[key] = c.Name
		}
	}
	// 表名之间同样会冲突
	seenTab := make(map[string]string, len(tables))
	for _, t := range tables {
		key := strings.ToLower(t.Name)
		if prev, ok := seenTab[key]; ok && prev != t.Name {
			problems = append(problems, fmt.Sprintf(
				"表名 %q 与 %q 大小写不同——MySQL 表名在多数平台上不区分大小写",
				prev, t.Name))
			continue
		}
		seenTab[key] = t.Name
	}
	return problems
}
