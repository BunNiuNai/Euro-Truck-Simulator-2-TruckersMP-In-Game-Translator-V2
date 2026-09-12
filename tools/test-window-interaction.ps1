# 用真实的合成鼠标输入测试悬浮窗的交互能力：拖动 / 缩放 / 右键菜单。
#
# 为什么必须用真实输入（mouse_event）而不是 PostMessage：
#   PostMessage 只往目标窗口的消息队列塞一条消息，绕过了 Windows 的命中测试
#   与鼠标捕获——那样测出来的「成功」是假的。真实注入走的是和用户手一样
#   的输入路径，测出来的结论才可信。
#
# 用法:
#   pwsh -File tools\test-window-interaction.ps1 -Test drag
#   pwsh -File tools\test-window-interaction.ps1 -Test resize
#   pwsh -File tools\test-window-interaction.ps1 -Test menu
#   pwsh -File tools\test-window-interaction.ps1 -Test all

param(
    [ValidateSet('drag', 'resize', 'menu', 'settings', 'all')]
    [string]$Test = 'all',
    [string]$ShotDir = 'Y:\翻译器项目\.shots'
)

Add-Type @'
using System;
using System.Runtime.InteropServices;
public class Act {
    [DllImport("user32.dll")] public static extern bool SetProcessDPIAware();
    [DllImport("user32.dll", CharSet=CharSet.Unicode, EntryPoint="FindWindowW")]
    public static extern IntPtr FindWindowClass(string cls, IntPtr win);
    [DllImport("user32.dll")] public static extern bool GetWindowRect(IntPtr h, out RECT r);
    [DllImport("user32.dll")] public static extern bool GetCursorPos(out POINT p);
    [DllImport("user32.dll")] public static extern bool SetCursorPos(int x, int y);
    [DllImport("user32.dll")] public static extern void mouse_event(uint f, int dx, int dy, uint d, IntPtr e);
    // 之前漏了这一个声明：脚本走到右键测试的收尾（按 ESC 关菜单）才会抛
    // 「找不到方法」，而且是在**测试做完之后**才炸，很容易被当成误报。
    [DllImport("user32.dll")] public static extern void keybd_event(byte vk, byte scan, uint flags, IntPtr extra);
    // 用来给设置窗口发 WM_CLOSE(0x0010)——测试完不能把它留在用户屏幕上。
    [DllImport("user32.dll")] public static extern bool PostMessageW(IntPtr h, uint msg, IntPtr wp, IntPtr lp);
    [DllImport("user32.dll")] public static extern IntPtr WindowFromPoint(POINT p);
    [DllImport("user32.dll", CharSet=CharSet.Unicode)] public static extern int GetClassNameW(IntPtr h, System.Text.StringBuilder s, int n);
    [StructLayout(LayoutKind.Sequential)] public struct RECT { public int Left, Top, Right, Bottom; }
    [StructLayout(LayoutKind.Sequential)] public struct POINT { public int X, Y; }
    public static string ClassOf(IntPtr h) {
        var sb = new System.Text.StringBuilder(256); GetClassNameW(h, sb, 256); return sb.ToString();
    }
}
'@

[void][Act]::SetProcessDPIAware()

$MDOWN = 0x0002; $MUP = 0x0004; $RDOWN = 0x0008; $RUP = 0x0010

function Get-Rect {
    $r = New-Object Act+RECT
    [void][Act]::GetWindowRect($script:hwnd, [ref]$r)
    return $r
}

function Move-To([int]$x, [int]$y) {
    [void][Act]::SetCursorPos($x, $y)
    Start-Sleep -Milliseconds 60
}

# 按下 → 分步移动 → 抬起。分步是必须的：一步跳到位的话
# 目标窗口只会收到「跳变后的位置」，拖动环很可能不认。
function Invoke-Drag([int]$fromX, [int]$fromY, [int]$dx, [int]$dy, [int]$steps = 12) {
    Move-To $fromX $fromY
    [Act]::mouse_event($MDOWN, 0, 0, 0, [IntPtr]::Zero)
    Start-Sleep -Milliseconds 120
    for ($i = 1; $i -le $steps; $i++) {
        $nx = $fromX + [int]($dx * $i / $steps)
        $ny = $fromY + [int]($dy * $i / $steps)
        Move-To $nx $ny
    }
    Start-Sleep -Milliseconds 120
    [Act]::mouse_event($MUP, 0, 0, 0, [IntPtr]::Zero)
    Start-Sleep -Milliseconds 250
}

$script:hwnd = [Act]::FindWindowClass('ETS2TranslatorOverlayV2', [IntPtr]::Zero)
if ($script:hwnd -eq [IntPtr]::Zero) { Write-Output '未找到悬浮窗'; exit 2 }

