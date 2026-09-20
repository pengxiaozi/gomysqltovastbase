package cmd

import (
	"fmt"
	"github.com/spf13/viper"
	"regexp"

	"strconv"
	"strings"
	"time"
)

var tabRet []string
var tableCount int
var failedCount int

type Database interface {
	// TableCreate (logDir string, tableMap map[string][]string) (result []string) 单线程
	TableCreate(logDir string, tblName string, ch chan struct{})
	SeqCreate(logDir string) (result []string)
	IdxCreate(logDir string, excludeTable []string) (result []string)
	ViewCreate(logDir string) (result []string)
	FKCreate(logDir string) (result []string)
	TriggerCreate(logDir string) (result []string)
}

type Table struct {
	columnName             string
	dataType               string
	characterMaximumLength string
	isNullable             string
	columnDefault          string
	numericPrecision       string
	numericScale           string
	datetimePrecision      string
	columnKey              string
	columnComment          string
	ordinalPosition        int
	destType               string
	destNullable           string
	destDefault            string
	autoIncrement          int
	destSeqSql             string
	destDefaultSeq         string
	dropSeqSql             string
	destIdxSql             string
	viewSql                string
}

// notNullPolicy 读取 yml 的 notNullPolicy 配置。
//
//	time —— 只放宽时间列（默认）
//	all  —— 所有列都建为可空
//	keep —— 完全按 MySQL 的 is_nullable 原样保留
//
// 未配置或取值无法识别时按 time 处理。
func notNullPolicy() string {
	switch strings.ToLower(strings.TrimSpace(viper.GetString("notNullPolicy"))) {
	case "all":
		return "all"
	case "keep":
		return "keep"
	default:
		return "time"
	}
}

// destNullableFor 决定目标列的可空性，返回 "null" 或 "not null"。
//
// 默认策略是只放宽时间列：MySQL 在非严格 SQL 模式下允许把非法日期
// （如 0000-00-00）写进 not null 的时间列，所以这类列声明的 not null
// 并不可信；PostgreSQL 无法表示零值日期，迁移时会把它们置为 NULL，
// 建表必须跟着放宽，否则 COPY 会撞 23502 非空冲突。
//
// PostgreSQL 里可空且无 DEFAULT 的列，插入时不带该列即为 NULL，
// 不需要额外写 DEFAULT NULL，所以这里只输出列级约束。
func destNullableFor(isNullable, dataType string) string {
	switch notNullPolicy() {
	case "all":
		return "null"
	case "keep":
		if isNullable != "NO" {
			return "null"
		}
		return "not null"
	default: // time
		if isNullable != "NO" || isTimeType(dataType) {
			return "null"
		}
		return "not null"
	}
}

// isTimeType 判断是否为 MySQL 的时间类型。
// 类型名大小写不敏感：建表路径取 information_schema 的小写，
// 迁移路径取驱动 DatabaseTypeName 的大写。
func isTimeType(dataType string) bool {
	switch strings.ToLower(dataType) {
	case "timestamp", "datetime", "date", "time":
		return true
	default:
		return false
	}
}

// isZeroDatetime 判断 MySQL 的零值日期字面量，例如 0000-00-00 00:00:00、
// 0000-01-01 00:00:00。PostgreSQL 无法表示年/月/日为 0 的时间值，这类值必须
// 特殊处理，否则报 invalid input syntax for type timestamp：
// 建表时丢弃该列的默认值，迁移行数据时把该列置为 NULL。
// 限定时间类型，避免误伤 varchar 里以 0000- 开头的普通字符串。
func isZeroDatetime(dataType, val string) bool {
	// 先用前缀做廉价判断：该函数在迁移时会对每一行的每一列调用，
	// 绝大多数值在这里就直接返回，避免为其执行 ToLower 造成分配。
	s := strings.TrimSpace(val)
	if len(s) < 5 || !strings.HasPrefix(s, "0000-") {
		return false
	}
	return isTimeType(dataType)
}

// quoteDefault 按 MySQL 列类型决定默认值是否需要单引号包裹。
// MySQL 的 information_schema.COLUMNS.column_default 返回的是「裸值」：
// 数值类型可以原样输出，但字符串/时间/枚举类型必须补上单引号，否则 PostgreSQL
// 会把 1990-02-02 11:00:00 当成算术表达式(1990-02-02=1986)、把 enum 的默认值
// N 当成列引用，直接报 syntax error (42601)。
func quoteDefault(dataType, def string) string {
	upper := strings.ToUpper(strings.TrimSpace(def))
	// 表达式类默认值一旦被引号包裹就会退化成字符串字面量，必须原样输出
	for _, expr := range []string{"CURRENT_TIMESTAMP", "CURRENT_DATE", "CURRENT_TIME", "LOCALTIME", "LOCALTIMESTAMP", "NULL", "TRUE", "FALSE"} {
		if upper == expr || strings.HasPrefix(upper, expr+"(") {
			return def
		}
	}
	switch dataType {
	case "int", "mediumint", "tinyint", "smallint", "bigint",
		"decimal", "numeric", "double", "float", "real", "year", "bit":
		return def // 数值类型无需使用单引号包围
	default:
		// 其余类型(varchar/char/text/enum/set/timestamp/datetime/date/time 等)统一加引号，
		// 值里出现的单引号按 SQL 标准双写转义
		return "'" + strings.ReplaceAll(def, "'", "''") + "'"
	}
}

