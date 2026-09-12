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

# 排除模式：匹配相对路径（.NET 正则，不区分大小写）
#
# ⚠️ 教训（2026-09-12 第一次发布时踩的）：**不要用"整目录排除"**。
# 初版写了 `^tools/baseline/`，本意是排掉那个目录下的两份 .md，
# 结果连带把 `measure-v1.ps1`、`pipeline_probe.py` **两个脚本**一起扫掉了
# ——"除文档外都上传"变成了"除文档外还少了两个代码文件"，而且从排除表的
# 字面看不出这件事。现在按**文件类型**排除，不用目录。
$Exclude = @(
    '^docs/',                        # docs/ 下的内部记录（$Allow 里那三份除外）
    '/README\.md$',                  # 子目录的 README.md（**根目录那份是项目门面，要发**）
    '^tools/baseline/RESULTS-v1\.md$' # V1 基线测量报告
)

# 例外：即使命中 $Exclude 也保留。
#
# 目前**故意为空**。曾经想用它放行三份设计文档（architecture / language-assignment
# / migration-matrix）以避免 README 里的死链，但用户明确要求：**仓库里不要任何
# 纯文字说明性文件**，README 的链接改为不上传那三份（链接已从 README 里删掉）。
# 机制保留，是给将来真需要"排除表误伤某个代码文件"时用的——那种情况留个口子
# 比整条规则重写划算。加进来之前先确认它**不是文档**。
$Allow = @()

# 必须存在的文件：缺任何一个就中止。
# 前三个 README.txt 是 //go:embed 的**占位文件**——目录为空时
# `//go:embed all:<目录>` 会让 go build 直接失败（pattern matches no files）。
# 它们是构建要件，不是文档，所以**不在**排除表里。
$MustHave = @(
    'README.md',                     # 项目门面，README 里的链接指着下面那三份
    'backend/go.mod',
    'backend/internal/embedded/frontend/README.txt',
    'backend/internal/embedded/native/README.txt',
    'backend/internal/embedded/resources/README.txt',
    'resources/dictionary.json',
    'frontend/package.json',
    'frontend/index.html',
    'native/CMakeLists.txt',
    'tools/baseline/measure-v1.ps1',   # 曾被整目录排除误伤，钉住
    'tools/baseline/pipeline_probe.py' # 同上
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
    # 例外表优先于排除表
    if ($hit -and ($Allow -notcontains $p)) { $dropped += $p } else { $keep += $p }
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
    Write-Host ("  {0,-34} {1}" -f $pat, $n) -ForegroundColor DarkGray
}
# 例外是被排除规则命中却特意保留的——单独列出来，否则"排除 N 个"这个数
# 与"跟踪文件 − 导出文件"对不上，下次核账会以为漏了东西
$ovr = ($all | ForEach-Object { $_ -replace '\\', '/' } | Where-Object { $Allow -contains $_ }).Count
Write-Host "例外（命中排除表但保留）：$ovr 个" -ForegroundColor DarkGray
$Allow | ForEach-Object { Write-Host "  $_" -ForegroundColor DarkGray }
