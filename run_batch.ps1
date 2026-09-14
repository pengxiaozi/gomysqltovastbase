# 顺序迁移 configs/ 下所有 yml，每对独立日志，单对失败不中断后续。
# 用法: powershell -ExecutionPolicy Bypass -File .\run_batch.ps1 [-ConfigDir configs] [-Mode migrate|dryrun]
#   -Mode dryrun: 仅做只读预检，不创建对象不迁移数据

[CmdletBinding()]
param(
    [string]$ConfigDir = "configs",
    [ValidateSet("migrate","dryrun")]
    [string]$Mode = "migrate"
)

$ErrorActionPreference = "Continue"

$Binary    = ".\gomysql2pg.exe"
$Ts        = Get-Date -Format "yyyyMMdd-HHmmss"
if ($Mode -eq "dryrun") {
    $BatchLog = "batch-dryrun-$Ts.log"
} else {
    $BatchLog = "batch-$Ts.log"
}

# 从 yml 中读取 section.key（section=src|dest，只支持二层缩进、无嵌套 map）
function Get-YmlValue {
    param(
        [Parameter(Mandatory)][string]$File,
        [Parameter(Mandatory)][string]$Section,
        [Parameter(Mandatory)][string]$Key
    )
    $inSec = $false
    foreach ($line in Get-Content -LiteralPath $File) {
        if ($line -match '^([A-Za-z_][A-Za-z0-9_]*):\s*$') {
            $inSec = ($Matches[1] -eq $Section)
            continue
        }
        if ($line -match '^[^\s]') {
            $inSec = $false
            continue
        }
        if ($inSec -and ($line -match '^\s+([A-Za-z_][A-Za-z0-9_]*):\s*(.*)$')) {
            if ($Matches[1] -eq $Key) {
                $val = $Matches[2].Trim()
                $val = $val -replace '^"(.*)"$', '$1'
                $val = $val -replace "^'(.*)'$", '$1'
                return $val
            }
        }
    }
    return ""
}

if (-not (Test-Path -LiteralPath $Binary -PathType Leaf)) {
    [Console]::Error.WriteLine("binary not found or not executable: $Binary")
    exit 2
}
if (-not (Test-Path -LiteralPath $ConfigDir -PathType Container)) {
    [Console]::Error.WriteLine("config dir not found: $ConfigDir")
    exit 2
}

$files = Get-ChildItem -LiteralPath $ConfigDir -File |
    Where-Object { $_.Extension -in '.yml', '.yaml' } |
    Sort-Object Name

if (-not $files -or $files.Count -eq 0) {
    [Console]::Error.WriteLine("no yml files under $ConfigDir")
    exit 2
}

# 预览待迁移的配置文件
Write-Host ("Found {0} config(s) to migrate:" -f $files.Count)
$i = 1
foreach ($f in $files) {
    $sHost = Get-YmlValue -File $f.FullName -Section src  -Key host
    $sPort = Get-YmlValue -File $f.FullName -Section src  -Key port
    $sDb   = Get-YmlValue -File $f.FullName -Section src  -Key database
    $dHost = Get-YmlValue -File $f.FullName -Section dest -Key host
    $dPort = Get-YmlValue -File $f.FullName -Section dest -Key port
    $dDb   = Get-YmlValue -File $f.FullName -Section dest -Key database
    $dUser = Get-YmlValue -File $f.FullName -Section dest -Key username

    if ([string]::IsNullOrEmpty($sHost)) { $sHost = "?" }
    if ([string]::IsNullOrEmpty($sPort)) { $sPort = "?" }
    if ([string]::IsNullOrEmpty($sDb))   { $sDb   = "?" }
    if ([string]::IsNullOrEmpty($dHost)) { $dHost = "?" }
    if ([string]::IsNullOrEmpty($dPort)) { $dPort = "?" }
    if ([string]::IsNullOrEmpty($dDb))   { $dDb   = "?" }
    if ([string]::IsNullOrEmpty($dUser)) { $dUser = "?" }

    Write-Host ("  [{0}] {1}  |  src={2}:{3}/{4}  ->  dest={5}@{6}:{7}/{8}" -f `
        $i, $f.FullName, $sHost, $sPort, $sDb, $dUser, $dHost, $dPort, $dDb)
    $i++
}
Write-Host ""

# 交互式确认（dryrun 模式只读，无需确认）
if ($Mode -ne "dryrun") {
    $answer = (Read-Host -Prompt ("Proceed with these {0} migration(s)? [yes/no]" -f $files.Count)).ToLower()
    if ($answer -notin @('yes','y')) {
        Write-Host "aborted by user"
        exit 1
    }
}

$succ = @()
$fail = @()
$missingSchemas = @()

Start-Transcript -LiteralPath $BatchLog -Append | Out-Null
try {
    Write-Host ("=== batch start {0} mode={1} total={2} ===" -f (Get-Date -Format "yyyy-MM-dd HH:mm:ss"), $Mode, $files.Count)
    foreach ($f in $files) {
        Write-Host ""
        Write-Host "============================================================"
        Write-Host (">>> [{0}] running: {1}" -f (Get-Date -Format "HH:mm:ss"), $f.FullName)
        Write-Host "============================================================"

        if ($Mode -eq "dryrun") {
            $runOut = & $Binary --config $f.FullName dryRun 2>&1
        } else {
            $runOut = & $Binary --config $f.FullName 2>&1
        }
        $rc = $LASTEXITCODE
        $runOut | ForEach-Object { Write-Host $_ }

        if ($rc -eq 0) {
            Write-Host ("<<< [OK] {0}" -f $f.FullName)
            $succ += $f.FullName
        } else {
            Write-Host ("<<< [FAIL rc={0}] {1}" -f $rc, $f.FullName)
            $fail += $f.FullName
            if ($runOut | Select-String -SimpleMatch 'no schema named') {
                $dUser = Get-YmlValue -File $f.FullName -Section dest -Key username
                if (-not [string]::IsNullOrEmpty($dUser)) {
                    $missingSchemas += $dUser
                }
            }
        }
    }

    Write-Host ""
    Write-Host ("=== batch done {0} ===" -f (Get-Date -Format "yyyy-MM-dd HH:mm:ss"))
    Write-Host ("success: {0}" -f $succ.Count)
    foreach ($x in $succ) { Write-Host ("  OK   {0}" -f $x) }
    Write-Host ("failed:  {0}" -f $fail.Count)
    foreach ($x in $fail) { Write-Host ("  FAIL {0}" -f $x) }

    $uniqMissing = $missingSchemas | Select-Object -Unique
    if ($uniqMissing.Count -gt 0) {
        Write-Host ""
        Write-Host "=== missing same-name schema(s) detected ==="
        Write-Host "Run the following SQL on the destination database as a privileged user:"
        Write-Host ""
        foreach ($u in $uniqMissing) {
            Write-Host ("create user {0} with password '123456';" -f $u)
            Write-Host ("create schema {0} authorization {0};" -f $u)
        }
        Write-Host ""
        Write-Host "(Replace '123456' with a real password before executing.)"
    }
} finally {
    Stop-Transcript | Out-Null
}

exit $fail.Count