// quoteLiteral 把文本转成 PostgreSQL 的字符串字面量。
// 注释内容来自 MySQL 元数据，可能含单引号或换行，必须双写单引号后再内联——
// COMMENT ON 属于工具语句，不支持绑定参数，只能拼字符串。
// 同时去掉 NUL 字节：PostgreSQL 的文本类型不接受 \x00，带上会整条语句报错。
// 该写法假定目标库 standard_conforming_strings 为 on(PostgreSQL 9.1 起的默认值，
// 也是 SQL 标准行为)，此时反斜杠在普通字面量里没有转义含义，无需处理。
func quoteLiteral(s string) string {
	s = strings.ReplaceAll(s, "\x00", "")
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// caseMode 读取 yml 的 identifierCase 配置。
// 取值 lower(全小写) / upper(全大写) / preserve(保留原始，默认)。
// 未配置或取值无法识别时按 preserve 处理，返回空串。
func caseMode() string {
	switch strings.ToLower(strings.TrimSpace(viper.GetString("identifierCase"))) {
	case "lower":
		return "lower"
	case "upper":
		return "upper"
	default:
		return ""
	}
}

// applyCase 按 identifierCase 转换单个标识符(表名/列名，不含引号)。
func applyCase(name string) string {
	switch caseMode() {
	case "lower":
		return strings.ToLower(name)
	case "upper":
		return strings.ToUpper(name)
	default:
		return name
	}
}

// applyCaseToDDL 按 identifierCase 转换 DDL 中双引号包裹的标识符。
//
// 之所以按引号扫描而不是整句转换：SQL 的字符串字面量用的是单引号，
// 这样 nextval('seq_x_y') 里的内容不会被误改，序列名才能和 create sequence 对上。
// 未加引号的标识符(索引名、约束名)不处理——PostgreSQL 本就会把它们折叠成小写，
// 与生成这些 DDL 时的不加引号写法保持一致。
func applyCaseToDDL(ddl string) string {
	if caseMode() == "" {
		return ddl
	}
	var b strings.Builder
	b.Grow(len(ddl))
	segStart := 0 // 当前双引号段在 ddl 中的起点(不含引号本身)
	inQuote := false
	for i := 0; i < len(ddl); i++ {
		if ddl[i] != '"' {
			continue
		}
		if !inQuote {
			b.WriteString(ddl[segStart:i]) // 引号外的内容原样保留
			inQuote = true
		} else {
			b.WriteString(applyCase(ddl[segStart:i])) // 引号内的标识符按配置转换
			inQuote = false
		}
		b.WriteByte('"')
		segStart = i + 1
	}
	// 收尾：引号未闭合时，剩余部分仍处在标识符段内，按标识符处理，
	// 与循环里已进入引号段的状态保持一致(正常生成的 DDL 不会走到这里)
	if inQuote {
		b.WriteString(applyCase(ddl[segStart:]))
	} else {
		b.WriteString(ddl[segStart:])
	}
	return b.String()
}

// ensureTimeColumnTypes 建表后核对时间列的实际类型，不对就改回来。
//
// 有些目标库（GaussDB 的部分兼容模式）会把 DDL 里写的 timestamp 落成
// timestamp WITH time zone，数据读出来就带上 +08 偏移。这是目标库单方面的
// 解释，改工具的 DDL 治不了根——所以建完表先查一遍实际类型。
//
// 此时表刚建好、还没有数据，直接改类型不存在"按哪个时区还原"的歧义，
// 这也是放在灌数据之前做的原因。
func ensureTimeColumnTypes(logDir, tblName string) {
	const q = `SELECT a.attname, format_type(a.atttypid, a.atttypmod)
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
JOIN pg_catalog.pg_attribute a ON a.attrelid = c.oid
WHERE c.relname = $1
  AND n.nspname = current_schema()
  AND a.attnum > 0
  AND NOT a.attisdropped
  AND format_type(a.atttypid, a.atttypmod)
      IN ('timestamp with time zone', 'time with time zone')
ORDER BY a.attnum`

	rows, err := destDb.Query(q, tblName)
	if err != nil {
		log.Warn("check time column types failed ", tblName, " ", err)
		return
	}
	var actions []string
	for rows.Next() {
		var col, actual string
		if err := rows.Scan(&col, &actual); err != nil {
			log.Warn("scan time column type failed ", err)
			continue
		}
		want := "timestamp without time zone"
		if strings.HasPrefix(actual, "time with") {
			want = "time without time zone"
		}
		actions = append(actions, fmt.Sprintf("alter column %s type %s",
			quoteLiteral(col), want))
		log.Warnf("table %s column %s is %q on target, fixing to %q",
			tblName, col, actual, want)
	}
	_ = rows.Close()
	if len(actions) == 0 {
		return
	}

	// 一次 ALTER 改完所有列，避免每列一个来回
	sql := applyCaseToDDL("alter table " + fmt.Sprintf("\"") + tblName +
		fmt.Sprintf("\"") + " " + strings.Join(actions, ", "))
	if _, err := execDest(sql); err != nil {
		log.Error("fix time column types failed ", tblName, " ", sql, " ", err)
		LogError(logDir, "tableCreateFailed", sql, err)
	}
}

func (tb *Table) TableCreate(logDir string, tblName string, ch chan struct{}) {
	defer wg2.Done()
	var newTable Table
	tableCount += 1
	// 使用goroutine并发的创建多个表
	var colTotal int
	pgCreateTbl := "create table " + fmt.Sprintf("\"") + tblName + fmt.Sprintf("\"") + "("
	// 查询当前表总共有多少个列字段
	colTotalSql := fmt.Sprintf("select count(*) from information_schema.COLUMNS  where table_schema=database() and table_name='%s'", tblName)
	err := srcDb.QueryRow(colTotalSql).Scan(&colTotal)
	if err != nil {
		log.Error(err)
	}
	// 查询表注释。PostgreSQL 不支持在 create table 里内联注释(那是 MySQL 语法)，
	// 只能等表建好之后用独立的 COMMENT ON TABLE 语句设置。
	var tableComment string
	if err := srcDb.QueryRow("select ifnull(table_comment,'') from information_schema.TABLES where table_schema=database() and table_name=?", tblName).Scan(&tableComment); err != nil {
		log.Error("query table comment failed ", tblName, " ", err)
	}
	// 收集列注释，等建表成功后统一执行
	var commentSqls []string
	// 查询MySQL表结构
	// 列名保留原始大小写：索引/外键/序列的 DDL 都直接用 information_schema 的
	// COLUMN_NAME 拼装，这里若做小写化会与之不一致，混合大小写的列会报 column does not exist
	sql := fmt.Sprintf("select concat('\"',column_name,'\"'),data_type,ifnull(character_maximum_length,'null'),is_nullable,case  column_default when '( \\'user\\' )' then 'user' else ifnull(column_default,'null') end as column_default,ifnull(numeric_precision,'null'),ifnull(numeric_scale,'null'),ifnull(datetime_precision,'null'),ifnull(column_key,'null'),ifnull(column_comment,'null'),ORDINAL_POSITION from information_schema.COLUMNS where table_schema=database() and table_name='%s' order by ORDINAL_POSITION", tblName)
	//fmt.Println(sql)
	rows, err := srcDb.Query(sql)
	if err != nil {
		log.Error(err)
	}
	// 遍历MySQL表字段,一行就是一个字段的基本信息
	for rows.Next() {
		if err := rows.Scan(&newTable.columnName, &newTable.dataType, &newTable.characterMaximumLength, &newTable.isNullable, &newTable.columnDefault, &newTable.numericPrecision, &newTable.numericScale, &newTable.datetimePrecision, &newTable.columnKey, &newTable.columnComment, &newTable.ordinalPosition); err != nil {
			log.Error(err)
		}
		//fmt.Println(columnName,dataType,characterMaximumLength,isNullable,columnDefault,numericPrecision,numericScale,datetimePrecision,columnKey,columnComment,ordinalPosition)
		//适配MySQL字段类型到PostgreSQL字段类型
		// 列字段是否允许null，策略由 notNullPolicy 配置决定
		newTable.destNullable = destNullableFor(newTable.isNullable, newTable.dataType)
		// 列字段default默认值的处理
		switch {
		case newTable.columnDefault == "null": // 没有默认值，目标也就不设默认值
			newTable.destDefault = ""
		case isZeroDatetime(newTable.dataType, newTable.columnDefault):
			// MySQL 的零值日期(0000-00-00 00:00:00)在 PostgreSQL 中无法表示，
			// 保留该默认值会导致建表失败，这里直接丢弃(可空列的默认值即为 NULL)
			newTable.destDefault = ""
		default: // 默认值不是null时，按列类型决定是否需要单引号包裹
			newTable.destDefault = "default " + quoteDefault(newTable.dataType, newTable.columnDefault)
		}
		// 列字段类型的处理
		switch newTable.dataType {
		case "int", "mediumint", "tinyint":
			newTable.destType = "int"
		case "varchar":
			if strings.ToUpper(viper.GetString("charInLength")) == "TRUE" { // charInLength指定后，使用varchar(100 char)这种形式
				newTable.destType = "varchar(" + newTable.characterMaximumLength + " char)"
			} else {
				newTable.destType = "varchar(" + newTable.characterMaximumLength + ")" // 常规varchar
			}
			if strings.ToUpper(viper.GetString("useNvarchar2")) == "TRUE" { // 一旦useNvarchar2指定后，后面都会使用nvarchar2类型，比如GaussDB支持以字符长度作为单位
				newTable.destType = "nvarchar2(" + newTable.characterMaximumLength + ")"
			}
		case "char":
			if strings.ToUpper(viper.GetString("charInLength")) == "TRUE" {
				newTable.destType = "char(" + newTable.characterMaximumLength + " char)"
			} else {
				newTable.destType = "char(" + newTable.characterMaximumLength + ")"
			}
		case "text", "tinytext", "mediumtext", "longtext":
			newTable.destType = "text"
		case "datetime", "timestamp":
			// 类型名写全，不用裸 timestamp：
			// 裸写法在不同目标库上的解释不一致——GaussDB 的部分兼容模式会把它
			// 当成 timestamp WITH time zone，数据读出来就带上 +08 偏移。
			// MySQL 的 DATETIME 是「墙上时间」，对应的正是 without time zone。
			newTable.destType = "timestamp without time zone"
		case "time":
			// 同理：裸 time 可能被解释成 timetz，写全更稳
			newTable.destType = "time without time zone"
		case "decimal":
			if newTable.numericScale == "null" {
				newTable.destType = "decimal(" + newTable.numericPrecision + ")"
			} else {
				newTable.destType = "decimal(" + newTable.numericPrecision + "," + newTable.numericScale + ")"
			}
		case "double":
			newTable.destType = "double precision"
		case "float":
			newTable.destType = "double precision"
		case "tinyblob", "blob", "mediumblob", "longblob", "binary", "varbinary":
			newTable.destType = "bytea"
		case "enum", "set":
			// PostgreSQL 没有内联的 enum 列类型(枚举需先 CREATE TYPE 再引用)，
			// MySQL 的 enum/set 统一降级为 varchar(255)，保留长度限制和索引能力
			newTable.destType = "varchar(255)"
		// 其余类型，源库使用什么类型，目标库就使用什么类型
		default:
			newTable.destType = newTable.dataType
		}
		// 在目标库创建的语句
		pgCreateTbl += fmt.Sprintf("%s %s %s %s,", newTable.columnName, newTable.destType, newTable.destNullable, newTable.destDefault)
		if newTable.ordinalPosition == colTotal {
			pgCreateTbl = pgCreateTbl[:len(pgCreateTbl)-1] + ")" // 最后一个列字段结尾去掉逗号,并且加上语句的右括号
		}
		// 收集列注释：空串表示没写注释，'null' 是查询里 ifnull 的哨兵值，两者都跳过。
		// newTable.columnName 已由查询拼好双引号，可直接作为限定列名使用。
		if cm := strings.TrimSpace(newTable.columnComment); cm != "" && cm != "null" {
			commentSqls = append(commentSqls, applyCaseToDDL("comment on column "+fmt.Sprintf("\"")+tblName+fmt.Sprintf("\"")+"."+newTable.columnName+" is "+quoteLiteral(cm)))
		}
	}
	//fmt.Println(pgCreateTbl) // 打印创建表语句
	// 标识符大小写按 identifierCase 配置统一转换。建表与后续所有引用
	// (COPY/索引/外键/序列)必须用同一套规则，否则会找不到列。
	pgCreateTbl = applyCaseToDDL(pgCreateTbl)
	// 创建前先删除目标表
	dropDestTbl := applyCaseToDDL("drop table if exists " + fmt.Sprintf("\"") + tblName + fmt.Sprintf("\"") + " cascade")
	if _, err = execDest(dropDestTbl); err != nil {
		log.Error("drop table ", tblName, " failed ", err)
	}
	// 创建PostgreSQL表结构
	log.Info(fmt.Sprintf("%v Table total %s create table %s", time.Now().Format("2006-01-02 15:04:05.000000"), strconv.Itoa(tableCount), tblName))
	if _, err = execDest(pgCreateTbl); err != nil {
		log.Error("table ", tblName, " create failed  ", err)
		LogError(logDir, "tableCreateFailed", pgCreateTbl, err)
		failedCount += 1
	} else {
		// 表刚建好、还没灌数据，此时纠正时间列类型没有数据转换歧义
		ensureTimeColumnTypes(logDir, tblName)
		// 表建好之后才能挂注释，所以放在这里而不是并进建表语句。
		// 注释失败不影响表和数据的迁移，因此只记日志，不计入建表失败数。
		if tc := strings.TrimSpace(tableComment); tc != "" {
			cs := applyCaseToDDL("comment on table " + fmt.Sprintf("\"") + tblName + fmt.Sprintf("\"") + " is " + quoteLiteral(tc))
			if _, err = execDest(cs); err != nil {
				log.Error("comment on table ", tblName, " failed ", err)
				LogError(logDir, "commentFailed", cs, err)
			}
		}
		for _, cs := range commentSqls {
			if _, err = execDest(cs); err != nil {
				log.Error("comment on column ", tblName, " failed ", err)
				LogError(logDir, "commentFailed", cs, err)
			}
		}
	}
	<-ch
}

func (tb *Table) SeqCreate(logDir string) (result []string) {
	startTime := time.Now()
	tableCount := 0
	failedCount := 0
	var tableName string
	// 查询MySQL自增列信息，批量生成创建序列sql
	sql := fmt.Sprintf("select table_name,COLUMN_NAME,Auto_increment,lower(concat('drop sequence if exists ','seq_',TABLE_NAME,'_',COLUMN_NAME,';')) drop_seq,lower(concat('create sequence ','seq_',TABLE_NAME,'_',COLUMN_NAME,' INCREMENT BY 1 START ',Auto_increment,';')) create_seq, concat('alter table \"',table_name,'\" alter column \"',COLUMN_NAME, '\" set default nextval(', '''' ,'seq_',lower(TABLE_NAME),'_',lower(COLUMN_NAME),  '''',');') alter_default  from (select Auto_increment,column_name,a.table_name from (select TABLE_NAME, Auto_increment,case when Auto_increment  is not null then 'auto_increment' else '0' end ai from information_schema. TABLES where TABLE_SCHEMA =database() and  AUTO_INCREMENT is not null) a join (select table_name,COLUMN_NAME,EXTRA from information_schema. COLUMNS where TABLE_SCHEMA =database() and table_name in(select t.TABLE_NAME from information_schema. TABLES t where TABLE_SCHEMA =database() and AUTO_INCREMENT is not null)  and EXTRA='auto_increment' ) b on a.ai = b.EXTRA and a.table_name =b.table_name) aaa;")
	//fmt.Println(sql)
	rows, err := srcDb.Query(sql)
	if err != nil {
		log.Error(err)
	}
	// 从sql结果集遍历，获取到删除序列，创建序列，默认值为自增列
	for rows.Next() {
		tableCount += 1
		if err := rows.Scan(&tableName, &tb.columnName, &tb.autoIncrement, &tb.dropSeqSql, &tb.destSeqSql, &tb.destDefaultSeq); err != nil {
			log.Error(err)
		}
		// 序列 DDL 里的表名/列名带双引号，按配置转换；
		// nextval('seq_x_y') 是单引号字面量，不会被转换，仍与 create sequence 的名字一致
		tb.dropSeqSql = applyCaseToDDL(tb.dropSeqSql)
		tb.destSeqSql = applyCaseToDDL(tb.destSeqSql)
		tb.destDefaultSeq = applyCaseToDDL(tb.destDefaultSeq)
		// 创建前先删除目标序列
		if _, err = execDest(tb.dropSeqSql); err != nil {
			log.Error(err)
		}
		// 创建目标序列
		log.Info(fmt.Sprintf("%v ProcessingID %s create sequence %s", time.Now().Format("2006-01-02 15:04:05.000000"), strconv.Itoa(tableCount), tableName))
		if _, err = execDest(tb.destSeqSql); err != nil {
			log.Error("table ", tableName, " create sequence failed ", err)
			LogError(logDir, "seqCreateFailed", tb.destSeqSql, err)
			failedCount += 1
		}
		// 设置表自增列为序列，如果表不存并单独创建序列会有error但是毫无影响
		log.Info(fmt.Sprintf("%v ProcessingID %s set default sequence %s", time.Now().Format("2006-01-02 15:04:05.000000"), strconv.Itoa(tableCount), tableName))
		if _, err = execDest(tb.destDefaultSeq); err != nil {
			log.Error("table ", tableName, " set default sequence failed ", err)
			LogError(logDir, "seqCreateFailed", tb.destDefaultSeq, err)
			failedCount += 1
		}
	}
	endTime := time.Now()
	cost := time.Since(startTime)
	result = append(result, "Sequence", startTime.Format("2006-01-02 15:04:05.000000"), endTime.Format("2006-01-02 15:04:05.000000"), strconv.Itoa(failedCount), cost.String())
	log.Info("sequence count ", tableCount)
	return result
}

func (tb *Table) IdxCreate(logDir string, excludeTable []string) (result []string) {
	startTime := time.Now()
	failedCount := 0
	id := 0
	var sql, excludeSql string
	if excludeTable != nil {
		for _, tabName := range excludeTable {
			if strings.Contains(tabName, "*") {
				tabName = strings.ReplaceAll(tabName, "*", "%")
				excludeSql += " and table_name not like '" + tabName + "'"
			} else {
				excludeSql += " and table_name not like '" + tabName + "'"
			}

		}
		sql = fmt.Sprintf("SELECT IF ( INDEX_NAME = 'PRIMARY', CONCAT( 'ALTER TABLE \"', TABLE_NAME, '\" ', 'ADD ', IF ( NON_UNIQUE = 1, CASE UPPER( INDEX_TYPE ) WHEN 'FULLTEXT' THEN 'FULLTEXT INDEX' WHEN 'SPATIAL' THEN 'SPATIAL INDEX' ELSE CONCAT( 'INDEX ', INDEX_NAME, '' ) END, IF ( UPPER( INDEX_NAME ) = 'PRIMARY', CONCAT( 'PRIMARY KEY ' ), CONCAT( 'UNIQUE INDEX ', INDEX_NAME ) ) ), '(', GROUP_CONCAT( DISTINCT CONCAT( '\"', COLUMN_NAME, '\"' ) ORDER BY SEQ_IN_INDEX ASC SEPARATOR ', ' ), ');' ), IF ( UPPER( INDEX_NAME ) != 'PRIMARY' AND non_unique = 0,CONCAT( 'CREATE UNIQUE INDEX ', index_name, '_', substr( uuid(), 1, 8 ), substr( MD5( RAND()), 1, 3 ), ' ON \"', table_name, '\"(', GROUP_CONCAT( DISTINCT CONCAT( '\"', COLUMN_NAME, '\"' ) ORDER BY SEQ_IN_INDEX ASC SEPARATOR ', ' ), ');' ),REPLACE ( REPLACE ( CONCAT( 'CREATE INDEX ', index_name, '_', substr( uuid(), 1, 8 ), substr( MD5( RAND()), 1, 3 ), ' ON ', IF ( NON_UNIQUE = 1, CASE UPPER( INDEX_TYPE ) WHEN 'FULLTEXT' THEN 'FULLTEXT INDEX' WHEN 'SPATIAL' THEN 'SPATIAL INDEX' ELSE CONCAT( ' \"', table_name, '\"' ) END, IF ( UPPER( INDEX_NAME ) = 'PRIMARY', CONCAT( 'PRIMARY KEY ' ), CONCAT( table_name, ' xxx' ) ) ), '(', GROUP_CONCAT( DISTINCT CONCAT( '\"', COLUMN_NAME, '\"' ) ORDER BY SEQ_IN_INDEX ASC SEPARATOR ', ' ), ');' ), CHAR ( 13 ), '' ), CHAR ( 10 ), '' ) ) ) sql_text,INDEX_NAME,concat('alter table \"',table_name,'\"',' DISTRIBUTE BY hash ','(',GROUP_CONCAT( DISTINCT CONCAT( '\"', COLUMN_NAME, '\"' ) ORDER BY SEQ_IN_INDEX ASC SEPARATOR ', ' ),');') FROM information_schema.STATISTICS WHERE TABLE_SCHEMA IN ( SELECT DATABASE()) %s GROUP BY TABLE_NAME, INDEX_NAME ORDER BY TABLE_NAME ASC, INDEX_NAME ASC;", excludeSql)
	} else {
		sql = fmt.Sprintf("SELECT IF ( INDEX_NAME = 'PRIMARY', CONCAT( 'ALTER TABLE \"', TABLE_NAME, '\" ', 'ADD ', IF ( NON_UNIQUE = 1, CASE UPPER( INDEX_TYPE ) WHEN 'FULLTEXT' THEN 'FULLTEXT INDEX' WHEN 'SPATIAL' THEN 'SPATIAL INDEX' ELSE CONCAT( 'INDEX ', INDEX_NAME, '' ) END, IF ( UPPER( INDEX_NAME ) = 'PRIMARY', CONCAT( 'PRIMARY KEY ' ), CONCAT( 'UNIQUE INDEX ', INDEX_NAME ) ) ), '(', GROUP_CONCAT( DISTINCT CONCAT( '\"', COLUMN_NAME, '\"' ) ORDER BY SEQ_IN_INDEX ASC SEPARATOR ', ' ), ');' ), IF ( UPPER( INDEX_NAME ) != 'PRIMARY' AND non_unique = 0,CONCAT( 'CREATE UNIQUE INDEX ', index_name, '_', substr( uuid(), 1, 8 ), substr( MD5( RAND()), 1, 3 ), ' ON \"', table_name, '\"(', GROUP_CONCAT( DISTINCT CONCAT( '\"', COLUMN_NAME, '\"' ) ORDER BY SEQ_IN_INDEX ASC SEPARATOR ', ' ), ');' ),REPLACE ( REPLACE ( CONCAT( 'CREATE INDEX ', index_name, '_', substr( uuid(), 1, 8 ), substr( MD5( RAND()), 1, 3 ), ' ON ', IF ( NON_UNIQUE = 1, CASE UPPER( INDEX_TYPE ) WHEN 'FULLTEXT' THEN 'FULLTEXT INDEX' WHEN 'SPATIAL' THEN 'SPATIAL INDEX' ELSE CONCAT( ' \"', table_name, '\"' ) END, IF ( UPPER( INDEX_NAME ) = 'PRIMARY', CONCAT( 'PRIMARY KEY ' ), CONCAT( table_name, ' xxx' ) ) ), '(', GROUP_CONCAT( DISTINCT CONCAT( '\"', COLUMN_NAME, '\"' ) ORDER BY SEQ_IN_INDEX ASC SEPARATOR ', ' ), ');' ), CHAR ( 13 ), '' ), CHAR ( 10 ), '' ) ) ) sql_text,INDEX_NAME,concat('alter table \"',table_name,'\"',' DISTRIBUTE BY hash ','(',GROUP_CONCAT( DISTINCT CONCAT( '\"', COLUMN_NAME, '\"' ) ORDER BY SEQ_IN_INDEX ASC SEPARATOR ', ' ),');') FROM information_schema.STATISTICS WHERE TABLE_SCHEMA IN ( SELECT DATABASE()) GROUP BY TABLE_NAME, INDEX_NAME ORDER BY TABLE_NAME ASC, INDEX_NAME ASC;")
	}
	// 查询MySQL索引、主键、唯一约束等信息，批量生成创建语句
	fmt.Println("create index: ", sql)
	rows, err := srcDb.Query(sql)
	if err != nil {
		log.Error(err)
	}
	// 从sql结果集遍历，获取到创建语句
	for rows.Next() {
		var indexName, alterDistributeSql string
		id += 1
		if err := rows.Scan(&tb.destIdxSql, &indexName, &alterDistributeSql); err != nil {
			log.Error(err)
		}
		// 索引 DDL 由 MySQL 侧拼装，标识符已用双引号包裹，这里按配置转换大小写
		tb.destIdxSql = applyCaseToDDL(tb.destIdxSql)
		alterDistributeSql = applyCaseToDDL(alterDistributeSql)
		// 如果是分布式数据库，先更改分布列，这里挑选主键作为分布列,避免之前创建表没指定主键，某些数据库会自动挑选分布列，后面再加主键会遇到主键没包括分布列的问题
		if strings.ToUpper(viper.GetString("Distributed")) == "TRUE" {
			if indexName == "PRIMARY" {
				if _, err = execDest(alterDistributeSql); err != nil {
					log.Error(alterDistributeSql, " alter table DISTRIBUTE failed ", err)
					LogError(logDir, "DistributedAlterFailed", tb.destIdxSql, err)
					failedCount += 1
				}
			}
		}
		// 不管是不是分布式数据库，下面的主键都会创建
		log.Info(fmt.Sprintf("%v ProcessingID %s %s", time.Now().Format("2006-01-02 15:04:05.000000"), strconv.Itoa(id), tb.destIdxSql))
		if _, err = execDest(tb.destIdxSql); err != nil {
			log.Error("index ", tb.destIdxSql, " create index failed ", err)
			LogError(logDir, "idxCreateFailed", tb.destIdxSql, err)
			failedCount += 1
		}
	}
	endTime := time.Now()
	cost := time.Since(startTime)
	log.Info("index  count ", id)
	result = append(result, "Index", startTime.Format("2006-01-02 15:04:05.000000"), endTime.Format("2006-01-02 15:04:05.000000"), strconv.Itoa(failedCount), cost.String())
	return result
}

func (tb *Table) FKCreate(logDir string) (result []string) {
	failedCount := 0
	startTime := time.Now()
	id := 0
	var createSql string
	var fkTable string
	// 查询MySQL外键，批量生成创建语句
	//sql := fmt.Sprintf("SELECT ifnull(concat('ALTER TABLE \"',K.TABLE_NAME,'\" ADD CONSTRAINT ',K.CONSTRAINT_NAME,' FOREIGN KEY(',GROUP_CONCAT(COLUMN_NAME),')',' REFERENCES \"',K.REFERENCED_TABLE_NAME,'\"(',GROUP_CONCAT(REFERENCED_COLUMN_NAME),')',' ON DELETE ',DELETE_RULE,' ON UPDATE ',UPDATE_RULE),'null') FROM INFORMATION_SCHEMA.KEY_COLUMN_USAGE k INNER JOIN INFORMATION_SCHEMA.REFERENTIAL_CONSTRAINTS r on k.CONSTRAINT_NAME = r.CONSTRAINT_NAME where k.CONSTRAINT_SCHEMA =database() AND r.CONSTRAINT_SCHEMA=database()  and k.REFERENCED_TABLE_NAME is not null order by k.ORDINAL_POSITION;")
	// 先获取有外键的表名
	sql := fmt.Sprintf("select  table_name from INFORMATION_SCHEMA.REFERENTIAL_CONSTRAINTS where CONSTRAINT_SCHEMA =database();")
	//fmt.Println(sql)
	rows, err := srcDb.Query(sql)
	if err != nil {
		log.Error(err)
	}
	// 从sql结果集遍历，获取到单个表外键的创建语句
	for rows.Next() {
		id += 1
		if err := rows.Scan(&fkTable); err != nil {
			log.Error(err)
		}
		sql = fmt.Sprintf("SELECT ifnull(concat('ALTER TABLE \"',K.TABLE_NAME,'\" ADD CONSTRAINT ',K.CONSTRAINT_NAME,' FOREIGN KEY(',GROUP_CONCAT(CONCAT('\"',COLUMN_NAME,'\"')),')',' REFERENCES \"',K.REFERENCED_TABLE_NAME,'\"(',GROUP_CONCAT(CONCAT('\"',REFERENCED_COLUMN_NAME,'\"')),')',' ON DELETE ',DELETE_RULE,' ON UPDATE ',UPDATE_RULE),'null') FROM INFORMATION_SCHEMA.KEY_COLUMN_USAGE k INNER JOIN INFORMATION_SCHEMA.REFERENTIAL_CONSTRAINTS r on k.CONSTRAINT_NAME = r.CONSTRAINT_NAME where k.CONSTRAINT_SCHEMA =database() AND r.CONSTRAINT_SCHEMA=database()  and k.REFERENCED_TABLE_NAME is not null and k.table_name='%s'  order by k.ORDINAL_POSITION;", fkTable)
		err := srcDb.QueryRow(sql).Scan(&createSql) // 根据单个表获取外键拼接sql
		if err != nil {
			log.Error(err)
		}
		// 外键 DDL 同样由 MySQL 侧拼装，标识符已用双引号包裹
		createSql = applyCaseToDDL(createSql)
		// 创建目标外键
		if createSql != "null" {
			log.Info(fmt.Sprintf("%v ProcessingID %s create foreign key %s", time.Now().Format("2006-01-02 15:04:05.000000"), strconv.Itoa(id), createSql))
			if _, err = execDest(createSql); err != nil {
				log.Error(createSql, " create foreign key failed ", err)
				LogError(logDir, "FkCreateFailed", createSql, err)
				failedCount += 1
			}
		}
	}
	log.Info("foreign key count ", id)
	endTime := time.Now()
	cost := time.Since(startTime)
	result = append(result, "ForeignKey", startTime.Format("2006-01-02 15:04:05.000000"), endTime.Format("2006-01-02 15:04:05.000000"), strconv.Itoa(failedCount), cost.String())
	return result
}

func (tb *Table) ViewCreate(logDir string) (result []string) {
	failedCount := 0
	startTime := time.Now()
	id := 0
	schemaMappingCache := viper.GetStringMapString("schemaMapping")
	// 查询视图，获取视图名和经过基础清理（去反引号、schema前缀、convert/utf8mb4）的视图定义
	sql := fmt.Sprintf("select table_name, replace(replace(replace(replace(VIEW_DEFINITION,'`',''),concat(table_schema,'.'),''),'convert(',''),'using utf8mb4)','') as view_def from information_schema.VIEWS where TABLE_SCHEMA=database();")
	rows, err := srcDb.Query(sql)
	if err != nil {
		log.Error(err)
	}
	// 从sql结果集遍历，获取到创建语句
	var viewName, rawDef string
	for rows.Next() {
		id += 1
		if err := rows.Scan(&viewName, &rawDef); err != nil {
			log.Error(err)
		}
		// 跨 schema 表前缀按配置映射替换（处理 MySQL 视图引用其他库的场景）
		rawDef = applySchemaMapping(rawDef, schemaMappingCache)
		// 将MySQL视图定义改写为PostgreSQL兼容语法
		transformed := transformViewDef(rawDef)
		// view_portal_myitem 中 DISABLED 列为字符串类型，PostgreSQL 不做隐式类型转换
		if viewName == "view_portal_myitem" {
			transformed = regexp.MustCompile(`(?i)\(portal_item\.DISABLED\s*=\s*0\)`).
				ReplaceAllString(transformed, "(portal_item.DISABLED = '0')")
		}
		tb.viewSql = "create or replace view " + viewName + " as " + transformed + ";"
		// 创建目标视图
		log.Info(fmt.Sprintf("%v ProcessingID %s create view %s", time.Now().Format("2006-01-02 15:04:05.000000"), strconv.Itoa(id), viewName))
		if _, err = execDest(tb.viewSql); err != nil {
			log.Error("view ", viewName, " create view failed ", err)
			LogError(logDir, "viewCreateFailed", tb.viewSql, err)
			failedCount += 1
		}
	}
	log.Info("view total ", id)
	endTime := time.Now()
	cost := time.Since(startTime)
	result = append(result, "View", startTime.Format("2006-01-02 15:04:05.000000"), endTime.Format("2006-01-02 15:04:05.000000"), strconv.Itoa(failedCount), cost.String())
	return result
}

// isWordChar reports whether b is a word character (letter, digit, underscore).
func isWordChar(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '_'
}

// stripOuterParens removes one matched outermost pair of parentheses if and
// only if the entire string is enclosed in a single matched pair.
func stripOuterParens(s string) string {
	s = strings.TrimSpace(s)
	if len(s) == 0 || s[0] != '(' {
		return s
	}
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				if i == len(s)-1 {
					return strings.TrimSpace(s[1 : len(s)-1])
				}
				return s // outer paren closes before end — not a simple wrapper
			}
		}
	}
	return s // unbalanced
}