$orig = New-Object Act+POINT
[void][Act]::GetCursorPos([ref]$orig)

$before = Get-Rect
$w = $before.Right - $before.Left
$h = $before.Bottom - $before.Top
Write-Output ("初始: {0},{1}  {2}x{3}" -f $before.Left, $before.Top, $w, $h)
Write-Output ''

$pass = 0; $fail = 0

# ── 拖动 ──
# 标题带：.accent-line(2 CSS px) + .header(32 CSS px)，都在 WebView 内容里。
# 150% 缩放下顶部内缩约 9 物理 px，所以标题带在窗口顶部向下约 9..60px。
if ($Test -eq 'drag' -or $Test -eq 'all') {
    $cx = $before.Left + [int]($w / 2)
    $cy = $before.Top + 30

    Write-Output "── 拖动测试 ──  抓取点 ($cx,$cy)  拖动 (+140,+80)"
    $pt = New-Object Act+POINT; $pt.X = $cx; $pt.Y = $cy
    Write-Output ("  该点归属: {0}" -f [Act]::ClassOf([Act]::WindowFromPoint($pt)))

    Invoke-Drag $cx $cy 140 80
    $after = Get-Rect
    $mx = $after.Left - $before.Left
    $my = $after.Top - $before.Top
    Write-Output ("  结果: 移动 ({0},{1})  期望约 (140,80)" -f $mx, $my)

    if ([Math]::Abs($mx) -gt 100 -and [Math]::Abs($my) -gt 50) {
        Write-Output '  [PASS] 拖动生效'; $pass++
    } else {
        Write-Output '  [FAIL] 窗口没动 —— app-region: drag 未生效'; $fail++
    }
    $before = $after
    $w = $before.Right - $before.Left; $h = $before.Bottom - $before.Top
    Write-Output ''
}

# ── 缩放 ──
if ($Test -eq 'resize' -or $Test -eq 'all') {
    $gx = $before.Right - 3
    $gy = $before.Bottom - 3

    Write-Output "── 缩放测试 ──  抓取点 ($gx,$gy)（右下角内 3px）  拖动 (+120,+90)"
    $pt = New-Object Act+POINT; $pt.X = $gx; $pt.Y = $gy
    Write-Output ("  该点归属: {0}" -f [Act]::ClassOf([Act]::WindowFromPoint($pt)))

    Invoke-Drag $gx $gy 120 90
    $after = Get-Rect
    $dw = ($after.Right - $after.Left) - $w
    $dh = ($after.Bottom - $after.Top) - $h
    Write-Output ("  结果: 尺寸变化 ({0},{1})  期望约 (120,90)" -f $dw, $dh)

    if ([Math]::Abs($dw) -gt 80 -and [Math]::Abs($dh) -gt 60) {
        Write-Output '  [PASS] 缩放生效'; $pass++
    } else {
        Write-Output '  [FAIL] 尺寸没变 —— 边缘没交给宿主窗口，或缺少 WS_THICKFRAME'; $fail++
    }
    $before = $after
    Write-Output ''
}

