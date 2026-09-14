#!/usr/bin/env bash
# 扫描 log/ 下所有子目录，列出含有失败日志文件的目录。
# 用法: bash check_log.sh [log_dir]

set -euo pipefail

LOG_DIR="${1:-log}"

FAIL_LOGS=(
    "tableCreateFailed.log"
    "seqCreateFailed.log"
    "idxCreateFailed.log"
    "DistributedAlterFailed.log"
    "FkCreateFailed.log"
    "viewCreateFailed.log"
    "TriggerCreateFailed.log"
    "failedTable.log"
    "errorTableData.log"
    "dryRunFailed.log"
)

WARN_LOGS=(
    "invalidTableData.log"
)

if [ ! -d "$LOG_DIR" ]; then
    echo "log directory not found: $LOG_DIR" >&2
    exit 2
fi

dirs=()
while IFS= read -r -d '' d; do
    dirs+=("$d")
done < <(find "$LOG_DIR" -mindepth 1 -maxdepth 1 -type d -print0 | sort -z)

if [ ${#dirs[@]} -eq 0 ]; then
    echo "no subdirectories under $LOG_DIR"
    exit 0
fi

echo "Scanning log directory: ${LOG_DIR}/"
echo ""

total_count=0
fail_count=0

for d in "${dirs[@]}"; do
    total_count=$((total_count + 1))
    dir_name=$(basename "$d")
    found=()
    warns=()

    for log_file in "${FAIL_LOGS[@]}"; do
        p="$d/$log_file"
        if [ -f "$p" ]; then
            lines=$(wc -l < "$p" | tr -d ' ')
            found+=("$(printf "       %-35s (%s lines)" "$log_file" "$lines")")
        fi
    done

    for log_file in "${WARN_LOGS[@]}"; do
        p="$d/$log_file"
        if [ -f "$p" ]; then
            lines=$(wc -l < "$p" | tr -d ' ')
            warns+=("$(printf "       %-35s (%s lines) [WARN]" "$log_file" "$lines")")
        fi
    done

    if [ ${#found[@]} -gt 0 ]; then
        fail_count=$((fail_count + 1))
        echo "[FAIL] $dir_name"
        for line in "${found[@]}"; do
            echo "$line"
        done
        if [ ${#warns[@]} -gt 0 ]; then
            for line in "${warns[@]}"; do
                echo "$line"
            done
        fi
        echo ""
    elif [ ${#warns[@]} -gt 0 ]; then
        echo "[WARN] $dir_name"
        for line in "${warns[@]}"; do
            echo "$line"
        done
        echo ""
    else
        echo "[OK]   $dir_name"
    fi
done

echo ""
echo "--- Summary ---"
echo "Total:   $total_count"
echo "Failed:  $fail_count"
echo "OK:      $((total_count - fail_count))"

exit $fail_count