// fullyStripOuterParens repeatedly strips outermost parens until stable.
func fullyStripOuterParens(s string) string {
	for {
		stripped := stripOuterParens(s)
		if stripped == s {
			return s
		}
		s = stripped
	}
}

// joinPart represents one operand in a JOIN chain.
type joinPart struct {
	raw      string // table ref (possibly with alias) and optional ON/USING clause
	joinType string // qualifier preceding JOIN: "LEFT", "RIGHT", "INNER", "CROSS", "FULL", ""
}

// splitTopLevelJoin splits s on JOIN keywords at parenthesis depth 0,
// also capturing the JOIN type qualifier (LEFT, RIGHT, etc.) when present.
func splitTopLevelJoin(s string) []joinPart {
	upper := strings.ToUpper(s)
	depth := 0
	start := 0
	var parts []joinPart
	pendingJoinType := ""

	i := 0
	for i < len(s) {
		ch := s[i]
		if ch == '(' {
			depth++
			i++
			continue
		}
		if ch == ')' {
			depth--
			i++
			continue
		}
		if depth == 0 && i+4 <= len(upper) && upper[i:i+4] == "JOIN" {
			// word-boundary check: char before must not be a word char
			if i > 0 && isWordChar(s[i-1]) {
				i++
				continue
			}
			// char after "JOIN" must be space or end of string
			if i+4 < len(s) && isWordChar(s[i+4]) {
				i++
				continue
			}
			// Extract the segment up to this JOIN keyword.
			// The join type qualifier (LEFT, RIGHT, etc.) may be at the tail of this segment.
			seg := strings.TrimSpace(s[start:i])
			jt := ""
			// Check if the segment ends with a known qualifier
			for _, q := range []string{"LEFT OUTER", "RIGHT OUTER", "FULL OUTER", "LEFT", "RIGHT", "INNER", "CROSS", "FULL"} {
				segUpper := strings.ToUpper(seg)
				if strings.HasSuffix(segUpper, " "+q) || segUpper == q {
					seg = strings.TrimSpace(seg[:len(seg)-len(q)])
					jt = q
					break
				}
			}
			if len(parts) == 0 {
				// First segment: record with whatever joinType was pending (empty for first)
				parts = append(parts, joinPart{raw: seg, joinType: pendingJoinType})
			} else {
				parts = append(parts, joinPart{raw: seg, joinType: pendingJoinType})
			}
			pendingJoinType = jt
			i += 4 // skip "JOIN"
			start = i
			continue
		}
		i++
	}
	// Append the last segment
	parts = append(parts, joinPart{raw: strings.TrimSpace(s[start:]), joinType: pendingJoinType})
	return parts
}

