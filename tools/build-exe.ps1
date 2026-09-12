# Translator V2 —— 打成单个 EXE
#
# 把前端产物与 native 能力层嵌进 Go 可执行文件，产出 dist\ets2-translator.exe：
# 用户拿到一个文件双击就能跑，不需要旁边放着 dist 目录和 translator_native.exe。
# V1 用 build_exe.py（PyInstaller）达到同样效果。
#
#   .\build-exe.ps1                # 构建前端 + native，再打包
#   .\build-exe.ps1 -SkipBuild     # 用现有的 frontend\dist 与 dist\translator_native.exe 直接打包
#
# -SkipBuild 不是给用户用的，是给「只改了 Go 代码、不想等 npm/cmake」的场合用的。
[CmdletBinding()]
param(
    [switch]$SkipBuild,
    # 打包完把嵌进源码树的产物清掉（默认开）。用 -KeepEmbedded 保留，
    # 便于观察到底嵌进去了什么。
    [switch]$KeepEmbedded,
    [string]$Output = ''
)

$ErrorActionPreference = 'Stop'

$V2      = Split-Path -Parent $PSScriptRoot          # Translator-V2\
$Backend = Join-Path $V2 'backend'
$Frontend= Join-Path $V2 'frontend'
$Dist    = Join-Path $V2 'dist'
$EmbFe   = Join-Path $Backend 'internal\embedded\frontend'
$EmbNat  = Join-Path $Backend 'internal\embedded\native'
$EmbRes  = Join-Path $Backend 'internal\embedded\resources'
$Res     = Join-Path $V2 'resources'
# 需要一起嵌进 EXE 的三个目录；占位 README.txt 由脚本保留
$EmbDirs = @($EmbFe, $EmbNat, $EmbRes)

if (-not $Output) { $Output = Join-Path $Dist 'ets2-translator.exe' }

function Step($msg) { Write-Host "`n=== $msg ===" -ForegroundColor Cyan }
function Die($msg)  { Write-Host $msg -ForegroundColor Red; exit 1 }

# go 常常不在 PATH 上（这台开发机就是如此，只在 C:\Go\bin 下），所以自己找一遍。
# 找不到就明确报错，而不是让 go build 报一句「无法将"go"项识别为 cmdlet」——
# 那看不出到底是缺工具链还是脚本写错了。
function Resolve-Go {
    $cmd = Get-Command go -ErrorAction SilentlyContinue
    if ($cmd) { return $cmd.Source }
    foreach ($c in @(
        'C:\Go\bin\go.exe',
        (Join-Path $env:ProgramFiles 'Go\bin\go.exe'),
        (Join-Path $env:LOCALAPPDATA 'Programs\Go\bin\go.exe')
    )) {
        if ($c -and (Test-Path $c)) { return $c }
    }
    return $null
}

$Go = Resolve-Go
if (-not $Go) { Die '找不到 go 可执行文件（不在 PATH 上，常见位置也没有）' }
Write-Host "go: $Go"

# ── 1. 前端 ────────────────────────────────────────────────
if (-not $SkipBuild) {
    Step '构建前端'
    Push-Location $Frontend
    try {
        # npm 缓存指到工作区里，避免沙箱/权限导致的诡异失败
        $env:npm_config_cache = Join-Path (Split-Path -Parent $V2) '.npm-cache'
        npm run build
        if ($LASTEXITCODE -ne 0) { Die '前端构建失败' }
    } finally { Pop-Location }
}

$feIndex = Join-Path $Frontend 'dist\index.html'
if (-not (Test-Path $feIndex)) { Die "找不到前端产物：$feIndex（先跑 npm run build，或去掉 -SkipBuild）" }