# ── 右键菜单 ──
#
# 右键弹的是**前端自绘的 DOM 元素**（V1 的 Settings/Exit 两项），从进程外面
# 既看不到它、也没有任何服务端信号。所以判定方式是「右键前后截图有没有变化」：
# 只要菜单弹出来了，像素一定会变。这比让人工看图可靠，也能自动判 PASS/FAIL。
#
# ⚠️ 别再去找 #32768（系统菜单）：那需要 app-region 生效，而那条路已被否掉
#    （见 native/src/webview.cpp 的调查记录）。照旧写法会永远探测不到。
if ($Test -eq 'menu' -or $Test -eq 'all') {
    $cx = $before.Left + [int](($before.Right - $before.Left) / 2)
    $cy = $before.Top + [int](($before.Bottom - $before.Top) / 2)   # 消息区正中

    Write-Output "── 右键测试 ──  消息区正中 ($cx,$cy)"
    if (-not (Test-Path $ShotDir)) { New-Item -ItemType Directory -Force -Path $ShotDir | Out-Null }

    $shotBefore = Join-Path $ShotDir 'ctx-before.png'
    $shotAfter = Join-Path $ShotDir 'ctx-after.png'

    Move-To 0 0  # 先把光标挪开，免得它自己的悬停高亮污染对比
    Start-Sleep -Milliseconds 300
    & (Join-Path $PSScriptRoot 'capture-overlay.ps1') -Out $shotBefore | Out-Null

    Move-To $cx $cy
    [Act]::mouse_event($RDOWN, 0, 0, 0, [IntPtr]::Zero)
    Start-Sleep -Milliseconds 60
    [Act]::mouse_event($RUP, 0, 0, 0, [IntPtr]::Zero)
    Start-Sleep -Milliseconds 700

    # 截图会把光标画进去吗？不会——CopyFromScreen 不含光标，所以菜单本身
    # 是唯一的变量。
    & (Join-Path $PSScriptRoot 'capture-overlay.ps1') -Out $shotAfter | Out-Null

    Add-Type -AssemblyName System.Drawing
    $a = [System.Drawing.Bitmap]::FromFile($shotBefore)
    $b = [System.Drawing.Bitmap]::FromFile($shotAfter)
    $diff = 0
    $total = 0
    if ($a.Width -eq $b.Width -and $a.Height -eq $b.Height) {
        for ($y = 0; $y -lt $a.Height; $y += 2) {
            for ($x = 0; $x -lt $a.Width; $x += 2) {
                $pa = $a.GetPixel($x, $y); $pb = $b.GetPixel($x, $y)
                $total++
                if ([Math]::Abs($pa.R - $pb.R) -gt 12 -or [Math]::Abs($pa.G - $pb.G) -gt 12 -or [Math]::Abs($pa.B - $pb.B) -gt 12) { $diff++ }
            }
        }
    }
    $a.Dispose(); $b.Dispose()
    $pct = if ($total -gt 0) { [Math]::Round(100 * $diff / $total, 3) } else { 0 }
    Write-Output ("  像素变化: {0} / {1} = {2}%   （截图: $shotBefore / $shotAfter）" -f $diff, $total, $pct)

    if ($pct -gt 0.5) {
        Write-Output '  [PASS] 右键弹出了菜单（画面出现明显变化）'; $pass++
    } else {
        Write-Output '  [FAIL] 右键前后画面几乎没变 —— 菜单没弹出来'; $fail++
    }

    # 关掉菜单，别把它留在屏幕上
    [Act]::keybd_event(0x1B, 0, 0, [IntPtr]::Zero)          # ESC down
    [Act]::keybd_event(0x1B, 0, 0x0002, [IntPtr]::Zero)     # ESC up
    Start-Sleep -Milliseconds 300
    Write-Output ''
}