// findTopLevelKeyword returns the byte index of keyword kw at paren depth 0,
// or -1 if not found.
func findTopLevelKeyword(s, kw string) int {
	upper := strings.ToUpper(s)
	kwUpper := strings.ToUpper(kw)
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
		}
		if depth == 0 && i+len(kw) <= len(s) && upper[i:i+len(kwUpper)] == kwUpper {
			// word-boundary: preceding char must not be word char
			if i > 0 && isWordChar(s[i-1]) {
				continue
			}
			// following char must not be word char
			if i+len(kw) < len(s) && isWordChar(s[i+len(kw)]) {
				continue
			}
			return i
		}
	}
	return -1
}

// buildFromClause converts join parts into a PostgreSQL FROM clause body.
func buildFromClause(parts []joinPart) string {
	if len(parts) == 0 {
		return ""
	}

	// resolveSegment expands a segment that is fully enclosed in parens
	// (e.g. "(t1 join t2)") into a PostgreSQL join chain.
	// It only recurses when stripOuterParens makes actual progress — this
	// prevents infinite recursion when a segment merely *starts with* "("
	// but is not a fully-wrapped group (e.g. "(t1) alias").
	var resolveSegment func(seg string) string
	resolveSegment = func(seg string) string {
		seg = strings.TrimSpace(seg)
		stripped := stripOuterParens(seg) // one level only
		if stripped == seg {
			return seg // not a fully-wrapped group — use as table reference
		}
		subParts := splitTopLevelJoin(stripped)
		return buildFromClause(subParts)
	}

	// First part: just the table reference
	result := resolveSegment(parts[0].raw)

	for _, p := range parts[1:] {
		raw := strings.TrimSpace(p.raw)
		jt := p.joinType
		if jt == "" {
			jt = "INNER"
		}

		// If the whole segment is a paren-wrapped sub-group (and stripping
		// makes progress), expand it instead of treating it as a table ref.
		strippedRaw := stripOuterParens(raw)
		if strippedRaw != raw {
			sub := resolveSegment(raw)
			result += " " + sub
			continue
		}

		// Find ON or USING at depth 0
		onIdx := findTopLevelKeyword(raw, "ON")
		usingIdx := findTopLevelKeyword(raw, "USING")

		switch {
		case onIdx >= 0:
			tableRef := strings.TrimSpace(raw[:onIdx])
			onExpr := strings.TrimSpace(raw[onIdx+2:])
			result += " " + jt + " JOIN " + tableRef + " ON " + onExpr
		case usingIdx >= 0:
			tableRef := strings.TrimSpace(raw[:usingIdx])
			usingExpr := strings.TrimSpace(raw[usingIdx+5:])
			result += " " + jt + " JOIN " + tableRef + " USING " + usingExpr
		default:
			// No ON/USING — bare JOIN treated as CROSS JOIN
			result += " CROSS JOIN " + resolveSegment(raw)
		}
	}
	return result
}

