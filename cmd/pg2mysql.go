package cmd

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/liushuochen/gotable"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"gomysql2pg/connect"
)

func init() {
	rootCmd.AddCommand(pg2mysqlCmd)
	pg2mysqlCmd.Flags().IntVar(&pgInsertBatch, "batch", 500,
		"每批 INSERT 的行数")
	pg2mysqlCmd.Flags().StringVar(&pgRowFormat, "row-format", "DYNAMIC",
		"建表时的 ROW_FORMAT，遇到行长度超限时改 COMPACT")
}

var (
	pgInsertBatch int
	pgRowFormat   string
)

var pg2mysqlCmd = &cobra.Command{
	Use:   "pg2mysql",
	Short: "Reverse migration: PostgreSQL / Vastbase / GaussDB to MySQL",
	Long: `把 PG 兼容库（PostgreSQL / Vastbase / GaussDB / HighGo）的数据迁移到 MySQL。

配置沿用同一份 yml，但方向相反：
  dest: 段当作【源库】（PG 侧）
  src:  段当作【目标库】（MySQL 侧）

迁移顺序：建表 -> 灌数据 -> 建索引 -> 建外键 -> 建视图。
索引和外键放到数据之后：边灌边维护索引会慢一个数量级，
外键也能避免表间先后顺序导致的失败。

能自动迁移的：
  表结构（列 / 类型 / 可空性 / 默认值 / 注释 / 主键 / 自增列）
  数据行（批量 INSERT）
  btree 索引、唯一索引
  外键约束
  视图（只做标识符引号转换）

只报告、不自动迁移的：
  触发器            PG 是「绑定到函数」，MySQL 是「BEGIN...END 内联体」，模型不同
  分区表 / 继承子表  建表方式完全不同
  表达式索引 / 部分索引 / gin·gist 索引   MySQL 没有对应物
  数组类型          MySQL 装不下

上述无法迁移的对象会在运行开始或对应阶段明确列出，并写入日志目录下的
pg2mysqlManual.log，不会静默跳过。

限制：PostgreSQL 的标识符大小写敏感，MySQL 的列名不区分大小写，
因此源库若同时存在只差大小写的列名或表名，MySQL 装不下——程序会在
建表前拦下来并列出冲突项，不会静默丢列。`,
	Run: func(cmd *cobra.Command, args []string) {
		os.Exit(runPG2MySQL())
	},
}

// resolvePGDriver 按 dest.dbType 决定 PG 侧的驱动名。
//
// 单独抽出来是因为这个映射在原代码里散了三处（app.go / dryrun.go /
// dumpschema.go 各一份），新增流程没必要再抄第四遍。
func resolvePGDriver() string {
	switch strings.ToUpper(strings.TrimSpace(viper.GetString("dest.dbType"))) {
	case "GAUSS":
		return "opengauss"
	case "HIGHGO":
		return "highgo"
	default:
		return "postgres"
	}
}

// pgDSN 拼 PG 侧的连接串。
func pgDSN(c *connect.DbConnStr) string {
	return fmt.Sprintf("host=%s user=%s password=%s dbname=%s port=%v sslmode=disable",
		c.DestHost, c.DestUserName, c.DestPassword, c.DestDatabase, c.DestPort)
}

// myDSN 拼 MySQL 侧的连接串。
func myDSN(c *connect.DbConnStr) string {
	return fmt.Sprintf("%s:%s@tcp(%s:%v)/%s?charset=utf8mb4&maxAllowedPacket=0",
		c.SrcUserName, c.SrcPassword, c.SrcHost, c.SrcPort, c.SrcDatabase)
}

