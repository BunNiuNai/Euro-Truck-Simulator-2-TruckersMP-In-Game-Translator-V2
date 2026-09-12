# Translator V2 —— 导出一棵"只含代码"的树，用于发布到公开仓库
#
# 为什么是脚本而不是手工挑文件：这个动作每次发布都要做一遍，手挑必然
# 漏掉或多带。判据固定为「git 跟踪的文件 − 下面那组排除模式」：
#   · 新增的**代码**文件会自动进入导出树（这是我们要的）
#   · 新增的**文档**目录必须显式加进 $Exclude（这是有意的）
# 宁可文档意外出现（一眼能看见），也不要代码意外消失（编译不过，
# 但报错原因不指向导出脚本）。
#
# 用法：
#   powershell -File tools\export-public.ps1 -OutDir ..\..\v2-public
#
[CmdletBinding()]
param(
    # 导出目标目录。会被**清空重建**——别指向任何你还要用的目录。
    [Parameter(Mandatory = $true)][string]$OutDir
)

$ErrorActionPreference = 'Stop'
$V2 = Split-Path -Parent $PSScriptRoot

# 排除模式：匹配相对路径（PowerShell 正则，不区分大小写）
$Exclude = @(
    '^docs/',                     # 全部文档
    'README\.md$',                # 任何位置的 README.md
    '^tools/baseline/',           # V1 基线测量工具与结果
    '^tools/fidelity/out/',       # 生成的测量数据
    '^native/tools/probe-logs/'   # 探测日志（15 个 0 字节文件）
)

# 必须存在的文件：缺任何一个就中止。
# 前三个 README.txt 是 //go:embed 的**占位文件**——目录为空时
# `//go:embed all:<目录>` 会让 go build 直接失败（pattern matches no files）。
# 它们是构建要件，不是文档，所以**不在**排除表里。
$MustHave = @(
    'backend/go.mod',
    'backend/internal/embedded/frontend/README.txt',
    'backend/internal/embedded/native/README.txt',
    'backend/internal/embedded/resources/README.txt',
    'resources/dictionary.json',
    'frontend/package.json',
    'frontend/index.html',
    'native/CMakeLists.txt'
)

# ── 1. 取跟踪文件清单 ──────────────────────────────────────
# core.quotepath=false：否则含中文的路径会被 git 转义成 \346\224\266 这种形式
$all = git -c core.quotepath=false -C $V2 ls-files
if ($LASTEXITCODE -ne 0) { throw 'git ls-files 失败' }

$keep = @()
$dropped = @()
foreach ($f in $all) {
    $p = $f -replace '\\', '/'
    $hit = $false
    foreach ($pat in $Exclude) {
        if ($p -match $pat) { $hit = $true; break }
    }
    if ($hit) { $dropped += $p } else { $keep += $p }
}

# ── 2. 自检：必需文件一个都不能少 ──────────────────────────
$missing = @()
foreach ($m in $MustHave) {
    if ($keep -notcontains $m) { $missing += $m }
}
if ($missing.Count -gt 0) {
    Write-Host '以下必需文件不在导出结果里：' -ForegroundColor Red
    $missing | ForEach-Object { Write-Host "  $_" -ForegroundColor Red }
    Write-Host '可能是被 $Exclude 误伤，或文件已从仓库删除。中止。' -ForegroundColor Red
    exit 1
}

# ── 3. 清空并重建目标目录 ──────────────────────────────────
if (Test-Path $OutDir) {
    Write-Host "清空已存在的目标目录：$OutDir" -ForegroundColor Yellow
    Get-ChildItem -Path $OutDir -Force | Remove-Item -Recurse -Force
} else {
    New-Item -ItemType Directory -Force -Path $OutDir | Out-Null
}

# ── 4. 拷贝 ────────────────────────────────────────────────
foreach ($rel in $keep) {
    $src = Join-Path $V2 ($rel -replace '/', '\')
    $dst = Join-Path $OutDir ($rel -replace '/', '\')
    $dstDir = Split-Path -Parent $dst
    if (-not (Test-Path $dstDir)) { New-Item -ItemType Directory -Force -Path $dstDir | Out-Null }
    Copy-Item -LiteralPath $src -Destination $dst -Force
}

# ── 5. 汇总 ────────────────────────────────────────────────
Write-Host ''
Write-Host "导出完成：$($keep.Count) 个文件 -> $OutDir" -ForegroundColor Green
Write-Host "排除：$($dropped.Count) 个文件" -ForegroundColor DarkGray
foreach ($pat in $Exclude) {
    $n = ($dropped | Where-Object { $_ -match $pat }).Count
    Write-Host ("  {0,-30} {1}" -f $pat, $n) -ForegroundColor DarkGray
}