// rewriteFromParen handles MySQL's nested parenthesized FROM … JOIN syntax,
// converting it to PostgreSQL-compatible JOIN expressions. It scans every
// `FROM (` in the statement; a FROM whose inner group begins with SELECT is
// treated as a pure subquery and skipped, while mixed forms like
// `FROM (t a JOIN (SELECT …) b)` are rewritten (the subquery operand is
// preserved verbatim by splitTopLevelJoin/buildFromClause).
func rewriteFromParen(def string) string {
	reFindFrom := regexp.MustCompile(`(?i)\bFROM\s*\(`)
	reStartsWithSelect := regexp.MustCompile(`(?i)^SELECT\b`)
	pos := 0
	for pos < len(def) {
		loc := reFindFrom.FindStringIndex(def[pos:])
		if loc == nil {
			return def
		}
		absFromStart := pos + loc[0]
		openIdx := pos + loc[1] - 1 // index of the '('
		depth := 0
		closeIdx := -1
		for i := openIdx; i < len(def); i++ {
			switch def[i] {
			case '(':
				depth++
			case ')':
				depth--
				if depth == 0 {
					closeIdx = i
				}
			}
			if closeIdx >= 0 {
				break
			}
		}
		if closeIdx < 0 {
			return def // unbalanced — bail out safely
		}
		inner := def[openIdx+1 : closeIdx]
		trimmed := strings.TrimSpace(fullyStripOuterParens(inner))
		if reStartsWithSelect.MatchString(trimmed) {
			pos = closeIdx + 1 // pure subquery — skip past the close paren
			continue
		}
		parts := splitTopLevelJoin(trimmed)
		rewritten := buildFromClause(parts)
		before := def[:absFromStart]
		after := def[closeIdx+1:]
		def = before + "FROM " + rewritten + after
		pos = absFromStart // resume scanning from the rewritten position
	}
	return def
}

