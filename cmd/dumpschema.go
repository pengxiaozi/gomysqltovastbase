package cmd

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "gitee.com/opengauss/openGauss-connector-go-pq"
	_ "github.com/lib/pq"
	"github.com/liushuochen/gotable"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"gomysql2pg/connect"
	_ "gomysql2pg/internal/highgopq"
)

func init() {
	rootCmd.AddCommand(dumpSchemaCmd)
	dumpSchemaCmd.Flags().StringVarP(&dumpSchemaOut, "out", "o", "schema.json", "output file path")
	dumpSchemaCmd.Flags().StringVar(&dumpSchemaSchema, "schema", "",
		"target schema (default: dest.username from config)")
}

var (
	dumpSchemaOut    string
	dumpSchemaSchema string
)

var dumpSchemaCmd = &cobra.Command{
	Use:   "dumpSchema",
	Short: "Dump target table/column names as JSON for application code alignment",
	Long: `Reads the migrated PostgreSQL / GaussDB / HighGo target and writes a JSON
manifest of every table, view and its columns with their exact case.

The manifest is meant to be consumed by external tools that need to align
application code (e.g. PHP SQL statements using backticks) with the column
names that actually exist in the target database.

Read-only: connects only to the target and issues SELECT statements.`,
	Run: func(cmd *cobra.Command, args []string) {
		if exitCode := runDumpSchema(getConn()); exitCode != 0 {
			os.Exit(exitCode)
		}
	},
}

type schemaColumn struct {
	Name     string `json:"name"`
	Position int    `json:"position"`
	DataType string `json:"data_type"`
	Nullable bool   `json:"nullable"`
}

type schemaTable struct {
	Name       string         `json:"name"`
	ObjectType string         `json:"object_type"` // BASE TABLE / VIEW / MATERIALIZED VIEW / FOREIGN TABLE
	Columns    []schemaColumn `json:"columns"`
}

type schemaManifest struct {
	GeneratedAt    string        `json:"generated_at"`
	Driver         string        `json:"driver"`
	Database       string        `json:"database"`
	Schema         string        `json:"schema"`
	IdentifierCase string        `json:"identifier_case"`
	TableCount     int           `json:"table_count"`
	ColumnCount    int           `json:"column_count"`
	Tables         []schemaTable `json:"tables"`
}

func runDumpSchema(connStr *connect.DbConnStr) int {
	driver := "postgres"
	switch strings.ToUpper(strings.TrimSpace(viper.GetString("dest.dbType"))) {
	case "GAUSS":
		driver = "opengauss"
	case "HIGHGO":
		driver = "highgo"
	}

	// 只读连接：不复用全局 destDb，避免把 dump 混进迁移的连接池
	dsn := fmt.Sprintf("host=%s user=%s password=%s dbname=%s port=%v sslmode=disable",
		connStr.DestHost, connStr.DestUserName, connStr.DestPassword, connStr.DestDatabase, connStr.DestPort)
	db, err := sql.Open(driver, dsn)
	if err != nil {
		log.Error("open target failed: ", err)
		return 2
	}
	defer db.Close()
	db.SetConnMaxLifetime(5 * time.Minute)

	// 建表不带 schema 前缀，落点由目标库的 search_path 决定，
	// dest.username 只是常见情况。解析不出来时列出候选，见 resolveTargetSchema。
	schema := resolveTargetSchema(db, connStr)
	if schema == "" {
		reportEmptySchema(db, strings.TrimSpace(connStr.DestUserName), connStr)
		return 1
	}

	manifest, data, err := buildManifest(db, schema, driver, connStr.DestDatabase)
	if err != nil {
		log.Error("read target schema failed: ", err)
		return 1
	}
	if len(manifest.Tables) == 0 {
		reportEmptySchema(db, schema, connStr)
		return 1
	}

	if err := os.WriteFile(dumpSchemaOut, data, 0o600); err != nil {
		log.Error("write manifest failed: ", err)
		return 1
	}

	tbl, err := gotable.Create("Target", "Schema", "IdentifierCase", "Objects", "Columns", "Output")
	if err == nil {
		_ = tbl.AddRow([]string{
			fmt.Sprintf("%s:%d/%s", connStr.DestHost, connStr.DestPort, connStr.DestDatabase),
			schema, manifest.IdentifierCase,
			fmt.Sprintf("%d", manifest.TableCount), fmt.Sprintf("%d", manifest.ColumnCount),
			dumpSchemaOut,
		})
		fmt.Println(tbl)
	}
	log.Infof("dumpSchema wrote %d object(s) / %d column(s) to %s",
		manifest.TableCount, manifest.ColumnCount, dumpSchemaOut)
	return 0
}

