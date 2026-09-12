# 从外部核实悬浮窗的窗口样式与几何，用于诊断「拖不动 / 缩放不了 / 右键无反应」。
#
# 检查四件事（每一项都对应一个曾经真实踩过的坑）：
#   1. WS_THICKFRAME  —— 没有它，Windows 根本不启动缩放循环，
#                        WM_NCHITTEST 返回 HTBOTTOMRIGHT 也是白搭。
#   2. 客户区 == 窗口矩形  —— WM_NCCALCSIZE 返回 0 的效果；
#                        不相等说明边框回来了，视觉上会多一圈。
#   3. WebView2 子窗口是否内缩了 kResizeInset(6px) ——
#                        正是这一圈把鼠标缩放让给了宿主窗口。
#   4. WebView2 子窗口是否存在 —— 不存在说明显示层没起来。
#
# 用法: pwsh -File tools\inspect-overlay-window.ps1

Add-Type @'
using System;
using System.Text;
using System.Collections.Generic;
using System.Runtime.InteropServices;

public class WinInspect {
    // ⚠️ 第二个参数必须是 IntPtr 而不是 string：
    //    PowerShell 绑定 [string] 参数时会把 $null 转成空串，
    //    FindWindowW(cls, "") 就变成「找标题为空的窗口」——永远找不到。
    [DllImport("user32.dll", CharSet=CharSet.Unicode, EntryPoint="FindWindowW")]
    public static extern IntPtr FindWindowClass(string cls, IntPtr win);
    [DllImport("user32.dll")]
    public static extern IntPtr GetWindowLongPtrW(IntPtr h, int i);
    [DllImport("user32.dll")]
    public static extern bool GetWindowRect(IntPtr h, out RECT r);
    [DllImport("user32.dll")]
    public static extern bool GetClientRect(IntPtr h, out RECT r);
    [DllImport("user32.dll")]
    public static extern bool IsWindowVisible(IntPtr h);
    [DllImport("user32.dll", CharSet=CharSet.Unicode)]
    public static extern int GetClassNameW(IntPtr h, StringBuilder s, int n);

    public delegate bool EnumProc(IntPtr h, IntPtr p);
    [DllImport("user32.dll")]
    public static extern bool EnumChildWindows(IntPtr parent, EnumProc cb, IntPtr p);

    [StructLayout(LayoutKind.Sequential)]
    public struct RECT { public int Left, Top, Right, Bottom; }

    public static List<string> Children(IntPtr parent) {
        var list = new List<string>();
        EnumChildWindows(parent, delegate(IntPtr h, IntPtr p) {
            var sb = new StringBuilder(256);
            GetClassNameW(h, sb, 256);
            RECT r; GetWindowRect(h, out r);
            list.Add(string.Format("{0}|{1},{2},{3},{4}|vis={5}",
                sb.ToString(), r.Left, r.Top, r.Right - r.Left, r.Bottom - r.Top,
                IsWindowVisible(h)));
            return true;
        }, IntPtr.Zero);
        return list;
    }
}
'@

$WS_THICKFRAME  = 0x00040000
$WS_POPUP       = 0x80000000
$WS_CAPTION     = 0x00C00000
$WS_EX_TOOLWIN  = 0x00000080
$WS_EX_TOPMOST  = 0x00000008
$WS_EX_LAYERED  = 0x00080000
$WS_EX_TRANSPARENT = 0x00000020

$hwnd = [WinInspect]::FindWindowClass('ETS2TranslatorOverlayV2', [IntPtr]::Zero)
if ($hwnd -eq [IntPtr]::Zero) {
    Write-Output '未找到悬浮窗（类名 ETS2TranslatorOverlayV2）——程序没起来，或窗口已隐藏。'
    exit 2
}

Write-Output ('HWND = 0x{0:X}' -f [int64]$hwnd)

# ── 1. 样式 ──
$style   = [int64][WinInspect]::GetWindowLongPtrW($hwnd, -16)   # GWL_STYLE
$exStyle = [int64][WinInspect]::GetWindowLongPtrW($hwnd, -20)   # GWL_EXSTYLE

