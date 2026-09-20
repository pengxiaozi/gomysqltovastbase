package cmd

import (
	"strings"
	"testing"
)

// TestPGTypeToMySQL 覆盖 PG → MySQL 的类型映射。
// 重点是两类边界：MySQL 装不下的（数组）必须报错而不是猜，
// 以及带长度的类型不能被前缀规则误伤。
func TestPGTypeToMySQL(t *testing.T) {
	tests := []struct {
		name     string
		pgType   string
		enumVals []string
		want     string
		wantErr  bool
	}{
		// 整数族
		{"smallint", "smallint", nil, "smallint", false},
		{"integer", "integer", nil, "int", false},
		{"bigint", "bigint", nil, "bigint", false},
		{"int4 alias", "int4", nil, "int", false},
		{"oid", "oid", nil, "bigint", false},

		// 布尔
		{"boolean", "boolean", nil, "tinyint(1)", false},
		{"bool alias", "bool", nil, "tinyint(1)", false},

		// 浮点与定点
		{"real", "real", nil, "float", false},
		{"double precision", "double precision", nil, "double", false},
		{"numeric with scale", "numeric(10,2)", nil, "decimal(10,2)", false},
		{"numeric bare", "numeric", nil, "decimal(65,30)", false},

		// 字符类型：带长度不能被前缀规则吃掉
		{"varchar", "character varying(50)", nil, "varchar(50)", false},
		{"varchar over limit", "character varying(100000)", nil, "text", false},
		{"char", "character(10)", nil, "char(10)", false},
		{"char over limit", "character(300)", nil, "varchar(300)", false},
		{"text", "text", nil, "longtext", false},

		// 二进制与特殊类型
		{"bytea", "bytea", nil, "longblob", false},
		{"uuid", "uuid", nil, "char(36)", false},
		{"jsonb", "jsonb", nil, "json", false},
		{"inet", "inet", nil, "varchar(45)", false},

		// 时间类型：带不带时区一律落到 datetime
		{"timestamp without tz", "timestamp without time zone", nil, "datetime", false},
		{"timestamp with tz", "timestamp with time zone", nil, "datetime", false},
		{"date", "date", nil, "date", false},
		{"time", "time without time zone", nil, "time", false},

		// 自定义枚举：还原成 MySQL 的 ENUM(...)
		{"enum", "public.mood", []string{"happy", "sad"}, "enum('happy','sad')", false},
		{"enum with quote", "public.mood", []string{"it's"}, "enum('it''s')", false},

		// 装不下的必须报错，不能糊一个类型过去
		{"array", "integer[]", nil, "", true},
		{"text array", "text[]", nil, "", true},
		{"unknown", "tsvector", nil, "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := pgTypeToMySQL(tt.pgType, tt.enumVals)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("pgTypeToMySQL(%q) 期望报错，实际返回 %q", tt.pgType, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("pgTypeToMySQL(%q) 意外报错: %v", tt.pgType, err)
			}
			if got != tt.want {
				t.Errorf("pgTypeToMySQL(%q) = %q, want %q", tt.pgType, got, tt.want)
			}
		})
	}
}

// TestMyDefault 覆盖 PG 默认值 → MySQL 写法。
// 转不了的一律返回 false——保留一个错误的默认值比不要它更危险。
func TestMyDefault(t *testing.T) {
	tests := []struct {
		name      string
		pgDefault string
		want      string
		wantOK    bool
	}{
		{"now to current_timestamp", "now()", "CURRENT_TIMESTAMP", true},
		{"current_timestamp", "CURRENT_TIMESTAMP", "CURRENT_TIMESTAMP", true},
		{"current_timestamp with precision", "CURRENT_TIMESTAMP(3)", "CURRENT_TIMESTAMP", true},
		{"true to 1", "true", "1", true},
		{"false to 0", "false", "0", true},
		{"integer literal", "0", "0", true},
		{"negative literal", "-1", "-1", true},
		{"quoted literal", "'abc'", "'abc'", true},

		// 序列自增在 MySQL 里是 AUTO_INCREMENT，不能当默认值写
		{"nextval skipped", "nextval('t_id_seq'::regclass)", "", false},
		// 带类型转换的表达式 MySQL 没有对应写法
		{"cast expression skipped", "'abc'::character varying", "", false},
		{"unknown function skipped", "my_func()", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := myDefault(tt.pgDefault, "")
			if ok != tt.wantOK {
				t.Fatalf("myDefault(%q) ok = %v, want %v", tt.pgDefault, ok, tt.wantOK)
			}
			if ok && got != tt.want {
				t.Errorf("myDefault(%q) = %q, want %q", tt.pgDefault, got, tt.want)
			}
		})
	}
}