// buildManifest 读取目标库并组装字段清单，同时返回序列化后的 JSON。
// dumpSchema 子命令与迁移结束时的自动导出共用这一份逻辑，避免两边格式漂移。
func buildManifest(db *sql.DB, schema, driver, database string) (schemaManifest, []byte, error) {
	tables, colCount, err := readTargetSchema(db, schema)
	if err != nil {
		return schemaManifest{}, nil, err
	}
	mode := caseMode()
	if mode == "" {
		mode = "preserve"
	}
	m := schemaManifest{
		GeneratedAt:    time.Now().Format("2006-01-02 15:04:05"),
		Driver:         driver,
		Database:       database,
		Schema:         schema,
		IdentifierCase: mode,
		TableCount:     len(tables),
		ColumnCount:    colCount,
		Tables:         tables,
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return schemaManifest{}, nil, err
	}
	return m, data, nil
}

// writeSchemaManifest 在迁移结束时把字段清单写进本次运行的日志目录。
//
// 迁移本来就要读一遍目标库，顺手导出可以省掉单独跑一次 dumpSchema，
// 也保证清单与这一次运行严格对应。整个过程与迁移结果解耦：
// 清单生成失败只记 warning，不影响已经完成的迁移。
func writeSchemaManifest(db *sql.DB, logDir string, connStr *connect.DbConnStr) {
	if strings.TrimSpace(connStr.DestUserName) == "" && strings.TrimSpace(dumpSchemaSchema) == "" {
		log.Warn("skip schema manifest: dest.username is empty in config")
		return
	}

	driver := "postgres"
	switch strings.ToUpper(strings.TrimSpace(viper.GetString("dest.dbType"))) {
	case "GAUSS":
		driver = "opengauss"
	case "HIGHGO":
		driver = "highgo"
	}

	schema := resolveTargetSchema(db, connStr)
	if schema == "" {
		log.Warn("skip schema manifest: no table or view found in any schema, " +
			"check whether the tables were created")
		return
	}

	manifest, data, err := buildManifest(db, schema, driver, connStr.DestDatabase)
	if err != nil {
		log.Warn("skip schema manifest: ", err)
		return
	}
	if len(manifest.Tables) == 0 {
		log.Warnf("skip schema manifest: schema %q contains no table or view", schema)
		return
	}

	path := filepath.Join(logDir, "schema.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		log.Warn("write schema manifest failed: ", err)
		return
	}
	log.Infof("schema manifest (%d object(s) / %d column(s)) written to %s",
		manifest.TableCount, manifest.ColumnCount, path)
}