$checks = @(
    @{ Name = 'WS_THICKFRAME (DWM 视作可缩放窗)'; Ok = ($style -band $WS_THICKFRAME) -ne 0; Hint = '' }
    @{ Name = 'WS_POPUP      (无边框)';   Ok = ($style -band $WS_POPUP) -ne 0;      Hint = '缺它 → 会多出标题栏' }
    @{ Name = 'WS_CAPTION 不出现 (应为否)'; Ok = ($style -band $WS_CAPTION) -eq 0;  Hint = '出现 → 画出了系统标题栏' }
    @{ Name = 'WS_EX_TOOLWINDOW (不进任务栏)'; Ok = ($exStyle -band $WS_EX_TOOLWIN) -ne 0; Hint = '' }
    # ⚠️ 这一条是重点。曾经因为配置里 click_through 残留为 true，窗口长时间处于
    #    穿透状态，拖动/缩放/右键**全部失效**，查了很久才定位到。穿透的窗口收不到
    #    任何鼠标消息，而外观上完全看不出异常——必须显式检查这一位。
    @{ Name = 'WS_EX_TRANSPARENT 不出现 (必须为否)'; Ok = ($exStyle -band $WS_EX_TRANSPARENT) -eq 0; Hint = '出现 → 鼠标穿透开着，窗口既点不动也拖不动' }
    @{ Name = '窗口可见'; Ok = [WinInspect]::IsWindowVisible($hwnd); Hint = '' }
)

Write-Output ''
Write-Output '── 窗口样式 ──'
Write-Output ('  style   = 0x{0:X8}' -f $style)
Write-Output ('  exStyle = 0x{0:X8}' -f $exStyle)
foreach ($c in $checks) {
    Write-Output ('  [{0}] {1}  {2}' -f $(if ($c.Ok) { 'OK  ' } else { 'FAIL' }), $c.Name, $c.Hint)
}

# ── 2. 客户区 == 窗口矩形 ──
$wr = New-Object WinInspect+RECT
$cr = New-Object WinInspect+RECT
[void][WinInspect]::GetWindowRect($hwnd, [ref]$wr)
[void][WinInspect]::GetClientRect($hwnd, [ref]$cr)

$ww = $wr.Right - $wr.Left;  $wh = $wr.Bottom - $wr.Top
$cw = $cr.Right - $cr.Left;  $ch = $cr.Bottom - $cr.Top

Write-Output ''
Write-Output '── 几何 ──'
Write-Output ('  窗口矩形 = {0},{1} {2}x{3}' -f $wr.Left, $wr.Top, $ww, $wh)
Write-Output ('  客户区   = {0}x{1}' -f $cw, $ch)
if ($cw -eq $ww -and $ch -eq $wh) {
    Write-Output '  [OK  ] 客户区铺满窗口 → WM_NCCALCSIZE 返回 0 生效，无边框占位'
} else {
    Write-Output ('  [FAIL] 客户区比窗口小 {0}x{1}px → 边框占位回来了' -f ($ww - $cw), ($wh - $ch))
}

# ── 3. WebView2 子窗口 ──
$kids = [WinInspect]::Children($hwnd)
Write-Output ''
Write-Output ('── 子窗口 ({0} 个) ──' -f $kids.Count)
foreach ($k in $kids) {
    $p = $k -split '\|'
    Write-Output ('  {0,-34} {1}' -f $p[0], $p[1] + '  ' + $p[2])
}

# 内缩检查：找 WebView2 的宿主子窗口，看它相对窗口的偏移。
#
# ⚠️ 每个减法都要单独加括号。PowerShell 里 `[int]$xy[0] - $wr.Left` 会被
#    解析成「先对数组做 [int] 转换再相减」，抛 op_Subtraction 找不到的错。
$inset = $null
foreach ($k in $kids) {
    $p = $k -split '\|'
    if ($p[0] -like 'Chrome_WidgetWin*' -or $p[0] -like '*WebView*') {
        $xy = $p[1] -split ','
        $dx = ([int]$xy[0]) - $wr.Left
        $dy = ([int]$xy[1]) - $wr.Top
        $inset = @($dx, $dy, [int]$xy[2], [int]$xy[3])
        break
    }
}

Write-Output ''
if ($inset -eq $null) {
    Write-Output '  [FAIL] 没有 WebView2 子窗口 → 显示层没起来（检查上面的 stdout 日志）'
} elseif ($inset[0] -eq 0 -and $inset[1] -eq 0) {
    # 满幅是**期望**状态：曾经试过让 WebView 内缩 6px，把边缘留给宿主窗口做缩放，
    # 但那只裁剪绘制、不裁剪命中测试（见 native/src/webview.cpp 的 kResizeInset）。
    # 缩放现在走前端 mousedown → IPC，与这里无关。
    Write-Output ('  [OK  ] WebView 满幅 {0},{1}，尺寸 {2}x{3}（与 V1 观感一致；缩放不经由边缘命中测试）' -f $inset[0], $inset[1], $inset[2], $inset[3])
} else {
    Write-Output ('  [WARN] WebView 偏移 {0},{1} → 会露出一圈背景色；若这是有意内缩请更新本检查' -f $inset[0], $inset[1])
}