// applySchemaMapping 将视图定义中跨 schema 的表前缀按 mapping 替换。
// mapping: key=源 schema（小写），value=目标 schema（空串表示删除前缀）。
// 仅在 SQL 字符串字面量（单引号包裹）之外发生替换。
func applySchemaMapping(def string, mapping map[string]string) string {
	if len(mapping) == 0 {
		return def
	}
	type rule struct {
		re   *regexp.Regexp
		repl string
	}
	rules := make([]rule, 0, len(mapping))
	for src, dst := range mapping {
		if src == "" || src == dst {
			continue
		}
		// 左侧必须是行首或非词字符；右侧必须紧跟一个合法标识符。
		pat := fmt.Sprintf(`(?i)(^|[^A-Za-z0-9_])%s\.([A-Za-z_][A-Za-z0-9_]*)`,
			regexp.QuoteMeta(src))
		repl := `${1}${2}`
		if dst != "" {
			repl = `${1}` + dst + `.${2}`
		}
		rules = append(rules, rule{regexp.MustCompile(pat), repl})
	}
	// 按单引号切分，奇数段是字面量，跳过不替换。
	parts := strings.Split(def, "'")
	for i := 0; i < len(parts); i += 2 {
		for _, r := range rules {
			parts[i] = r.re.ReplaceAllString(parts[i], r.repl)
		}
	}
	return strings.Join(parts, "'")
}

