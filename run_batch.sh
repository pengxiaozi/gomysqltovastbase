#!/usr/bin/env bash
# 顺序迁移 configs/ 下所有 yml，每对独立日志，单对失败不中断后续。
# 用法: bash run_batch.sh [config_dir] [mode]
#   mode: migrate (默认) | dryrun (仅做只读预检，不创建对象不迁移数据)

CONFIG_DIR="${1:-configs}"
MODE="${2:-migrate}"
BINARY="./gomysql2pg"
TS="$(date +%Y%m%d-%H%M%S)"
case "$MODE" in
    migrate) BATCH_LOG="batch-${TS}.log" ;;
    dryrun)  BATCH_LOG="batch-dryrun-${TS}.log" ;;
    *) echo "unknown mode: $MODE (use 'migrate' or 'dryrun')" >&2; exit 2 ;;
esac

# 从 yml 中读取 section.key（section=src|dest，只支持二层缩进、无嵌套 map）
yml_get() {
    local file="$1" section="$2" key="$3"
    awk -v sec="$section" -v k="$key" '
        /^[A-Za-z_][A-Za-z0-9_]*:[[:space:]]*$/ {
            cur = $0; sub(/:.*/, "", cur)
            in_sec = (cur == sec)
            next
        }
        /^[^[:space:]]/ { in_sec = 0; next }
        in_sec {
            line = $0
            sub(/^[[:space:]]+/, "", line)
            if (match(line, /^[A-Za-z_][A-Za-z0-9_]*:/)) {
                name = substr(line, 1, RLENGTH - 1)
                if (name == k) {
                    val = substr(line, RLENGTH + 1)
                    sub(/^[[:space:]]+/, "", val)
                    sub(/[[:space:]]+$/, "", val)
                    gsub(/^"|"$/, "", val)
                    gsub(/^'\''|'\''$/, "", val)
                    print val
                    exit
                }
            }
        }
    ' "$file"
}

if [[ ! -x "$BINARY" ]]; then
    echo "binary not found or not executable: $BINARY" >&2
    exit 2
fi
if [[ ! -d "$CONFIG_DIR" ]]; then
    echo "config dir not found: $CONFIG_DIR" >&2
    exit 2
fi

shopt -s nullglob
files=("$CONFIG_DIR"/*.yml "$CONFIG_DIR"/*.yaml)
if [[ ${#files[@]} -eq 0 ]]; then
    echo "no yml files under $CONFIG_DIR" >&2
    exit 2
fi
IFS=$'\n' files=($(printf '%s\n' "${files[@]}" | sort))
unset IFS

# 预览待迁移的配置文件
echo "Found ${#files[@]} config(s) to migrate:"
i=1
for f in "${files[@]}"; do
    s_host=$(yml_get "$f" src  host)
    s_port=$(yml_get "$f" src  port)
    s_db=$(  yml_get "$f" src  database)
    d_host=$(yml_get "$f" dest host)
    d_port=$(yml_get "$f" dest port)
    d_db=$(  yml_get "$f" dest database)
    d_user=$(yml_get "$f" dest username)
    printf "  [%d] %s  |  src=%s:%s/%s  ->  dest=%s@%s:%s/%s\n" \
        "$i" "$f" \
        "${s_host:-?}" "${s_port:-?}" "${s_db:-?}" \
        "${d_user:-?}" "${d_host:-?}" "${d_port:-?}" "${d_db:-?}"
    i=$((i + 1))
done
echo

# 交互式确认（dryrun 模式只读，无需确认）
if [[ "$MODE" != "dryrun" ]]; then
    read -r -p "Proceed with these ${#files[@]} migration(s)? [yes/no]: " answer
    answer=$(printf '%s' "$answer" | tr '[:upper:]' '[:lower:]')
    case "$answer" in
        yes|y) ;;
        *) echo "aborted by user"; exit 1 ;;
    esac
fi

succ=()
fail=()
missing_schemas=()

{
    echo "=== batch start $(date '+%Y-%m-%d %H:%M:%S') mode=${MODE} total=${#files[@]} ==="
    for f in "${files[@]}"; do
        echo
        echo "============================================================"
        echo ">>> [$(date '+%H:%M:%S')] running: $f"
        echo "============================================================"
        out=$(mktemp)
        if [[ "$MODE" == "dryrun" ]]; then
            "$BINARY" --config "$f" dryRun 2>&1 | tee "$out"
            rc=${PIPESTATUS[0]}
        else
            "$BINARY" --config "$f" 2>&1 | tee "$out"
            rc=${PIPESTATUS[0]}
        fi
        if [[ $rc -eq 0 ]]; then
            echo "<<< [OK] $f"
            succ+=("$f")
        else
            echo "<<< [FAIL rc=$rc] $f"
            fail+=("$f")
            if grep -q 'no schema named' "$out"; then
                d_user=$(yml_get "$f" dest username)
                if [[ -n "$d_user" ]]; then
                    missing_schemas+=("$d_user")
                fi
            fi
        fi
        rm -f "$out"
    done

    echo
    echo "=== batch done $(date '+%Y-%m-%d %H:%M:%S') ==="
    echo "success: ${#succ[@]}"
    for f in "${succ[@]}"; do echo "  OK   $f"; done
    echo "failed:  ${#fail[@]}"
    for f in "${fail[@]}"; do echo "  FAIL $f"; done

    if [[ ${#missing_schemas[@]} -gt 0 ]]; then
        declare -A seen=()
        uniq=()
        for u in "${missing_schemas[@]}"; do
            if [[ -z "${seen[$u]:-}" ]]; then
                seen[$u]=1
                uniq+=("$u")
            fi
        done
        echo
        echo "=== missing same-name schema(s) detected ==="
        echo "Run the following SQL on the destination database as a privileged user:"
        echo
        for u in "${uniq[@]}"; do
            printf "create user %s with password '123456';\n" "$u"
            printf "create schema %s authorization %s;\n" "$u" "$u"
        done
        echo
        echo "(Replace '123456' with a real password before executing.)"
    fi
} 2>&1 | tee "$BATCH_LOG"

exit ${#fail[@]}
