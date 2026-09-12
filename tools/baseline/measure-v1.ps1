#!/usr/bin/env pwsh
<#
.SYNOPSIS
    Translator V2 — 阶段 0 基线测量（进程级）

.DESCRIPTION
    测量 V1（ets2-translator, Python 版）的进程级基线指标，输出 JSON + Markdown。
    对应 docs/migration-matrix.md 第 6 节表 5 的指标编号。

    本脚本只读取 V1 源码，不修改 V1 任何文件。
    但运行 V1 本体时，V1 会按自身逻辑写入 文档\ETS2 Translator\（配置与日志）——
    这是 V1 的正常行为，用 -SkipProcessRun 可跳过。

.PARAMETER V1Path
    V1 项目目录。默认为仓库根下的 ets2-translator。

.PARAMETER OutDir
    报告输出目录。

.PARAMETER IdleSampleSeconds
    空闲采样时长（秒），用于 B3 空闲 RSS 与 B9 闲置 CPU。

.PARAMETER SkipProcessRun
    跳过需要启动 V1 进程的指标（B2/B3/B9）。

.PARAMETER SetGameFps
    可选。用于 B14：手动填入游戏内关闭悬浮窗时的基准 FPS，脚本只做记录。

.NOTES
    本文件必须以 **UTF-8 with BOM** 保存。Windows PowerShell 5.1 会把无 BOM 的
    UTF-8 脚本按本地代码页读取，中文注释会破坏语法。重建 BOM 的方法见 README.md。
#>
[CmdletBinding()]
param(
    [string]$V1Path,
    [string]$OutDir,
    [int]$IdleSampleSeconds = 30,
    [switch]$SkipProcessRun,
    [double]$SetGameFps = 0
)

$ErrorActionPreference = 'Stop'
$script:Root = Resolve-Path (Join-Path $PSScriptRoot '..\..\..')

if (-not $V1Path) { $V1Path = Join-Path $script:Root 'ets2-translator' }
if (-not $OutDir) { $OutDir  = Join-Path $PSScriptRoot 'out' }

$V1Path = (Resolve-Path $V1Path).Path
New-Item -ItemType Directory -Force -Path $OutDir | Out-Null

$results = [ordered]@{}
function Set-Metric {
    param([string]$Id, [string]$Name, $Value, [string]$Unit = '', [string]$Note = '')
    $results[$Id] = [pscustomobject]@{
        Id = $Id; Name = $Name; Value = $Value; Unit = $Unit; Note = $Note
    }
    $shown = if ($null -eq $Value) { 'n/a' } else { $Value }
    Write-Host ("  {0,-4} {1,-32} {2} {3}" -f $Id, $Name, $shown, $Unit) -ForegroundColor Cyan
}

# 按命令行回退查找 V1 进程。
# 为什么需要：`python` 可能指向 WindowsApps 的转发壳，壳进程会先于真实解释器退出，
# 因此不能只依赖启动时拿到的 PID。这是上一版脚本 B3/B9 测出 n/a 的原因。
function Find-V1Proc {
    param([int]$Id)
    $q = Get-Process -Id $Id -ErrorAction SilentlyContinue
    if ($q) { return $q }
    $c = Get-CimInstance Win32_Process -Filter "Name='python.exe'" -ErrorAction SilentlyContinue |
         Where-Object { $_.CommandLine -and $_.CommandLine -like '*main.py*' } |
         Select-Object -First 1
    if ($c) { return (Get-Process -Id $c.ProcessId -ErrorAction SilentlyContinue) }
    return $null
}

Write-Host ''
Write-Host '=== Translator V2 · 阶段 0 基线测量（V1）===' -ForegroundColor Yellow
Write-Host "  V1 路径 : $V1Path"
Write-Host "  输出目录: $OutDir"
Write-Host ''

# ─────────────────────────────────────────────────────────────
# 预检
# ─────────────────────────────────────────────────────────────
Write-Host '[预检]' -ForegroundColor Green