// transformViewDef 将 MySQL 视图定义（经过反引号/schema前缀/convert初步清理后）
// 改写为 PostgreSQL 兼容的 SELECT 语句体。
func transformViewDef(def string) string {
	// 1. 处理 MySQL FROM 子句中的嵌套括号和隐式 CROSS JOIN 语法
	def = rewriteFromParen(def)

	// 2. ifnull(x, y) → coalesce(x, y)
	def = regexp.MustCompile(`(?i)\bifnull\s*\(`).ReplaceAllString(def, "coalesce(")

	// 3. isnull(x) → (x IS NULL)
	def = regexp.MustCompile(`(?i)\bisnull\s*\(([^)]+)\)`).ReplaceAllString(def, "($1 IS NULL)")

	// 4. group_concat(x separator 'sep') → string_agg(x, 'sep')
	def = regexp.MustCompile(`(?i)\bgroup_concat\s*\((.+?)\s+separator\s+('[^']*')\s*\)`).ReplaceAllString(def, "string_agg($1, $2)")
	def = regexp.MustCompile(`(?i)\bgroup_concat\s*\(`).ReplaceAllString(def, "string_agg(")

	// 5. date_format(x, fmt) → to_char(x, fmt)，并替换常见格式符
	def = regexp.MustCompile(`(?i)\bdate_format\s*\(`).ReplaceAllString(def, "to_char(")
	def = strings.ReplaceAll(def, "%Y", "YYYY")
	def = strings.ReplaceAll(def, "%m", "MM")
	def = strings.ReplaceAll(def, "%d", "DD")
	def = strings.ReplaceAll(def, "%H", "HH24")
	def = strings.ReplaceAll(def, "%i", "MI")
	def = strings.ReplaceAll(def, "%s", "SS")

	// 6. if(cond, a, b) → CASE WHEN cond THEN a ELSE b END
	def = regexp.MustCompile(`(?i)\bif\s*\(([^,]+),\s*([^,]+),\s*([^)]+)\)`).ReplaceAllString(def, "CASE WHEN $1 THEN $2 ELSE $3 END")

	// 7. 剥离 MySQL 特有的 CHARSET 子句（出现在 CAST / 类型声明中）
	//    cast('' as char(32) charset utf8mb4) → cast('' as char(32))
	def = regexp.MustCompile(`(?i)\s+charset\s+\w+`).ReplaceAllString(def, "")

	return def
}
func (tb *Table) TriggerCreate(logDir string) (result []string) {
	id := 0
	failedCount := 0
	startTime := time.Now()
	var createSql string
	// 查询触发器，批量生成创建语句
	sql := fmt.Sprintf("SELECT replace(lower(concat('create or replace trigger ',trigger_name,' ',action_timing,' ',event_manipulation,' on \"',event_object_table,'\" for each row as ',action_statement)),'#','-- ') FROM information_schema.triggers WHERE trigger_schema=database();")
	//fmt.Println(sql)
	rows, err := srcDb.Query(sql)
	if err != nil {
		log.Error(err)
	}
	// 从sql结果集遍历，获取到创建语句
	for rows.Next() {
		id += 1
		if err := rows.Scan(&createSql); err != nil {
			log.Error(err)
		}
		// 创建目标触发器
		log.Info(fmt.Sprintf("%v ProcessingID %s create trigger %s", time.Now().Format("2006-01-02 15:04:05.000000"), strconv.Itoa(id), createSql))
		if _, err = execDest(createSql); err != nil {
			log.Error(createSql, " create trigger failed ", err)
			LogError(logDir, "TriggerCreateFailed", createSql, err)
			failedCount += 1
		}
	}
	log.Info("trigger count ", id)
	endTime := time.Now()
	cost := time.Since(startTime)
	result = append(result, "Trigger", startTime.Format("2006-01-02 15:04:05.000000"), endTime.Format("2006-01-02 15:04:05.000000"), strconv.Itoa(failedCount), cost.String())
	return result
}