// resolveTargetSchema 确定字段清单该读哪个 schema。
//
// 建表语句是 `create table "X"`，不带 schema 前缀，落点由目标库的 search_path
// 决定，所以「表在哪个 schema」只能问数据库，不能从配置推断。
// 依次尝试：
//
//	1. 显式 --schema        —— 查不到也不回退，用户明确指定就该如实报告
//	2. current_schema()     —— 建表的实际落点，权威答案
//	3. dest.username        —— 常见情况，但 GaussDB 建库时会建同名 schema 并设为默认，
//	                           此时落点是库名，用户名那个 schema 可能是空的
//	4. 对象最多的 schema      —— 兜底，并打印用了哪个
func resolveTargetSchema(db *sql.DB, connStr *connect.DbConnStr) string {
	// 显式指定优先，且查不到也不回退——用户明确要求了，就该如实报告
	if explicit := strings.TrimSpace(dumpSchemaSchema); explicit != "" {
		return explicit
	}
	configured := strings.TrimSpace(connStr.DestUserName)

	// 建表语句不带 schema 前缀，落点就是连接当时的 current_schema()，
	// 所以它才是"表到底在哪儿"的权威答案。dest.username 只是常见情况——
	// GaussDB / openGauss 建库时会建一个与库名同名的 schema 并设为默认，
	// 此时库名才是落点，用户名那个同名 schema 可能是空甚至不存在。
	var current string
	if err := db.QueryRow("SELECT current_schema()").Scan(&current); err == nil &&
		hasObjects(db, current) {
		if configured != "" && current != configured {
			log.Infof("target schema is %q (current_schema), not dest.username %q",
				current, configured)
		}
		return current
	}

	// current_schema() 里没有对象，退回配置里的 dest.username
	if hasObjects(db, configured) {
		log.Warnf("current_schema() contains no object, falling back to dest.username %q", configured)
		return configured
	}

	others, err := listSchemasWithTables(db)
	if err != nil || len(others) == 0 {
		return ""
	}
	log.Warnf("no object in current_schema() or dest.username, falling back to %q (%d object(s))",
		others[0].Name, others[0].Count)
	return others[0].Name
}

// hasObjects 判断 schema 下是否表/视图/物化视图。
func hasObjects(db *sql.DB, schema string) bool {
	if strings.TrimSpace(schema) == "" {
		return false
	}
	const q = `SELECT count(*)
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = $1 AND c.relkind IN ('r','v','m','f','p')`
	var n int
	if err := db.QueryRow(q, schema).Scan(&n); err != nil {
		return false
	}
	return n > 0
}