# ── 2. native ─────────────────────────────────────────────
if (-not $SkipBuild) {
    Step '构建 native'
    $buildDir = Join-Path $V2 'build'
    if (-not (Test-Path (Join-Path $buildDir 'CMakeCache.txt'))) {
        cmake -S (Join-Path $V2 'native') -B $buildDir -G 'Visual Studio 17 2022' -A x64
        if ($LASTEXITCODE -ne 0) { Die 'CMake 配置失败' }
    }
    # native exe 在运行时会被锁住，先收掉
    Get-Process translator_native -ErrorAction SilentlyContinue | Stop-Process -Force -ErrorAction SilentlyContinue
    Start-Sleep -Milliseconds 500
    cmake --build $buildDir --config Release
    if ($LASTEXITCODE -ne 0) { Die 'native 构建失败' }

    $built = Get-ChildItem -Path $buildDir -Recurse -Filter 'translator_native.exe' -ErrorAction SilentlyContinue |
             Select-Object -First 1
    if (-not $built) { Die "构建目录里找不到 translator_native.exe（$buildDir）" }
    New-Item -ItemType Directory -Force -Path $Dist | Out-Null
    Copy-Item $built.FullName (Join-Path $Dist 'translator_native.exe') -Force
}

$natExe = Join-Path $Dist 'translator_native.exe'
if (-not (Test-Path $natExe)) { Die "找不到 native 产物：$natExe" }

# ── 3. 把产物拷进 embed 目录 ───────────────────────────────
#
# 只清掉旧产物、**保留占位 README.txt**：
# `//go:embed all:<dir>` 在目录为空时会直接让 go build 失败，
# 而这个脚本结束时会清场（除非 -KeepEmbedded），所以占位文件必须活下来。
Step '拷入 embed 目录'
foreach ($d in $EmbDirs) {
    if (-not (Test-Path $d)) { New-Item -ItemType Directory -Force -Path $d | Out-Null }
    Get-ChildItem -Path $d -Recurse -File | Where-Object { $_.Name -ne 'README.txt' } | Remove-Item -Force
}
Copy-Item (Join-Path $Frontend 'dist\*') $EmbFe -Recurse -Force
Copy-Item $natExe $EmbNat -Force
# resources 必须一起嵌：程序起来第一件事就是读 resources/dictionary.json，
# 读不到会直接以「加载词库失败」退出。只嵌前端和 native 的「单 EXE」
# 拷到一个空目录里根本起不来——而它看起来是构建成功的。
Copy-Item (Join-Path $Res '*') $EmbRes -Recurse -Force

$feFiles = (Get-ChildItem -Path $EmbFe -Recurse -File).Count
$natKB   = [int]((Get-Item (Join-Path $EmbNat 'translator_native.exe')).Length / 1KB)
$resKB   = [int](((Get-ChildItem -Path $EmbRes -File | Measure-Object Length -Sum).Sum) / 1KB)
Write-Host "  前端 $feFiles 个文件，native $natKB KB，资源 $resKB KB"

# ── 4. 构建 Go ────────────────────────────────────────────
Step '构建单 EXE'
$env:GOCACHE  = Join-Path (Split-Path -Parent $V2) '.gocache'
$env:GOTMPDIR = Join-Path (Split-Path -Parent $V2) '.gotmp'
Push-Location $Backend
try {
    # -s -w 去掉符号表与调试信息：单 EXE 主要是给用户双击的，
    # 少几 MB 比保留调试能力更重要（需要调试时用普通的 go build）。
    #
    # -H=windowsgui：**双击时不弹控制台黑框**。
    # 控制台窗口在普通用户眼里就是「这软件是命令行的」，而双击是他们唯一的
    # 启动方式。代价是 stdout/stderr 被系统丢弃 —— 所以启动失败必须靠
    # main.go 里的 fatal() 弹系统对话框说出来，加上照常写 <数据目录>\logs\；
    # 两者缺一，「启动失败」就变成「双击了没反应」。
    & $Go build -trimpath -ldflags '-s -w -H=windowsgui' -o $Output ./cmd/translator
    if ($LASTEXITCODE -ne 0) { Die 'go build 失败' }
} finally { Pop-Location }

# ── 5. 清场 ───────────────────────────────────────────────
if (-not $KeepEmbedded) {
    Step '清理 embed 目录（保留占位文件）'
    foreach ($d in $EmbDirs) {
        Get-ChildItem -Path $d -Recurse -File | Where-Object { $_.Name -ne 'README.txt' } | Remove-Item -Force
    }
}

$sizeMB = [math]::Round((Get-Item $Output).Length / 1MB, 1)
Step '完成'
Write-Host "  $Output  ($sizeMB MB)"
Write-Host "  运行：`"$Output`" -serve"
