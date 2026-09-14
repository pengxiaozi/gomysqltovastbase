# 扫描 log/ 下所有子目录，列出含有失败日志文件的目录。
# 用法: powershell -ExecutionPolicy Bypass -File .\check_log.ps1 [-LogDir log]

[CmdletBinding()]
param(
    [string]$LogDir = "log"
)

$FailLogs = @(
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

$WarnLogs = @(
    "invalidTableData.log"
)

if (-not (Test-Path -LiteralPath $LogDir -PathType Container)) {
    [Console]::Error.WriteLine("log directory not found: $LogDir")
    exit 2
}

$dirs = Get-ChildItem -LiteralPath $LogDir -Directory | Sort-Object Name

if (-not $dirs -or $dirs.Count -eq 0) {
    Write-Host "no subdirectories under $LogDir"
    exit 0
}

Write-Host ("Scanning log directory: {0}/" -f $LogDir)
Write-Host ""

$totalCount  = 0
$failCount   = 0

foreach ($d in $dirs) {
    $totalCount++
    $found = @()
    $warns = @()

    foreach ($logFile in $FailLogs) {
        $p = Join-Path $d.FullName $logFile
        if (Test-Path -LiteralPath $p -PathType Leaf) {
            $lines = (Get-Content -LiteralPath $p | Measure-Object -Line).Lines
            $found += @{ Name = $logFile; Lines = $lines }
        }
    }

    foreach ($logFile in $WarnLogs) {
        $p = Join-Path $d.FullName $logFile
        if (Test-Path -LiteralPath $p -PathType Leaf) {
            $lines = (Get-Content -LiteralPath $p | Measure-Object -Line).Lines
            $warns += @{ Name = $logFile; Lines = $lines }
        }
    }

    if ($found.Count -gt 0) {
        $failCount++
        Write-Host ("[FAIL] {0}" -f $d.Name)
        foreach ($item in $found) {
            Write-Host ("       {0,-35} ({1} lines)" -f $item.Name, $item.Lines)
        }
        foreach ($item in $warns) {
            Write-Host ("       {0,-35} ({1} lines) [WARN]" -f $item.Name, $item.Lines)
        }
        Write-Host ""
    } elseif ($warns.Count -gt 0) {
        Write-Host ("[WARN] {0}" -f $d.Name)
        foreach ($item in $warns) {
            Write-Host ("       {0,-35} ({1} lines)" -f $item.Name, $item.Lines)
        }
        Write-Host ""
    } else {
        Write-Host ("[OK]   {0}" -f $d.Name)
    }
}

Write-Host ""
Write-Host "--- Summary ---"
Write-Host ("Total:   {0}" -f $totalCount)
Write-Host ("Failed:  {0}" -f $failCount)
Write-Host ("OK:      {0}" -f ($totalCount - $failCount))

exit $failCount