// TestCheckNameCollisions 命名冲突是反向迁移唯一无法自动绕过的限制，
// 必须在建表前拦下来，否则会静默丢列。
func TestCheckNameCollisions(t *testing.T) {
	tests := []struct {
		name        string
		tables      []pgTable
		wantCount   int
		wantContain string
	}{
		{
			name:      "no collision",
			tables:    []pgTable{{Name: "t1", Columns: []pgColumn{{Name: "id"}, {Name: "name"}}}},
			wantCount: 0,
		},
		{
			name:        "column case collision",
			tables:      []pgTable{{Name: "t1", Columns: []pgColumn{{Name: "Name"}, {Name: "name"}}}},
			wantCount:   1,
			wantContain: "同时有列",
		},
		{
			name:        "table case collision",
			tables:      []pgTable{{Name: "SysUser"}, {Name: "sysuser"}},
			wantCount:   1,
			wantContain: "表名",
		},
		{
			name: "same case twice is fine",
			tables: []pgTable{
				{Name: "t1", Columns: []pgColumn{{Name: "Id"}}},
				{Name: "t2", Columns: []pgColumn{{Name: "Id"}}},
			},
			wantCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := checkNameCollisions(tt.tables)
			if len(got) != tt.wantCount {
				t.Fatalf("checkNameCollisions() 返回 %d 条，期望 %d 条: %v",
					len(got), tt.wantCount, got)
			}
			if tt.wantContain != "" && !strings.Contains(got[0], tt.wantContain) {
				t.Errorf("提示信息里应包含 %q，实际: %s", tt.wantContain, got[0])
			}
		})
	}
}

// TestQuoteForMySQL 标识符与字面量的转义。
// 反引号在标识符里要双写，反斜杠在 MySQL 字面量里有转义含义，都不能漏。
func TestQuoteForMySQL(t *testing.T) {
	if got := myIdent("userName"); got != "`userName`" {
		t.Errorf("myIdent 应保留大小写: %q", got)
	}
	if got := myIdent("a`b"); got != "`a``b`" {
		t.Errorf("myIdent 应双写反引号: %q", got)
	}
	if got := myLiteral("it's"); got != "'it''s'" {
		t.Errorf("myLiteral 应双写单引号: %q", got)
	}
	if got := myLiteral(`C:\path`); got != `'C:\\path'` {
		t.Errorf("myLiteral 应转义反斜杠: %q", got)
	}
	if got := pgIdent("Name"); got != `"Name"` {
		t.Errorf("pgIdent 应保留大小写: %q", got)
	}
}

// TestBuildMySQLCreateTable 建表语句的整体拼装。
func TestBuildMySQLCreateTable(t *testing.T) {
	tbl := pgTable{
		Name:    "sys_user",
		Comment: "用户表",
		PK:      []string{"ID"},
		Columns: []pgColumn{
			{Name: "ID", DataType: "integer", NotNull: true},
			{Name: "userName", DataType: "character varying(50)", NotNull: false, Comment: "用户名"},
			{Name: "amount", DataType: "numeric(10,2)", NotNull: false, Default: "0"},
		},
	}

	ddl, warnings, err := buildMySQLCreateTable(tbl, "preserve", "DYNAMIC")
	if err != nil {
		t.Fatalf("意外报错: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("不该有告警: %v", warnings)
	}
	for _, want := range []string{
		"CREATE TABLE `sys_user` (",
		"`ID` int NOT NULL",
		"`userName` varchar(50) NULL COMMENT '用户名'",
		"`amount` decimal(10,2) NULL DEFAULT 0",
		"PRIMARY KEY (`ID`)",
		"COMMENT='用户表'",
		"ROW_FORMAT=DYNAMIC",
	} {
		if !strings.Contains(ddl, want) {
			t.Errorf("建表语句里应包含 %q\n实际:\n%s", want, ddl)
		}
	}
}

// TestBuildMySQLCreateTableFailsOnArray 数组类型必须让整张表建不出来，
// 而不是悄悄降级成某个字符类型。
func TestBuildMySQLCreateTableFailsOnArray(t *testing.T) {
	tbl := pgTable{
		Name:    "t",
		Columns: []pgColumn{{Name: "tags", DataType: "text[]", NotNull: false}},
	}
	if _, _, err := buildMySQLCreateTable(tbl, "preserve", ""); err == nil {
		t.Fatal("数组类型应该报错")
	}
}

// TestAutoIncrement 自增列的两种情形。
// MySQL 要求 AUTO_INCREMENT 列必须是一个键，不是键的只能降级并报出。
func TestAutoIncrement(t *testing.T) {
	tests := []struct {
		name        string
		table       pgTable
		wantContain string
		wantWarn    bool
	}{
		{
			name: "主键上的自增直接写 AUTO_INCREMENT",
			table: pgTable{
				Name: "t", PK: []string{"id"},
				Columns: []pgColumn{{
					Name: "id", DataType: "integer", NotNull: true,
					Default: "nextval('t_id_seq'::regclass)", AutoIncrement: true,
				}},
			},
			wantContain: "AUTO_INCREMENT",
		},
		{
			name: "非主键的自增降级并告警",
			table: pgTable{
				Name: "t", PK: []string{"id"},
				Columns: []pgColumn{
					{Name: "id", DataType: "integer", NotNull: true},
					{
						Name: "seq_no", DataType: "integer", NotNull: true,
						Default: "nextval('s'::regclass)", AutoIncrement: true,
					},
				},
			},
			wantWarn: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ddl, warns, err := buildMySQLCreateTable(tt.table, "preserve", "")
			if err != nil {
				t.Fatalf("意外报错: %v", err)
			}
			if tt.wantContain != "" && !strings.Contains(ddl, tt.wantContain) {
				t.Errorf("建表语句应包含 %q\n%s", tt.wantContain, ddl)
			}
			if (len(warns) > 0) != tt.wantWarn {
				t.Errorf("告警情况不符，warnings=%v", warns)
			}
		})
	}
}

