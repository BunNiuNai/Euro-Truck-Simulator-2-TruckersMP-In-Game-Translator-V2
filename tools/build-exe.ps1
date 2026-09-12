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
    [string]$Output = '',
    # 版本号，注入到 api.Version（见下面 -X 那段）。
    # 留空 = 用源码里的默认值，那是**开发用**的 v2.0.0-dev。
    [string]$Version = ''
)

# -X 要精确到「包路径.变量名」。模块名见 backend/go.mod（末尾是 /backend），
# 路径写错时 go **不报错**，只是版本号悄悄不变——所以 build 完会回读自检。
$ApiPkgVar = 'github.com/BunNiuNai/ets2-translator-v2/backend/internal/api.Version'

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

    # ⚠️ 产物**直接在 dist/**，不在 build/ 里：native/CMakeLists.txt:182-184 把
    # RUNTIME_OUTPUT_DIRECTORY 指到了 ${CMAKE_SOURCE_DIR}/../dist。
    #
    # 这里原先是在 $buildDir 下递归找 translator_native.exe，找不到就中止——
    # 也就是说**「不带 -SkipBuild 的完整打包」从来没成功过**。而带 -SkipBuild
    # 时整段被跳过、直接用 dist 里已有的产物，反而没事，所以这个 bug 藏了很久。
    $natBuilt = Join-Path $Dist 'translator_native.exe'
    if (-not (Test-Path $natBuilt)) {
        Die "构建后找不到 native 产物：$natBuilt`n（CMake 的输出目录见 native/CMakeLists.txt 的 RUNTIME_OUTPUT_DIRECTORY）"
    }
    Write-Host "  native 产物: $natBuilt ($([int]((Get-Item $natBuilt).Length / 1KB)) KB)"
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
    #
    # -X 注入版本号：api.Version 在源码里的默认值是开发用的 v2.0.0-dev。
    # ⚠️ 包路径写错 go **不会报错**，只是版本号悄悄不变——所以下面 build 完
    # 会用 `go version -m` 回读一次，确认 -X 真的进去了。
    $ld = '-s -w -H=windowsgui'
    if ($Version) {
        if ($Version -match '\s') { Die "版本号不能含空格：'$Version'（-X 的值遇空格会被拆成两个参数）" }
        $ld = "$ld -X $ApiPkgVar=$Version"
    }
    & $Go build -trimpath -ldflags $ld -o $Output ./cmd/translator
    if ($LASTEXITCODE -ne 0) { Die 'go build 失败' }

    # 回读自检：-X 写错包路径时 go **静默忽略**，只能靠读产物发现。
    #
    # ⚠️ 判据不能用 `go version -m`：它**不记录 -ldflags**（实测确认），
    # 拿它去找 "-X …=…" 会恒为假——初版就是这么写的，每次打包都误报失败。
    # 改成在产物里找版本串的 UTF-8 字节：-X 注入的是字符串字面量，必然出现。
    if ($Version) {
        $text = [System.IO.File]::ReadAllText($Output, [System.Text.Encoding]::UTF8)
        if (-not $text.Contains($Version)) {
            Die "版本号注入失败：产物里找不到 '$Version'。`n请核对包路径是否与 backend/go.mod 的模块名一致（当前 $ApiPkgVar）。"
        }
        Write-Host "  版本号已注入：$Version" -ForegroundColor Green
    } else {
        Write-Host '  未指定 -Version，产物报的是源码默认值（开发用）' -ForegroundColor Yellow
    }
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