// readTargetSchema 读取目标库指定 schema 下所有表、视图、物化视图的列名。
//
// 查 pg_catalog 而不是 information_schema，原因有两条：
//
//   - information_schema 会按当前用户的权限过滤，权限不足时"查不到"与
//     "这个 schema 里确实没表"无法区分，只会得到一个误导性的空结果；
//   - information_schema.tables 不包含物化视图，用它做 inner join 会把这些对象丢掉。
//
// 直接读目标库而不是从 MySQL 元数据推算，是因为后者要重放 identifierCase 的
// 转换逻辑，一旦两边实现有出入，下游工具就会拿着错误的列名去改应用代码。
func readTargetSchema(db *sql.DB, schema string) ([]schemaTable, int, error) {
	// 带上 data_type 和可空性：排查「时间怎么带了 +08」这类问题时，
	// 光知道列名没用，必须看到列的真实类型（timestamp 还是 timestamptz）。
	const q = `SELECT c.relname, a.attname, a.attnum, c.relkind,
       format_type(a.atttypid, a.atttypmod),
       a.attnotnull
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
JOIN pg_catalog.pg_attribute a ON a.attrelid = c.oid
WHERE n.nspname = $1
  AND c.relkind IN ('r','v','m','f','p')
  AND a.attnum > 0
  AND NOT a.attisdropped
ORDER BY c.relname, a.attnum`

	rows, err := db.Query(q, schema)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	// 结果已按 (relname, attnum) 有序，顺序扫一遍即可分组，不需要额外 map
	var tables []schemaTable
	colCount := 0
	for rows.Next() {
		var relName, colName, relKind, dataType string
		var position int
		var notNull bool
		if err := rows.Scan(&relName, &colName, &position, &relKind, &dataType, &notNull); err != nil {
			return nil, 0, err
		}
		if len(tables) == 0 || tables[len(tables)-1].Name != relName {
			tables = append(tables, schemaTable{Name: relName, ObjectType: relKindName(relKind)})
		}
		last := &tables[len(tables)-1]
		last.Columns = append(last.Columns, schemaColumn{
			Name: colName, Position: position,
			DataType: dataType, Nullable: !notNull,
		})
		colCount++
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return tables, colCount, nil
}

// relKindName 把 pg_class.relkind 映射成可读的对象类型。
func relKindName(relkind string) string {
	switch relkind {
	case "r":
		return "BASE TABLE"
	case "p":
		return "PARTITIONED TABLE"
	case "v":
		return "VIEW"
	case "m":
		return "MATERIALIZED VIEW"
	case "f":
		return "FOREIGN TABLE"
	default:
		return relkind
	}
}

// reportEmptySchema 在指定 schema 里查不到任何对象时给出可操作的提示。
//
// 单纯报一句 "no table found" 毫无帮助——用户真正需要知道的是数据建到哪儿去了。
// 本工具建表时不带 schema 前缀，落点由目标库的 search_path 决定，所以
// schema 名与 dest.username 不一致是常见情况（例如都落进了 public）。
func reportEmptySchema(db *sql.DB, schema string, connStr *connect.DbConnStr) {
	log.Errorf("schema %q contains no table or view on %s", schema, connStr.DestHost)

	var searchPath, currentSchema, currentUser string
	if err := db.QueryRow("SHOW search_path").Scan(&searchPath); err == nil {
		_ = db.QueryRow("SELECT current_schema()").Scan(&currentSchema)
		_ = db.QueryRow("SELECT current_user").Scan(&currentUser)
		fmt.Printf("当前用户 %s，search_path=%s，current_schema=%s\n\n",
			currentUser, searchPath, currentSchema)
	}

	others, err := listSchemasWithTables(db)
	if err != nil {
		log.Error("list schemas failed: ", err)
		return
	}
	if len(others) == 0 {
		fmt.Println("目标库里任何 schema 都没有表或视图，请确认迁移是否已经跑过。")
		return
	}

	fmt.Println("目标库中实际含对象的 schema：")
	tbl, err := gotable.Create("Schema", "对象数", "示例对象")
	if err != nil {
		for _, s := range others {
			fmt.Printf("  %-30s %d\n", s.Name, s.Count)
		}
	} else {
		for _, s := range others {
			_ = tbl.AddRow([]string{s.Name, fmt.Sprintf("%d", s.Count), s.Sample})
		}
		tbl.Align("对象数", 1)
		fmt.Println(tbl)
	}
	fmt.Println()
	fmt.Printf("如果数据在上面某个 schema 里，用 --schema 指定后重跑：\n")
	fmt.Printf("  gomysql2pg --config %s dumpSchema --schema <上面的 schema 名> -o schema.json\n\n",
		viper.ConfigFileUsed())
	fmt.Println("若列表里没有你的表，请确认迁移是否真的建过表（看 log 目录下的 tableCreateFailed.log）。")
}

type schemaStat struct {
	Name   string
	Count  int
	Sample string
}

// listSchemasWithTables 列出非系统 schema 及其包含的对象数，按对象数降序。
// 系统 schema 按前缀过滤，另外显式排除 GaussDB / openGauss 自带的一批。
func listSchemasWithTables(db *sql.DB) ([]schemaStat, error) {
	const q = `SELECT n.nspname, count(*) AS rels,
       (array_agg(c.relname ORDER BY c.relname))[1] AS sample
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r','v','m','f','p')
  AND n.nspname NOT LIKE 'pg\_%'
  AND n.nspname NOT IN ('information_schema', 'dbe_perf', 'snapshot', 'blockchain',
                        'cstore', 'sqladvisor', 'db4ai', 'pkg_service', 'xmltable')
GROUP BY n.nspname
ORDER BY rels DESC, n.nspname`

	rows, err := db.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []schemaStat
	for rows.Next() {
		var s schemaStat
		var sample sql.NullString
		if err := rows.Scan(&s.Name, &s.Count, &sample); err != nil {
			return nil, err
		}
		s.Sample = sample.String
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Count > out[j].Count })
	return out, nil
}