# ── 设置窗口（右键 → Settings / 设置）──
#
# 这条链路一次串起四层：前端自绘菜单 → window.open → WebView2 的
# NewWindowRequested → native 创建第二个窗口与 Controller。
# 任何一层断掉，用户看到的都是「点设置没反应」或一句误导性的提示。
if ($Test -eq 'settings' -or $Test -eq 'all') {
    $cx = $before.Left + [int](($before.Right - $before.Left) / 2)
    $cy = $before.Top + [int](($before.Bottom - $before.Top) / 2)

    # 先清掉可能残留的设置窗口，否则「已经开着」会让测试失去意义
    $existing = [Act]::FindWindowClass('ETS2TranslatorSettingsV2', [IntPtr]::Zero)
    if ($existing -ne [IntPtr]::Zero) {
        [void][Act]::PostMessageW($existing, 0x0010, [IntPtr]::Zero, [IntPtr]::Zero)
        Start-Sleep -Milliseconds 800
    }

    Write-Output "── 设置窗口测试 ──  在 ($cx,$cy) 右键，再点第一项「Settings / 设置」"

    if (-not (Test-Path $ShotDir)) { New-Item -ItemType Directory -Force -Path $ShotDir | Out-Null }

    # 菜单是**前端自绘的 DOM 元素**，位置 = 右键点的 CSS 坐标 × 显示缩放。
    # 硬编码偏移在这个环境里很容易点空（本机 150% 缩放，算错一个比例就偏出去），
    # 而点空**不会报任何错**——只表现为「设置窗口没出现」，极难定位。
    #
    # 所以先量：右键前后各截一张图，取**差异像素的外接矩形**，那就是菜单。
    $shotBefore = Join-Path $ShotDir 'settings-menu-before.png'
    $shotAfter = Join-Path $ShotDir 'settings-menu-after.png'
    Move-To 0 0
    Start-Sleep -Milliseconds 300
    & (Join-Path $PSScriptRoot 'capture-overlay.ps1') -Out $shotBefore | Out-Null

    Move-To $cx $cy
    [Act]::mouse_event($RDOWN, 0, 0, 0, [IntPtr]::Zero)
    Start-Sleep -Milliseconds 60
    [Act]::mouse_event($RUP, 0, 0, 0, [IntPtr]::Zero)
    Start-Sleep -Milliseconds 700
    & (Join-Path $PSScriptRoot 'capture-overlay.ps1') -Out $shotAfter | Out-Null

    Add-Type -AssemblyName System.Drawing
    $ba = [System.Drawing.Bitmap]::FromFile($shotBefore)
    $bb = [System.Drawing.Bitmap]::FromFile($shotAfter)
    $minX = [int]::MaxValue; $minY = [int]::MaxValue; $maxX = -1; $maxY = -1
    for ($y = 0; $y -lt [Math]::Min($ba.Height, $bb.Height); $y++) {
        for ($x = 0; $x -lt [Math]::Min($ba.Width, $bb.Width); $x++) {
            $pa = $ba.GetPixel($x, $y); $pb = $bb.GetPixel($x, $y)
            if ([Math]::Abs($pa.R - $pb.R) -gt 12 -or [Math]::Abs($pa.G - $pb.G) -gt 12 -or [Math]::Abs($pa.B - $pb.B) -gt 12) {
                if ($x -lt $minX) { $minX = $x }
                if ($x -gt $maxX) { $maxX = $x }
                if ($y -lt $minY) { $minY = $y }
                if ($y -gt $maxY) { $maxY = $y }
            }
        }
    }
    $ba.Dispose(); $bb.Dispose()

    if ($maxX -lt 0) {
        Write-Output '  [FAIL] 右键没弹出菜单（差异区域为空），后面的链路无从谈起'
        $fail++
    } else {
        # 截图坐标系 = 窗口客户区，加上窗口原点才是屏幕坐标
        $menuLeft = $before.Left + $minX
        $menuTop = $before.Top + $minY
        $menuW = $maxX - $minX + 1
        $menuH = $maxY - $minY + 1
        Write-Output ("  量到菜单：屏幕 ({0},{1}) 尺寸 {2}x{3}" -f $menuLeft, $menuTop, $menuW, $menuH)

        # 第一项占菜单顶部约 22%（条目 ≈24 CSS px，其后才是分隔线）
        $ix = $menuLeft + [int]($menuW / 2)
        $iy = $menuTop + [int]($menuH * 0.22)
        Write-Output "  点击第一项 ($ix,$iy)"

        Move-To $ix $iy
        [Act]::mouse_event($MDOWN, 0, 0, 0, [IntPtr]::Zero)
        Start-Sleep -Milliseconds 60
        [Act]::mouse_event($MUP, 0, 0, 0, [IntPtr]::Zero)
    }

    # 等窗口出现——WebView2 的 controller 创建是异步的，不会立刻就有
    $found = [IntPtr]::Zero
    for ($i = 0; $i -lt 30; $i++) {
        Start-Sleep -Milliseconds 200
        $found = [Act]::FindWindowClass('ETS2TranslatorSettingsV2', [IntPtr]::Zero)
        if ($found -ne [IntPtr]::Zero) { break }
    }

    if ($found -eq [IntPtr]::Zero) {
        Write-Output '  [FAIL] 设置窗口没有出现（链路：菜单 → window.open → native 建窗）'
        $fail++
    } else {
        $sr = New-Object Act+RECT
        [void][Act]::GetWindowRect($found, [ref]$sr)
        $sw = $sr.Right - $sr.Left
        $sh = $sr.Bottom - $sr.Top
        Write-Output ("  [PASS] 设置窗口已创建：{0},{1} {2}x{3}（期望约 540x700 × 显示缩放）" -f $sr.Left, $sr.Top, $sw, $sh)
        $pass++

        # 截图留档：窗口出现不等于页面加载成功，得看一眼里面有没有内容
        if (-not (Test-Path $ShotDir)) { New-Item -ItemType Directory -Force -Path $ShotDir | Out-Null }
        $shot = Join-Path $ShotDir 'settings.png'
        Add-Type -AssemblyName System.Drawing
        $bmp = New-Object System.Drawing.Bitmap($sw, $sh)
        $g = [System.Drawing.Graphics]::FromImage($bmp)
        $g.CopyFromScreen($sr.Left, $sr.Top, 0, 0, (New-Object System.Drawing.Size($sw, $sh)))
        $g.Dispose()
        $bmp.Save($shot, [System.Drawing.Imaging.ImageFormat]::Png)
        $bmp.Dispose()
        Write-Output "  截图: $shot"

        # 关掉它，别留在用户屏幕上
        [void][Act]::PostMessageW($found, 0x0010, [IntPtr]::Zero, [IntPtr]::Zero)
        Start-Sleep -Milliseconds 600
    }
    Write-Output ''
}

[void][Act]::SetCursorPos($orig.X, $orig.Y)
Write-Output "── 汇总: 通过 $pass  失败 $fail ──"
Write-Output "光标已还原到 $($orig.X),$($orig.Y)"
