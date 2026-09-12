# 构建前端并在浏览器里打开 UI（用于验证悬浮窗与设置页）
#
# 为什么需要这个脚本：Vite 构建依赖 esbuild，而 esbuild 要通过**匿名管道**
# 与子进程通信。开发沙箱在受限模式下禁止创建匿名管道，因此构建必须在
# 普通终端里执行（这也是本脚本存在的唯一原因）。
#
# 用法：
#   powershell -ExecutionPolicy Bypass -File tools\run-ui.ps1
#   powershell -ExecutionPolicy Bypass -File tools\run-ui.ps1 -SkipBuild   # 前端已构建过
#   powershell -ExecutionPolicy Bypass -File tools\run-ui.ps1 -Port 8899
#
# 打开：
#   http://127.0.0.1:8791/?page=overlay     ← 悬浮窗（默认）
#   http://127.0.0.1:8791/?page=settings    ← 设置页
#
# 按 Ctrl+C 退出。

param(
    [switch]$SkipBuild,
    [int]$Port = 8791
)

$ErrorActionPreference = 'Stop'

$root = Split-Path -Parent $PSScriptRoot
$fe = Join-Path $root 'frontend'
$be = Join-Path $root 'backend'
$dist = Join-Path $fe 'dist'

if (-not (Test-Path $fe)) { throw "找不到前端目录: $fe" }

# ── 1. 构建前端 ──────────────────────────────────────────────
if (-not $SkipBuild) {
    Write-Host '==> 构建前端' -ForegroundColor Cyan
    Push-Location $fe
    try {
        if (-not (Test-Path 'node_modules')) {
            Write-Host '    安装依赖…'
            npm install --ignore-scripts
        }
        npm run build
        if ($LASTEXITCODE -ne 0) { throw "前端构建失败（退出码 $LASTEXITCODE）" }
    } finally {
        Pop-Location
    }
}

if (-not (Test-Path (Join-Path $dist 'index.html'))) {
    throw "前端产物不存在: $dist\index.html（去掉 -SkipBuild 重新构建）"
}

$indexSize = (Get-Item (Join-Path $dist 'index.html')).Length
Write-Host "    产物就绪: $dist (index.html $indexSize 字节)" -ForegroundColor Green

# ── 2. 启动 Go（同时托管前端静态文件）───────────────────────
Write-Host '==> 启动后端' -ForegroundColor Cyan
Write-Host "    悬浮窗: http://127.0.0.1:$Port/?page=overlay"
Write-Host "    设置页: http://127.0.0.1:$Port/?page=settings"
Write-Host '    按 Ctrl+C 退出'
Write-Host ''

# 沙箱外的普通终端不需要这个；保留是为了让脚本在任何环境下都能找到可写的缓存目录
if (-not $env:GOCACHE) {
    $env:GOCACHE = Join-Path (Split-Path -Parent $root) '.gocache'
}
New-Item -ItemType Directory -Force -Path $env:GOCACHE | Out-Null

Push-Location $be
try {
    go run ./cmd/translator -serve -frontend $dist -addr "127.0.0.1:$Port"
} finally {
    Pop-Location
}
