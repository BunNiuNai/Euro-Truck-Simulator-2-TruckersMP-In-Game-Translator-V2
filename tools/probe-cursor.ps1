# 探测悬浮窗边缘的「归属」：把光标移到指定点，读回该点下的窗口与当前鼠标指针形状。
#
# 原理：鼠标缩放要生效，必须满足两个条件——
#   a) 那个像素归**宿主窗口**（而不是 WebView2 子窗口）所有，宿主才能收到 WM_NCHITTEST；
#   b) 宿主对 WM_NCHITTEST 返回了缩放类 HT 码，系统才会给出缩放指针。
# 读回的形状是缩放箭头就说明两条都成立，这是从外部能拿到的最硬的证据
# （比截图可靠：截图只能看出颜色，看不出谁在接管鼠标）。
#
# 用法:
#   pwsh -File tools\probe-cursor.ps1                     # 探窗口四条边
#   pwsh -File tools\probe-cursor.ps1 -X 815 -Y 623       # 探指定屏幕坐标

param([int]$X = [int]::MinValue, [int]$Y = [int]::MinValue)

Add-Type @'
using System;
using System.Collections.Generic;
using System.Runtime.InteropServices;
public class CurProbe {
    [DllImport("user32.dll")] public static extern bool SetProcessDPIAware();
    [DllImport("user32.dll", CharSet=CharSet.Unicode, EntryPoint="FindWindowW")]
    public static extern IntPtr FindWindowClass(string cls, IntPtr win);
    [DllImport("user32.dll")] public static extern bool GetWindowRect(IntPtr h, out RECT r);
    [DllImport("user32.dll")] public static extern IntPtr WindowFromPoint(POINT p);
    [DllImport("user32.dll")] public static extern bool ScreenToClient(IntPtr h, ref POINT p);
    [DllImport("user32.dll")] public static extern bool GetCursorPos(out POINT p);
    [DllImport("user32.dll")] public static extern bool SetCursorPos(int x, int y);
    [DllImport("user32.dll")] public static extern IntPtr GetCursorInfo(ref CURSORINFO ci);
    [DllImport("user32.dll")] public static extern IntPtr LoadCursorW(IntPtr inst, IntPtr name);
    [DllImport("user32.dll", CharSet=CharSet.Unicode)] public static extern int GetClassNameW(IntPtr h, System.Text.StringBuilder s, int n);
    [DllImport("user32.dll")] public static extern int GetSystemMetrics(int i);

    [StructLayout(LayoutKind.Sequential)] public struct RECT { public int Left, Top, Right, Bottom; }
    [StructLayout(LayoutKind.Sequential)] public struct POINT { public int X, Y; }
    [StructLayout(LayoutKind.Sequential)] public struct CURSORINFO {
        public int cbSize; public int flags; public IntPtr hCursor; public POINT pt;
    }

    public static string ClassOf(IntPtr h) {
        var sb = new System.Text.StringBuilder(256);
        GetClassNameW(h, sb, 256);
        return sb.ToString();
    }
    public static IntPtr CursorNow() {
        var ci = new CURSORINFO(); ci.cbSize = Marshal.SizeOf(typeof(CURSORINFO));
        GetCursorInfo(ref ci);
        return ci.hCursor;
    }
}
'@

[void][CurProbe]::SetProcessDPIAware()

# 系统预定义指针 ID（IDC_*）
$IDC = @{
    'IDC_ARROW'      = [IntPtr]32512
    'IDC_IBEAM'      = [IntPtr]32513
    'IDC_SIZEWE'     = [IntPtr]32644
    'IDC_SIZENS'     = [IntPtr]32645
    'IDC_SIZENWSE'   = [IntPtr]32642
    'IDC_SIZENESW'   = [IntPtr]32643
    'IDC_SIZEALL'    = [IntPtr]32646
    'IDC_HAND'       = [IntPtr]32649
    'IDC_NO'         = [IntPtr]32648
}
$byHandle = @{}
foreach ($k in $IDC.Keys) { $byHandle[[int64][CurProbe]::LoadCursorW([IntPtr]::Zero, $IDC[$k])] = $k }