// TableCreate 单线程创建表
//func (tb *Table) TableCreate(logDir string, tableMap map[string][]string) (result []string) {
//	tableCount := 0
//	startTime := time.Now()
//	failedCount := 0
//	// 获取tableMap键值对中的表名
//	for tblName, _ := range tableMap {
//		var colTotal int
//		tableCount += 1
//		pgCreateTbl := "create table " + tblName + "("
//		// 查询当前表总共有多少个列字段
//		colTotalSql := fmt.Sprintf("select count(*) from information_schema.COLUMNS  where table_schema=database() and table_name='%s'", tblName)
//		err := srcDb.QueryRow(colTotalSql).Scan(&colTotal)
//		if err != nil {
//			log.Error(err)
//		}
//		// 查询MySQL表结构
//		sql := fmt.Sprintf("select concat('\"',lower(column_name),'\"'),data_type,ifnull(character_maximum_length,'null'),is_nullable,case  column_default when '( \\'user\\' )' then 'user' else ifnull(column_default,'null') end as column_default,ifnull(numeric_precision,'null'),ifnull(numeric_scale,'null'),ifnull(datetime_precision,'null'),ifnull(column_key,'null'),ifnull(column_comment,'null'),ORDINAL_POSITION from information_schema.COLUMNS where table_schema=database() and table_name='%s'", tblName)
//		//fmt.Println(sql)
//		rows, err := srcDb.Query(sql)
//		if err != nil {
//			log.Error(err)
//		}
//		// 遍历MySQL表字段,一行就是一个字段的基本信息
//		for rows.Next() {
//			if err := rows.Scan(&tb.columnName, &tb.dataType, &tb.characterMaximumLength, &tb.isNullable, &tb.columnDefault, &tb.numericPrecision, &tb.numericScale, &tb.datetimePrecision, &tb.columnKey, &tb.columnComment, &tb.ordinalPosition); err != nil {
//				log.Error(err)
//			}
//			//fmt.Println(columnName,dataType,characterMaximumLength,isNullable,columnDefault,numericPrecision,numericScale,datetimePrecision,columnKey,columnComment,ordinalPosition)
//			//适配MySQL字段类型到PostgreSQL字段类型
//			// 列字段是否允许null
//			switch tb.isNullable {
//			case "NO":
//				tb.destNullable = "not null"
//			default:
//				tb.destNullable = "null"
//			}
//			// 列字段default默认值的处理
//			switch {
//			case tb.columnDefault != "null": // 默认值不是null并且是字符串类型下面就需要使用fmt.Sprintf格式化让字符串单引号包围，否则这个字符串是没有引号包围的
//				if tb.dataType == "varchar" {
//					tb.destDefault = fmt.Sprintf("default '%s'", tb.columnDefault)
//				} else if tb.dataType == "char" {
//					tb.destDefault = fmt.Sprintf("default '%s'", tb.columnDefault)
//				} else {
//					tb.destDefault = fmt.Sprintf("default %s", tb.columnDefault) // 非字符串类型无需使用单引号包围
//				}
//			default:
//				tb.destDefault = "" // 如果没有默认值，默认值就是空字符串，即目标没有默认值
//			}
//			// 列字段类型的处理
//			switch tb.dataType {
//			case "int", "mediumint", "tinyint":
//				tb.destType = "int"
//			case "varchar":
//				tb.destType = "varchar(" + tb.characterMaximumLength + ")"
//			case "char":
//				tb.destType = "char(" + tb.characterMaximumLength + ")"
//			case "text", "tinytext", "mediumtext", "longtext":
//				tb.destType = "text"
//			case "datetime", "timestamp":
//				tb.destType = "timestamp"
//			case "decimal", "double", "float":
//				if tb.numericScale == "null" {
//					tb.destType = "decimal(" + tb.numericPrecision + ")"
//				} else {
//					tb.destType = "decimal(" + tb.numericPrecision + "," + tb.numericScale + ")"
//				}
//			case "tinyblob", "blob", "mediumblob", "longblob":
//				tb.destType = "bytea"
//			// 其余类型，源库使用什么类型，目标库就使用什么类型
//			default:
//				tb.destType = tb.dataType
//			}
//			// 在目标库创建的语句
//			pgCreateTbl += fmt.Sprintf("%s %s %s %s,", tb.columnName, tb.destType, tb.destNullable, tb.destDefault)
//			if tb.ordinalPosition == colTotal {
//				pgCreateTbl = pgCreateTbl[:len(pgCreateTbl)-1] + ")" // 最后一个列字段结尾去掉逗号,并且加上语句的右括号
//			}
//		}
//		//fmt.Println(pgCreateTbl) // 打印创建表语句
//		// 创建前先删除目标表
//		dropDestTbl := "drop table if exists " + tblName + " cascade"
//		if _, err = destDb.Exec(dropDestTbl); err != nil {
//			log.Error(err)
//		}
//		// 创建PostgreSQL表结构
//		log.Info("Processing ID " + strconv.Itoa(tableCount) + " create table " + tblName)
//		if _, err = destDb.Exec(pgCreateTbl); err != nil {
//			log.Error("table ", tblName, " create failed", err)
//			failedCount += 1
//			LogError(logDir, "tableCreateFailed", pgCreateTbl, err)
//		}
//	}
//	endTime := time.Now()
//	cost := time.Since(startTime)
//	log.Info("Table structure synced from MySQL to PostgreSQL ,Source Table Total ", tableCount, " Failed Total ", strconv.Itoa(failedCount))
//	result = append(result, "Table", startTime.Format("2006-01-02 15:04:05.000000"), endTime.Format("2006-01-02 15:04:05.000000"), strconv.Itoa(failedCount), cost.String())
//	return result
//}