if (-not (Test-Path (Join-Path $V1Path 'main.py'))) {
    throw "在 $V1Path 未找到 main.py —— 请用 -V1Path 指定 V1 项目目录"
}

$python = (Get-Command python -ErrorAction SilentlyContinue)
if (-not $python) { throw '未找到 python，无法测量。请先安装 Python 3.10+ 并加入 PATH。' }
$pyVersion = (& python --version 2>&1) -join ''

# `python` 可能是 WindowsApps 转发壳，解析真实解释器路径
$realPy = $null
try { $realPy = (& python -c "import sys;print(sys.executable)" 2>&1 | Select-Object -First 1).Trim() } catch { }
if (-not $realPy -or -not (Test-Path $realPy)) { $realPy = $python.Source }

Write-Host "  Python  : $pyVersion"
Write-Host "  解释器  : $realPy"
Write-Host "  V1 main : ok"

$leftover = Get-CimInstance Win32_Process -Filter "Name='python.exe'" -ErrorAction SilentlyContinue |
            Where-Object { $_.CommandLine -and $_.CommandLine -like '*main.py*' }
if ($leftover) {
    Write-Host "  [警告] 已有 $($leftover.Count) 个 V1 实例在运行。" -ForegroundColor Yellow
    Write-Host "         V1 有单实例互斥体，会导致新实例直接退出。" -ForegroundColor Yellow
    Write-Host "         请先关闭它，或使用 -SkipProcessRun。" -ForegroundColor Yellow
}

# ─────────────────────────────────────────────────────────────
# B1 产物大小
# ─────────────────────────────────────────────────────────────
Write-Host ''
Write-Host '[B1] 产物大小' -ForegroundColor Green
$distDir = Join-Path $V1Path 'dist'
if (Test-Path $distDir) {
    $exe = Get-ChildItem $distDir -Filter '*.exe' -File | Sort-Object LastWriteTime -Descending | Select-Object -First 1
    if ($exe) {
        Set-Metric 'B1' '产物大小（exe）' ([math]::Round($exe.Length / 1MB, 2)) 'MB' $exe.Name
    } else {
        Set-Metric 'B1' '产物大小（exe）' $null 'MB' 'dist 目录下无 exe'
    }
} else {
    Set-Metric 'B1' '产物大小（exe）' $null 'MB' '未找到 dist 目录'
}

# ─────────────────────────────────────────────────────────────
# B10 单元测试通过数
# ─────────────────────────────────────────────────────────────
Write-Host ''
Write-Host '[B10] 单元测试' -ForegroundColor Green
Push-Location $V1Path
try {
    # -p no:cacheprovider：避免写 .pytest_cache（该目录在部分环境存在 ACL 限制）
    $pytestOut = & python -m pytest -q -p no:cacheprovider 2>&1 | Out-String
    if ($pytestOut -match '(\d+)\s+passed') {
        $passed = [int]$Matches[1]
        $failed  = if ($pytestOut -match '(\d+)\s+failed') { [int]$Matches[1] } else { 0 }
        $errored = if ($pytestOut -match '(\d+)\s+error')  { [int]$Matches[1] } else { 0 }
        $note = "failed=$failed error=$errored"
        if ($failed -gt 0 -or $errored -gt 0) {
            $note += ' —— 有失败时请先排查是否为受限环境导致（见 README）'
        }
        Set-Metric 'B10' '单元测试通过数' $passed 'passed' $note
    } else {
        Set-Metric 'B10' '单元测试通过数' $null '' '未能解析 pytest 输出'
        Write-Host $pytestOut -ForegroundColor DarkGray
    }
} finally {
    Pop-Location
}

# ─────────────────────────────────────────────────────────────
# B2 / B3 / B9 冷启动、空闲内存、闲置 CPU
# ─────────────────────────────────────────────────────────────
Write-Host ''
Write-Host '[B2/B3/B9] 启动与空闲采样' -ForegroundColor Green

