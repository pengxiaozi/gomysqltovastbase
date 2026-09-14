#!/usr/bin/env bash
# 从 configs/db_config.csv 读取每行连接信息，生成对应的 yml 配置文件。
# 用法: bash gen_configs.sh [csv_file] [out_dir]
# 注意: 请使用 bash 调用 (脚本依赖 [[ ]]、${var%$'\r'} 等扩展)，避免 sh 模式下兼容问题。

set -euo pipefail

CSV="${1:-configs/db_config.csv}"
OUT_DIR="${2:-configs}"

DB_TYPE="Gauss"
PAGE_SIZE=100000
MAX_PARALLEL=32

if [[ ! -f "$CSV" ]]; then
    echo "csv not found: $CSV" >&2
    exit 2
fi
mkdir -p "$OUT_DIR"

dup_out=$(awk -F, '
    NR == 1 { next }
    {
        line = $0
        sub(/\r$/, "", line)
        if (line == "") next
        if (line in seen) {
            printf "duplicate row detected: line %d duplicates line %d\n", NR, seen[line]
            printf "  content: %s\n", line
            dup = 1
        } else {
            seen[line] = NR
        }
    }
    END { exit (dup ? 1 : 0) }
' "$CSV") || {
    printf '%s\n' "$dup_out" >&2
    echo "aborted: please fix duplicate rows in $CSV" >&2
    exit 3
}

idx=0
gen=0
while IFS=',' read -r s_host s_port s_db s_user s_pwd d_host d_port d_db d_user d_pwd || [[ -n "${s_host:-}" ]]; do
    s_host=${s_host%$'\r'}
    s_port=${s_port%$'\r'}
    s_db=${s_db%$'\r'}
    s_user=${s_user%$'\r'}
    s_pwd=${s_pwd%$'\r'}
    d_host=${d_host%$'\r'}
    d_port=${d_port%$'\r'}
    d_db=${d_db%$'\r'}
    d_user=${d_user%$'\r'}
    d_pwd=${d_pwd%$'\r'}

    idx=$((idx + 1))
    [[ $idx -eq 1 ]] && continue
    [[ -z "$s_host" ]] && continue
    if [[ -z "$d_pwd" ]]; then
        echo "warn: line $idx incomplete, skip" >&2
        continue
    fi

    seq=$(printf "%03d" $((idx - 1)))
    out="$OUT_DIR/${seq}_${s_db}.yml"

    # 交互式询问是否配置 schemaMapping（使用平行索引数组以兼容 bash 3.2）
    schema_keys=()
    schema_vals=()
    read -r -p "Enable schemaMapping for ${s_db}? [y/N]: " enable_mapping </dev/tty
    enable_mapping=$(echo "$enable_mapping" | tr '[:upper:]' '[:lower:]')

    if [[ "$enable_mapping" == "y" || "$enable_mapping" == "yes" ]]; then
        echo "  Enter schema mappings (source -> target). Press Enter on empty source to finish."
        while true; do
            read -r -p "  Source schema (empty to finish) [$s_db]: " src_schema </dev/tty
            src_schema=${src_schema:-$s_db}
            [[ -z "$src_schema" ]] && break

            read -r -p "  Target schema (empty string to remove prefix) [$d_user]: " tgt_schema </dev/tty
            tgt_schema=${tgt_schema:-$d_user}

            schema_keys+=("$src_schema")
            schema_vals+=("$tgt_schema")
            echo "    Added: $src_schema -> $tgt_schema"
        done
    fi

    cat >"$out" <<EOF
src:
  host: "$s_host"
  port: $s_port
  database: "$s_db"
  username: "$s_user"
  password: "$s_pwd"

dest:
  dbType: $DB_TYPE
  host: $d_host
  port: $d_port
  database: $d_db
  username: $d_user
  password: $d_pwd

pageSize: $PAGE_SIZE
maxParallel: $MAX_PARALLEL
charInLength: false
useNvarchar2: true
Distributed: false
tables:
  pres_fieldinfo:
    - select * from pres_fieldinfo
exclude:
  - 'xmllog_copy1'
  - 'interfacecalllog_copy1'
  - '*_cswysk'
EOF

    # 动态写入 schemaMapping（如果有）
    if [[ ${#schema_keys[@]} -gt 0 ]]; then
        echo "schemaMapping:" >>"$out"
        i=0
        while [[ $i -lt ${#schema_keys[@]} ]]; do
            src="${schema_keys[$i]}"
            tgt="${schema_vals[$i]}"
            if [[ -z "$tgt" ]]; then
                echo "  $src: \"\"" >>"$out"
            else
                echo "  $src: $tgt" >>"$out"
            fi
            i=$((i + 1))
        done
    fi

    gen=$((gen + 1))
done < "$CSV"

echo "generated $gen yml file(s) under $OUT_DIR"