// TestBuildMySQLIndex 索引生成。
// 重点是"不支持的要明确跳过并给出原因"，不能静默少建。
func TestBuildMySQLIndex(t *testing.T) {
	tests := []struct {
		name      string
		idx       pgIndex
		wantDDL   string
		wantSkip  string // 非空表示应跳过，且这个子串出现在原因里
	}{
		{
			name:    "普通索引",
			idx:     pgIndex{Table: "t", Name: "idx_a", Method: "btree", Columns: []string{"a"}},
			wantDDL: "CREATE INDEX `idx_a` ON `t` (`a`)",
		},
		{
			name:    "唯一索引",
			idx:     pgIndex{Table: "t", Name: "uniq_a", Unique: true, Method: "btree", Columns: []string{"a", "b"}},
			wantDDL: "CREATE UNIQUE INDEX `uniq_a` ON `t` (`a`,`b`)",
		},
		{
			name:     "主键跳过",
			idx:      pgIndex{Table: "t", Name: "t_pkey", Primary: true, Method: "btree", Columns: []string{"id"}},
			wantSkip: "主键",
		},
		{
			name:     "表达式索引跳过",
			idx:      pgIndex{Table: "t", Name: "idx_lower", Method: "btree", Expr: "0 1"},
			wantSkip: "表达式索引",
		},
		{
			name:     "部分索引跳过",
			idx:      pgIndex{Table: "t", Name: "idx_part", Method: "btree", Columns: []string{"a"}, Predicate: "(a > 0)"},
			wantSkip: "部分索引",
		},
		{
			name:     "gin 索引跳过",
			idx:      pgIndex{Table: "t", Name: "idx_gin", Method: "gin", Columns: []string{"tags"}},
			wantSkip: "gin",
		},
		{
			name:     "hash 索引跳过",
			idx:      pgIndex{Table: "t", Name: "idx_hash", Method: "hash", Columns: []string{"a"}},
			wantSkip: "hash",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ddl, skip := buildMySQLIndex(tt.idx)
			if tt.wantSkip != "" {
				if skip == "" {
					t.Fatalf("期望跳过，实际生成了: %s", ddl)
				}
				if !strings.Contains(skip, tt.wantSkip) {
					t.Errorf("跳过原因应包含 %q，实际: %s", tt.wantSkip, skip)
				}
				return
			}
			if skip != "" {
				t.Fatalf("不该跳过: %s", skip)
			}
			if ddl != tt.wantDDL {
				t.Errorf("= %q, want %q", ddl, tt.wantDDL)
			}
		})
	}
}

// TestBuildMySQLFK 外键：PG 的 constraintdef 语法与 MySQL 基本一致，
// 只需换引号；带 MATCH 的 MySQL 不支持，要跳过。
func TestBuildMySQLFK(t *testing.T) {
	ddl, skip := buildMySQLFK(pgFK{
		Table: "child", Name: "fk_parent",
		Def: `FOREIGN KEY ("parentId") REFERENCES "parent"("ID") ON DELETE CASCADE`,
	})
	if skip != "" {
		t.Fatalf("不该跳过: %s", skip)
	}
	want := "ALTER TABLE `child` ADD CONSTRAINT `fk_parent` " +
		"FOREIGN KEY (`parentId`) REFERENCES `parent`(`ID`) ON DELETE CASCADE"
	if ddl != want {
		t.Errorf("= %q\nwant %q", ddl, want)
	}

	if _, skip := buildMySQLFK(pgFK{
		Table: "c", Name: "fk", HasMatch: true,
		Def: `FOREIGN KEY (a) REFERENCES p(b) MATCH FULL`,
	}); skip == "" {
		t.Error("带 MATCH 的外键应该跳过")
	}
}

// TestBuildMySQLView 视图只做引号转换，转不了的交给 MySQL 报错，
// 不猜、不降级。
func TestBuildMySQLView(t *testing.T) {
	got := buildMySQLView(pgView{
		Name: "v_user",
		Def:  `SELECT "ID", "Name" FROM "sys_user" WHERE "status" = 1;`,
	})
	want := "CREATE OR REPLACE VIEW `v_user` AS " +
		"SELECT `ID`, `Name` FROM `sys_user` WHERE `status` = 1;"
	if got != want {
		t.Errorf("\n= %q\nwant %q", got, want)
	}
}