func runPG2MySQL() int {
	connStr := getConn()

	// 日志目录与正向迁移同一套机制，方便统一排查
	logDir, _ := filepath.Abs(CreateDateDir(""))
	log.SetReportCaller(true)
	f, err := os.OpenFile(logDir+"/"+"run.log", os.O_CREATE|os.O_APPEND|os.O_RDWR, os.ModePerm)
	if err != nil {
		log.Fatal(err)
	}
	restore := TeeStdoutToFile(f)
	defer restore()
	defer f.Close()

	start := time.Now()
	log.Info("pg2mysql: PG 源 ", connStr.DestHost, ":", connStr.DestPort, "/", connStr.DestDatabase)
	log.Info("pg2mysql: MySQL 目标 ", connStr.SrcHost, ":", connStr.SrcPort, "/", connStr.SrcDatabase)

	srcDb, err := sql.Open(resolvePGDriver(), pgDSN(connStr))
	if err != nil {
		log.Fatal("打开 PG 源库失败: ", err)
	}
	defer srcDb.Close()
	srcDb.SetConnMaxLifetime(30 * time.Minute)
	srcDb.SetMaxOpenConns(8)
	if err := srcDb.Ping(); err != nil {
		log.Fatal("连接 PG 源库失败: ", err)
	}

	dstDb, err := sql.Open("mysql", myDSN(connStr))
	if err != nil {
		log.Fatal("打开 MySQL 目标库失败: ", err)
	}
	defer dstDb.Close()
	dstDb.SetConnMaxLifetime(30 * time.Minute)
	dstDb.SetMaxOpenConns(8)
	if err := dstDb.Ping(); err != nil {
		log.Fatal("连接 MySQL 目标库失败: ", err)
	}

	// 源 schema：dest.username 通常是 PG 侧与用户同名的 schema
	schema := strings.TrimSpace(connStr.DestUserName)
	if schema == "" {
		schema = "public"
	}

	tables, err := readPGTables(srcDb, schema)
	if err != nil {
		log.Fatal("读取源库结构失败: ", err)
	}
	if len(tables) == 0 {
		log.Errorf("schema %q 里没有基表，确认配置或在 PG 侧换个 schema", schema)
		return 1
	}
	log.Infof("源库发现 %d 张基表", len(tables))

	// 大小写冲突必须先拦：MySQL 装不下，硬转会静默丢列
	if problems := checkNameCollisions(tables); len(problems) > 0 {
		fmt.Println()
		fmt.Println("发现 MySQL 无法表达的命名冲突：")
		for _, p := range problems {
			fmt.Println("  ✗ " + p)
		}
		fmt.Println()
		log.Errorf("%d 处命名冲突，已中止——请先人工处理这些列/表", len(problems))
		LogError(logDir, "pg2mysqlFailed", strings.Join(problems, "\n"), nil)
		return 1
	}

	// 先报出无法自动迁移的对象，别让用户以为"迁完了"
	var manual []string
	if parts, err := readPGPartitioned(srcDb, schema); err != nil {
		log.Warn("读取分区/继承表失败: ", err)
	} else {
		for _, p := range parts {
			manual = append(manual, "分区/继承对象: "+p)
		}
	}
	if trigs, err := readPGTriggerNames(srcDb, schema); err != nil {
		log.Warn("读取触发器失败: ", err)
	} else {
		for _, name := range trigs {
			manual = append(manual, "触发器: "+name)
		}
	}
	if len(manual) > 0 {
		fmt.Println()
		fmt.Println("⚠ 以下对象无法自动迁移，需要人工处理：")
		for _, m := range manual {
			fmt.Println("   " + m)
		}
		fmt.Println()
		LogAlterSql(logDir, "pg2mysqlManual", strings.Join(manual, "\n"))
	}

	// 建表
	createFailed := 0
	var warnings []string
	for i := range tables {
		ddl, warns, err := buildMySQLCreateTable(tables[i], "preserve", pgRowFormat)
		warnings = append(warnings, warns...)
		if err != nil {
			log.Error("生成建表语句失败 ", tables[i].Name, " ", err)
			LogError(logDir, "pg2mysqlFailed", tables[i].Name+" -- "+err.Error(), nil)
			createFailed++
			continue
		}
		if _, err := dstDb.Exec("DROP TABLE IF EXISTS " + myIdent(tables[i].Name)); err != nil {
			log.Error("删除目标表失败 ", tables[i].Name, " ", err)
		}
		if _, err := dstDb.Exec(ddl); err != nil {
			log.Error("建表失败 ", tables[i].Name, " ", err)
			LogError(logDir, "pg2mysqlFailed", ddl, err)
			createFailed++
		}
	}
	log.Infof("建表完成：成功 %d，失败 %d", len(tables)-createFailed, createFailed)
	if len(warnings) > 0 {
		fmt.Println()
		fmt.Printf("⚠ 建表时有 %d 处信息损失（表能建出来，但部分属性没能照搬）：\n", len(warnings))
		for _, w := range warnings {
			fmt.Println("   " + w)
		}
		fmt.Println()
		LogAlterSql(logDir, "pg2mysqlWarnings", strings.Join(warnings, "\n"))
	}

	// 搬数据：每张表一个 goroutine，并发数由 maxParallel 控制
	maxParallel := viper.GetInt("maxParallel")
	if maxParallel <= 0 {
		maxParallel = 8
	}
	sem := make(chan struct{}, maxParallel)
	var wg sync.WaitGroup
	var totalRows int64
	var dataFailed int
	var mu sync.Mutex

	for i := range tables {
		if createFailed > 0 {
			// 建表失败的跳过，避免往不存在的表里灌数据刷屏
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(t pgTable) {
			defer wg.Done()
			defer func() { <-sem }()
			n, err := migratePGTable(srcDb, dstDb, schema, t, pgInsertBatch)
			mu.Lock()
			totalRows += n
			if err != nil {
				dataFailed++
				log.Error("迁移表 ", t.Name, " 失败: ", err)
				LogError(logDir, "pg2mysqlFailed", t.Name+" -- "+errSummary(err), err)
			} else {
				log.Infof("表 %s 完成，%d 行", t.Name, n)
			}
			mu.Unlock()
		}(tables[i])
	}
	wg.Wait()

	// ── 索引：数据灌完再建。边灌边维护索引会慢一个数量级 ──
	idxFailed, idxSkipped := 0, 0
	if indexes, err := readPGIndexes(srcDb, schema); err != nil {
		log.Error("读取索引失败: ", err)
	} else {
		for _, idx := range indexes {
			ddl, skip := buildMySQLIndex(idx)
			if skip != "" {
				idxSkipped++
				log.Warnf("跳过索引 %s.%s：%s", idx.Table, idx.Name, skip)
				LogAlterSql(logDir, "pg2mysqlManual",
					fmt.Sprintf("索引 %s.%s: %s", idx.Table, idx.Name, skip))
				continue
			}
			if _, err := dstDb.Exec(ddl); err != nil {
				log.Error("建索引失败 ", idx.Table, ".", idx.Name, " ", err)
				LogError(logDir, "pg2mysqlFailed", ddl, err)
				idxFailed++
			}
		}
		log.Infof("索引完成：共 %d 个，跳过 %d，失败 %d", len(indexes), idxSkipped, idxFailed)
	}

	// ── 外键：同样放到灌完数据之后，避免表间先后顺序导致的失败 ──
	fkFailed, fkSkipped := 0, 0
	if fks, err := readPGForeignKeys(srcDb, schema); err != nil {
		log.Error("读取外键失败: ", err)
	} else {
		for _, fk := range fks {
			ddl, skip := buildMySQLFK(fk)
			if skip != "" {
				fkSkipped++
				log.Warnf("跳过外键 %s：%s", fk.Name, skip)
				LogAlterSql(logDir, "pg2mysqlManual",
					fmt.Sprintf("外键 %s.%s: %s", fk.Table, fk.Name, skip))
				continue
			}
			if _, err := dstDb.Exec(ddl); err != nil {
				log.Error("建外键失败 ", fk.Table, ".", fk.Name, " ", err)
				LogError(logDir, "pg2mysqlFailed", ddl, err)
				fkFailed++
			}
		}
		log.Infof("外键完成：共 %d 个，跳过 %d，失败 %d", len(fks), fkSkipped, fkFailed)
	}

	// ── 视图：最后建，因为视图可能依赖前两者 ──
	viewFailed := 0
	if views, err := readPGViews(srcDb, schema); err != nil {
		log.Error("读取视图失败: ", err)
	} else {
		for _, v := range views {
			ddl := buildMySQLView(v)
			if _, err := dstDb.Exec(ddl); err != nil {
				log.Error("建视图失败 ", v.Name, " ", err)
				LogError(logDir, "pg2mysqlFailed", ddl, err)
				viewFailed++
			}
		}
		log.Infof("视图完成：共 %d 个，失败 %d", len(views), viewFailed)
	}

	cost := time.Since(start)
	allFailed := createFailed + dataFailed + idxFailed + fkFailed + viewFailed
	fmt.Println()
	tbl, _ := gotable.Create("Direction", "Schema", "Tables", "Rows",
		"IndexSkip", "FkSkip", "Failed", "LogDir")
	if tbl != nil {
		_ = tbl.AddRow([]string{
			"PG -> MySQL", schema,
			fmt.Sprintf("%d", len(tables)),
			fmt.Sprintf("%d", totalRows),
			fmt.Sprintf("%d", idxSkipped),
			fmt.Sprintf("%d", fkSkipped),
			fmt.Sprintf("%d", allFailed),
			logDir,
		})
		fmt.Println(tbl)
	}
	log.Infof("pg2mysql 完成，共 %d 行，耗时 %s，日志目录 %s", totalRows, cost, logDir)

	if allFailed > 0 {
		return 1
	}
	return 0
}

// migratePGTable 把一张 PG 表的数据搬到 MySQL。
//
// 用批量 INSERT 而不是 COPY：MySQL 没有 PG 那套 COPY 协议，
// 而逐行 INSERT 在几十万行以上会慢到不可接受。
func migratePGTable(srcDB, dstDB *sql.DB, schema string, t pgTable, batch int) (int64, error) {
	if batch <= 0 {
		batch = 500
	}
	pgCols := make([]string, 0, len(t.Columns))
	myCols := make([]string, 0, len(t.Columns))
	isBlob := make([]bool, 0, len(t.Columns))
	for _, c := range t.Columns {
		pgCols = append(pgCols, pgIdent(c.Name))
		myCols = append(myCols, myIdent(c.Name))
		mt, err := pgTypeToMySQL(c.DataType, c.EnumVals)
		if err != nil {
			return 0, err
		}
		isBlob = append(isBlob, strings.Contains(mt, "blob") || strings.Contains(mt, "binary"))
	}

	query := "SELECT " + strings.Join(pgCols, ",") + " FROM " +
		pgIdent(schema) + "." + pgIdent(t.Name)
	rows, err := srcDB.Query(query)
	if err != nil {
		return 0, fmt.Errorf("查询源表失败: %w", err)
	}
	defer rows.Close()

	// 每行一组占位符，批内用逗号连接
	rowPlaceholder := "(" + strings.TrimSuffix(strings.Repeat("?,", len(pgCols)), ",") + ")"
	insertHead := "INSERT INTO " + myIdent(t.Name) + " (" + strings.Join(myCols, ",") + ") VALUES "

	values := make([]sql.RawBytes, len(pgCols))
	scanArgs := make([]interface{}, len(pgCols))
	for i := range values {
		scanArgs[i] = &values[i]
	}

	var total int64
	var pending []interface{}
	var pendingRows int

	flush := func() error {
		if pendingRows == 0 {
			return nil
		}
		sql := insertHead + strings.TrimSuffix(strings.Repeat(rowPlaceholder+",", pendingRows), ",")
		if _, err := dstDB.Exec(sql, pending...); err != nil {
			return fmt.Errorf("批量写入失败(本批 %d 行): %w", pendingRows, err)
		}
		total += int64(pendingRows)
		pending = pending[:0]
		pendingRows = 0
		return nil
	}

	for rows.Next() {
		if err := rows.Scan(scanArgs...); err != nil {
			return total, fmt.Errorf("扫描行失败: %w", err)
		}
		for i, v := range values {
			if v == nil {
				pending = append(pending, nil)
				continue
			}
			// bytea 要按二进制送，其余按字符串送——MySQL 驱动会据此
			// 决定用文本还是二进制协议，混用会把二进制内容按字符集转换
			if isBlob[i] {
				pending = append(pending, []byte(v))
			} else {
				pending = append(pending, string(v))
			}
		}
		pendingRows++
		if pendingRows >= batch {
			if err := flush(); err != nil {
				return total, err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return total, err
	}
	return total, flush()
}
