package cmd

import (
	"testing"
)

func TestTransformViewDef(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "nested parenthesized FROM with implicit cross join",
			input: "select b.elementname AS elementname,b.elementurl AS elementurl from ((personal_portal_element a join app_element b) join app_portal_element c on(((c.elementguid = b.RowGuid) and (a.ptrowguid = c.rowguid))))",
			want:  "select b.elementname AS elementname,b.elementurl AS elementurl FROM personal_portal_element a CROSS JOIN app_element b INNER JOIN app_portal_element c ON (((c.elementguid = b.RowGuid) and (a.ptrowguid = c.rowguid)))",
		},
		{
			name:  "simple flat join followed by WHERE",
			input: "select a.id from (t1 a join t2 b) where a.id=1",
			want:  "select a.id FROM t1 a CROSS JOIN t2 b where a.id=1",
		},
		{
			name:  "no parenthesized FROM — no change",
			input: "select a.id from t1 a, t2 b where a.id=b.id",
			want:  "select a.id from t1 a, t2 b where a.id=b.id",
		},
		{
			name:  "subquery guard — leave unchanged",
			input: "select * from (select id from t1) sub where sub.id=1",
			want:  "select * from (select id from t1) sub where sub.id=1",
		},
		{
			name:  "explicit join with ON — preserve join type",
			input: "select a.id from (t1 a left join t2 b on(a.id=b.id))",
			want:  "select a.id FROM t1 a LEFT JOIN t2 b ON (a.id=b.id)",
		},
		{
			name:  "ifnull conversion",
			input: "select ifnull(a,0) from t",
			want:  "select coalesce(a,0) from t",
		},
		{
			name:  "date_format conversion",
			input: "select date_format(d,'%Y-%m-%d') from t",
			want:  "select to_char(d,'YYYY-MM-DD') from t",
		},
		{
			name:  "cast with charset clause — strip charset",
			input: "select cast('' as char(32) charset utf8mb4) as x from t",
			want:  "select cast('' as char(32)) as x from t",
		},
		{
			name:  "cast with charset uppercase",
			input: "select CAST(a AS CHAR(64) CHARSET utf8) from t",
			want:  "select CAST(a AS CHAR(64)) from t",
		},
		{
			name:  "cast with charset and no length",
			input: "select cast(a as char charset utf8mb4) from t",
			want:  "select cast(a as char) from t",
		},
		{
			name:  "join with subquery operand — rewrite outer, preserve subquery body",
			input: "select e.x, d.y from (t1 e join (select 1 as id from t0) d) where (e.id = d.id)",
			want:  "select e.x, d.y FROM t1 e CROSS JOIN (select 1 as id from t0) d where (e.id = d.id)",
		},
		{
			name:  "deeply nested left-associated joins",
			input: "select * from (((bp_department d0 join bp_department d1) join bp_department d2) join bp_department d3) where (d1.parent_id=0)",
			want:  "select * FROM bp_department d0 CROSS JOIN bp_department d1 CROSS JOIN bp_department d2 CROSS JOIN bp_department d3 where (d1.parent_id=0)",
		},
		{
			name:  "multiple FROM ( — UNION subquery body also rewritten",
			input: "select * from (a x join (select b.id from (b y join c z) where (b.x=1) union select c.id from ((b y join c z) join d w) where (b.x=2)) d)",
			want:  "select * FROM a x CROSS JOIN (select b.id FROM b y CROSS JOIN c z where (b.x=1) union select c.id FROM b y CROSS JOIN c z CROSS JOIN d w where (b.x=2)) d",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := transformViewDef(tc.input)
			if got != tc.want {
				t.Errorf("\ninput: %s\n  got: %s\n want: %s", tc.input, got, tc.want)
			}
		})
	}
}

func TestStripOuterParens(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"(abc)", "abc"},
		{"((abc))", "(abc)"},
		{"(a) join (b)", "(a) join (b)"},
		{"abc", "abc"},
		{"", ""},
	}
	for _, tc := range tests {
		got := stripOuterParens(tc.input)
		if got != tc.want {
			t.Errorf("stripOuterParens(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestFullyStripOuterParens(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"(((abc)))", "abc"},
		{"(a) join (b)", "(a) join (b)"},
		{"((a join b) join c)", "(a join b) join c"},
	}
	for _, tc := range tests {
		got := fullyStripOuterParens(tc.input)
		if got != tc.want {
			t.Errorf("fullyStripOuterParens(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestApplySchemaMapping(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		mapping map[string]string
		want    string
	}{
		{
			name:    "删除前缀-基本场景",
			input:   "select schema_b.t1.id from schema_b.t1 where schema_b.t1.x=1",
			mapping: map[string]string{"schema_b": ""},
			want:    "select t1.id from t1 where t1.x=1",
		},
		{
			name:    "替换为目标 schema",
			input:   "select legacy.t1.id from legacy.t1",
			mapping: map[string]string{"legacy": "public"},
			want:    "select public.t1.id from public.t1",
		},
		{
			name:    "用户案例-完整视图",
			input:   "select schema_b.agentdropstartivrcfg.ID AS ID from schema_b.agentdropstartivrcfg where (schema_b.agentdropstartivrcfg.TenantID = (select mytenantinfo.TenantID from mytenantinfo))",
			mapping: map[string]string{"schema_b": ""},
			want:    "select agentdropstartivrcfg.ID AS ID from agentdropstartivrcfg where (agentdropstartivrcfg.TenantID = (select mytenantinfo.TenantID from mytenantinfo))",
		},
		{
			name:    "字符串字面量内不替换",
			input:   "select 'schema_b.foo' as note, schema_b.t1.id from schema_b.t1",
			mapping: map[string]string{"schema_b": ""},
			want:    "select 'schema_b.foo' as note, t1.id from t1",
		},
		{
			name:    "大小写不敏感匹配",
			input:   "select Schema_B.T1.id from SCHEMA_B.t1",
			mapping: map[string]string{"schema_b": ""},
			want:    "select T1.id from t1",
		},
		{
			name:    "未配置时不修改",
			input:   "select schema_b.t1.id from schema_b.t1",
			mapping: map[string]string{},
			want:    "select schema_b.t1.id from schema_b.t1",
		},
		{
			name:    "源等于目标-幂等",
			input:   "select schema_b.t1.id from schema_b.t1",
			mapping: map[string]string{"schema_b": "schema_b"},
			want:    "select schema_b.t1.id from schema_b.t1",
		},
		{
			name:    "前缀子串不误伤",
			input:   "select myschema_b.t1.id from myschema_b.t1",
			mapping: map[string]string{"schema_b": ""},
			want:    "select myschema_b.t1.id from myschema_b.t1",
		},
		{
			name:    "右侧非标识符不命中",
			input:   "select schema_b.123 from t",
			mapping: map[string]string{"schema_b": ""},
			want:    "select schema_b.123 from t",
		},
		{
			name:    "多 schema 同时映射",
			input:   "select a.t1.x, b.t2.y from a.t1 join b.t2",
			mapping: map[string]string{"a": "ns1", "b": ""},
			want:    "select ns1.t1.x, t2.y from ns1.t1 join t2",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := applySchemaMapping(tc.input, tc.mapping)
			if got != tc.want {
				t.Errorf("\ninput: %s\n  got: %s\n want: %s", tc.input, got, tc.want)
			}
		})
	}
}