function Name-Cursor([IntPtr]$h) {
    $n = $byHandle[[int64]$h]
    if ($n) { return $n }
    return ('未知(0x{0:X})' -f [int64]$h)
}

$hwnd = [CurProbe]::FindWindowClass('ETS2TranslatorOverlayV2', [IntPtr]::Zero)
if ($hwnd -eq [IntPtr]::Zero) { Write-Output '未找到悬浮窗'; exit 2 }

$r = New-Object CurProbe+RECT
[void][CurProbe]::GetWindowRect($hwnd, [ref]$r)
$w = $r.Right - $r.Left; $h = $r.Bottom - $r.Top

Write-Output "悬浮窗 = $($r.Left),$($r.Top)  ${w}x${h}"
Write-Output "屏幕   = $([CurProbe]::GetSystemMetrics(0))x$([CurProbe]::GetSystemMetrics(1))"

# 保存原光标位置，结束时还原——不把用户的鼠标留在别处
$orig = New-Object CurProbe+POINT
[void][CurProbe]::GetCursorPos([ref]$orig)

$points = @()
if ($X -ne [int]::MinValue) {
    $points += ,@('指定点', $X, $Y)
} else {
    $cx = $r.Left + [int]($w / 2)
    $cy = $r.Top + [int]($h / 2)

    # ⚠️ 所有算术必须先算好、再放进数组。
    #    PowerShell 里逗号的优先级**高于**加号：`'名字', $r.Left + 3, $cy`
    #    会被解析成 `('名字', $r.Left) + (3, $cy)`——数组拼接，坐标全错，
    #    探针点会跑到屏幕外去（曾因此得到一堆毫无意义的探测结果）。
    $L3  = $r.Left + 3
    $L12 = $r.Left + 12
    $R3  = $r.Right - 4
    $B3  = $r.Bottom - 4
    $T3  = $r.Top + 3
    $T12 = $r.Top + 12

    # 内外各取一个点：内缩圈是 6 逻辑 px，150% 缩放下约 9 物理 px，
    # 所以 +3（圈内）与 +12（圈外、内容区）正好跨过那条分界线。
    $points += ,@('左边缘  圈内(+3)',  $L3,  $cy)
    $points += ,@('左边缘  圈外(+12)', $L12, $cy)
    $points += ,@('右边缘  圈内(-4)',  $R3,  $cy)
    $points += ,@('上边缘  圈内(+3)',  $cx,  $T3)
    $points += ,@('上边缘  圈外(+12)', $cx,  $T12)
    $points += ,@('下边缘  圈内(-4)',  $cx,  $B3)
    $points += ,@('左上角  圈内(+3)',  $L3,  $T3)
    $points += ,@('窗口正中(对照)',    $cx,  $cy)
}

Write-Output ''
foreach ($p in $points) {
    $name = $p[0]; $px = $p[1]; $py = $p[2]
    [void][CurProbe]::SetCursorPos($px, $py)
    Start-Sleep -Milliseconds 220

    $pt = New-Object CurProbe+POINT
    $pt.X = $px; $pt.Y = $py
    $under = [CurProbe]::WindowFromPoint($pt)
    $cls = if ($under -eq [IntPtr]::Zero) { '<无>' } else { [CurProbe]::ClassOf($under) }

    # 该点相对宿主窗口客户区的位置
    $cp = New-Object CurProbe+POINT
    $cp.X = $px; $cp.Y = $py
    [void][CurProbe]::ScreenToClient($hwnd, [ref]$cp)

    $cur = [CurProbe]::CursorNow()
    $isResize = (Name-Cursor $cur) -like 'IDC_SIZE*'

    Write-Output ("{0,-18} ({1,5},{2,5})  客户区({3,4},{4,4})  归属={5,-26} 指针={6,-14} {7}" -f `
        $name, $px, $py, $cp.X, $cp.Y, $cls, (Name-Cursor $cur), $(if ($isResize) { '<== 缩放可用' } else { '' }))
}

[void][CurProbe]::SetCursorPos($orig.X, $orig.Y)
Write-Output ''
Write-Output "光标已还原到 $($orig.X),$($orig.Y)"
