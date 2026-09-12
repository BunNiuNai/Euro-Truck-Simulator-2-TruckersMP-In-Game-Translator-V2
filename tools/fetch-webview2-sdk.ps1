# 取回 WebView2 SDK（可选，非构建必需）
#
# 为什么是「可选」：见 docs/architecture.md ADR-013「SDK 不得成为构建硬依赖」。
# native/webview 优先走自写的 COM 声明，SDK 只作为可选加速路径与对照实现。
# 本开发环境网络被阻断（TLS 直接失败），故这份脚本留给有网络的机器执行。
#
# 用法（在**普通** PowerShell 里跑，沙箱内网络不可用）：
#   powershell -ExecutionPolicy Bypass -File tools\fetch-webview2-sdk.ps1
#   powershell -ExecutionPolicy Bypass -File tools\fetch-webview2-sdk.ps1 -Version 1.0.3124.44
#
# 产物：
#   .sdk/webview2/          解压后的 NuGet 包
#   .sdk/webview2/include/  WebView2.h 等头文件
#   .sdk/webview2/lib/      WebView2Loader.lib（静态与动态两种）
#   .sdk/webview2/loader/   WebView2Loader.dll（运行期需要）
#
# 注意：本脚本用 PowerShell 5.1 也能跑；不写入任何项目源码。

param(
    [string]$Version = '',                 # 留空则取最新稳定版
    [string]$OutDir  = '',                 # 留空则用 <仓库根>\.sdk
    [switch]$Force                         # 已存在时也重新下载
)

$ErrorActionPreference = 'Stop'
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

$repoRoot = Split-Path -Parent $PSScriptRoot
if (-not $OutDir) { $OutDir = Join-Path $repoRoot '.sdk' }
$target = Join-Path $OutDir 'webview2'

if ((Test-Path (Join-Path $target 'include')) -and -not $Force) {
    Write-Host "已存在，跳过（加 -Force 可重新下载）: $target"
    exit 0
}

Write-Host "==> 查询 NuGet 版本列表"
$indexUrl = 'https://api.nuget.org/v3-flatcontainer/microsoft.web.webview2/index.json'
$index = Invoke-RestMethod -Uri $indexUrl -TimeoutSec 30
$versions = @($index.versions)

if (-not $Version) {
    $Version = ($versions | Where-Object { $_ -notmatch '-' } | Select-Object -Last 1)
    if (-not $Version) { throw '拿不到稳定版本号' }
}
Write-Host "    使用版本: $Version"

$pkgUrl = "https://api.nuget.org/v3-flatcontainer/microsoft.web.webview2/$Version/microsoft.web.webview2.$Version.nupkg"
$zip = Join-Path $OutDir "webview2-$Version.zip"

New-Item -ItemType Directory -Force -Path $OutDir | Out-Null
Write-Host "==> 下载 $pkgUrl"
Invoke-WebRequest -Uri $pkgUrl -OutFile $zip -TimeoutSec 300 -UseBasicParsing
Write-Host "    完成: $([math]::Round((Get-Item $zip).Length / 1MB, 2)) MB"

Write-Host "==> 解压"
if (Test-Path $target) { Remove-Item $target -Recurse -Force }
Expand-Archive -Path $zip -DestinationPath $target -Force

# NuGet 包内布局随版本变过，这里按文件名找，不假设固定路径
Write-Host "==> 定位关键文件"
$wanted = @('WebView2.h', 'WebView2Loader.dll', 'WebView2LoaderStatic.lib', 'WebView2Loader.lib')
$found = @{}
Get-ChildItem -Path $target -Recurse -File -ErrorAction SilentlyContinue | ForEach-Object {
    if ($wanted -contains $_.Name -and -not $found.ContainsKey($_.Name)) {
        $found[$_.Name] = $_.FullName
    }
}

foreach ($name in $wanted) {
    if ($found.ContainsKey($name)) {
        Write-Host ("    ✓ {0,-28} {1}" -f $name, $found[$name].Replace($target, ''))
    } else {
        Write-Host ("    ✗ {0,-28} 未找到" -f $name)
    }
}

if (-not $found.ContainsKey('WebView2.h')) {
    throw '没找到 WebView2.h，包结构可能已变化，请人工检查后修改本脚本'
}

Write-Host ''
Write-Host '==> 下一步'
Write-Host '  1) 仅作对照：直接读 .sdk\webview2 下的头文件，不必改任何构建配置'
Write-Host '  2) 运行期：把 WebView2Loader.dll 放到 dist\ 旁边即可（动态加载，缺失也不影响启动）'
Write-Host '  3) 官方 SDK 加速路径：等 native/webview 落地后由 -DTN_WEBVIEW2_SDK 接入（尚未实现）'