if ($SkipProcessRun) {
    Set-Metric 'B2' '冷启动 → 窗口可见' $null 's' '已跳过（-SkipProcessRun）'
    Set-Metric 'B3' '空闲 RSS（均值）'   $null 'MB' '已跳过（-SkipProcessRun）'
    Set-Metric 'B9' '闲置 CPU（均值）'   $null '%'  '已跳过（-SkipProcessRun）'
} else {
    $v1Out = Join-Path $OutDir 'v1-stdout.txt'
    $v1Err = Join-Path $OutDir 'v1-stderr.txt'
    Remove-Item $v1Out, $v1Err -Force -ErrorAction SilentlyContinue

    $sw = [System.Diagnostics.Stopwatch]::StartNew()
    $proc = Start-Process -FilePath $realPy -ArgumentList 'main.py' `
                          -WorkingDirectory $V1Path -PassThru `
                          -RedirectStandardOutput $v1Out `
                          -RedirectStandardError  $v1Err
    Write-Host "  已启动 PID=$($proc.Id)，等待主窗口出现（最多 60s）..."

    $coldStart = $null
    $deadline = (Get-Date).AddSeconds(60)
    while ((Get-Date) -lt $deadline) {
        Start-Sleep -Milliseconds 100
        $p = Find-V1Proc -Id $proc.Id
        if (-not $p) { break }
        if ($p.MainWindowHandle -ne 0) {
            $coldStart = [math]::Round($sw.Elapsed.TotalSeconds, 2)
            break
        }
    }
    $sw.Stop()

    if ($null -eq $coldStart) {
        $hint = '未检测到主窗口'
        if (-not (Find-V1Proc -Id $proc.Id)) {
            $hint = '进程提前退出（可能是单实例互斥体，或启动期报错）'
        }
        Set-Metric 'B2' '冷启动 → 窗口可见' $null 's' $hint
    } else {
        Set-Metric 'B2' '冷启动 → 窗口可见' $coldStart 's' '进程启动 → 主窗口句柄出现'
        Start-Sleep -Seconds 2   # 让窗口完成首帧渲染与初始化再开始采样
    }

    # 空闲采样
    $script:lastCpuMs = 0
    $rss = @(); $cpu = @()
    Write-Host "  空闲采样 $IdleSampleSeconds 秒..."
    for ($i = 0; $i -lt $IdleSampleSeconds; $i++) {
        Start-Sleep -Seconds 1
        $p = Find-V1Proc -Id $proc.Id
        if (-not $p) { break }
        $p.Refresh()
        $rss += $p.WorkingSet64 / 1MB
        if ($script:lastCpuMs -gt 0) {
            $cpu += [math]::Round(($p.TotalProcessorTime.TotalMilliseconds - $script:lastCpuMs) / 10, 2)
        }
        $script:lastCpuMs = $p.TotalProcessorTime.TotalMilliseconds
    }

    if ($rss.Count -gt 0) {
        $rssAvg = [math]::Round(($rss | Measure-Object -Average).Average, 1)
        $rssMax = [math]::Round(($rss | Measure-Object -Maximum).Maximum, 1)
        Set-Metric 'B3' '空闲 RSS（均值）' $rssAvg 'MB' "峰值 $rssMax MB，样本 $($rss.Count)"
    } else {
        Set-Metric 'B3' '空闲 RSS（均值）' $null 'MB' '进程未存活，无样本'
        if (Test-Path $v1Err) {
            $tail = (Get-Content $v1Err -Tail 8 -ErrorAction SilentlyContinue) -join ' / '
            if ($tail) { Write-Host "  V1 stderr: $tail" -ForegroundColor DarkYellow }
        }
    }

    if ($cpu.Count -gt 0) {
        $cpuAvg = [math]::Round(($cpu | Measure-Object -Average).Average, 2)
        Set-Metric 'B9' '闲置 CPU（均值）' $cpuAvg '%' "样本 $($cpu.Count)（单核百分比）"
    } else {
        Set-Metric 'B9' '闲置 CPU（均值）' $null '%' '无样本'
    }

    # 收尾
    $alive = Find-V1Proc -Id $proc.Id
    if ($alive) {
        Write-Host '  正在结束 V1 进程...'
        Stop-Process -Id $alive.Id -Force -ErrorAction SilentlyContinue
        Start-Sleep -Milliseconds 800
    }
}

# ─────────────────────────────────────────────────────────────
# B14 人工输入项
# ─────────────────────────────────────────────────────────────
Write-Host ''
Write-Host '[B14] 游戏帧率（需人工）' -ForegroundColor Green
if ($SetGameFps -gt 0) {
    Set-Metric 'B14' '游戏基准 FPS（关悬浮窗）' $SetGameFps 'fps' '人工提供'
} else {
    Set-Metric 'B14' '游戏基准 FPS（关悬浮窗）' $null 'fps' '需人工：游戏内采样后以 -SetGameFps 传入'
}

# ─────────────────────────────────────────────────────────────
# 报告
# ─────────────────────────────────────────────────────────────
# UTF-8 无 BOM（PowerShell 5.1 的 -Encoding UTF8 会写 BOM，
# 会让 Go 与 Python 的严格 JSON 解析失败）
function Write-Utf8NoBom {
    param([string]$Path, [string]$Content)
    $enc = New-Object System.Text.UTF8Encoding($false)
    [System.IO.File]::WriteAllText($Path, $Content, $enc)
}

$stamp    = Get-Date -Format 'yyyyMMdd-HHmmss'
$jsonPath = Join-Path $OutDir "baseline-v1-$stamp.json"
$mdPath   = Join-Path $OutDir "baseline-v1-$stamp.md"
$latest   = Join-Path $OutDir 'baseline-v1-latest.json'

$payload = [pscustomobject]@{
    tool        = 'Translator V2 baseline'
    measuredAt  = (Get-Date).ToString('s')
    v1Path      = $V1Path
    python      = $pyVersion
    interpreter = $realPy
    machine     = $env:COMPUTERNAME
    os          = (Get-CimInstance Win32_OperatingSystem -ErrorAction SilentlyContinue).Caption
    metrics     = $results.Values
}

$json = $payload | ConvertTo-Json -Depth 5
Write-Utf8NoBom -Path $jsonPath -Content $json
Write-Utf8NoBom -Path $latest   -Content $json

$md = @()
$md += '# Translator V2 · 阶段 0 基线（V1 进程级）'
$md += ''
$md += '| 项目 | 值 |'
$md += '|---|---|'
$md += "| 测量时间 | $($payload.measuredAt) |"
$md += "| V1 路径 | ``$V1Path`` |"
$md += "| Python | $pyVersion |"
$md += "| 解释器 | ``$realPy`` |"
$md += "| 主机 | $($payload.machine) |"
$md += ''
$md += '| # | 指标 | 实测 | 单位 | 备注 |'
$md += '|---|---|---|---|---|'
foreach ($m in $results.Values) {
    $v = if ($null -eq $m.Value) { '—' } else { $m.Value }
    $md += "| $($m.Id) | $($m.Name) | $v | $($m.Unit) | $($m.Note) |"
}
$md += ''
$md += '> 未覆盖的指标（需真实 API Key 或人工参与）：B4 负载 RSS、B5 首条翻译延迟、B6 竞速延迟、B8 缓存命中率、B11 Win10 毛玻璃、B12 设置窗口耗时、B13 热键延迟。'
$md += '> 其中 B5/B7/B8 的离线可测部分由 `pipeline_probe.py` 覆盖。'
Write-Utf8NoBom -Path $mdPath -Content ($md -join "`n")

Write-Host ''
Write-Host '=== 完成 ===' -ForegroundColor Yellow
Write-Host "  JSON : $jsonPath"
Write-Host "  MD   : $mdPath"
Write-Host "  最新 : $latest"
Write-Host ''
Write-Host '提示：B4/B5/B6/B8 需要有效 Provider 配置与真实聊天流量；' -ForegroundColor DarkGray
Write-Host '      B7 与词典路径延迟请运行 pipeline_probe.py。' -ForegroundColor DarkGray
