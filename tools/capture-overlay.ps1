# 截取悬浮窗（可按边距外扩，用来连带截出弹出菜单等窗口外的内容）。
#
# 用法:
#   pwsh -File tools\capture-overlay.ps1 -Out shot.png
#   pwsh -File tools\capture-overlay.ps1 -Out shot.png -Margin 260
#
# 用屏幕拷贝而不是 PrintWindow：WebView2 走 DirectComposition 合成，
# PrintWindow 拿到的常常是黑块或空白，而屏幕拷贝拿到的一定是用户眼睛看到的。

param(
    [string]$Out = 'overlay.png',
    [int]$Margin = 0
)

Add-Type -AssemblyName System.Drawing
Add-Type @'
using System;
using System.Runtime.InteropServices;
public class WinCap {
    // ⚠️ 必须在任何取坐标/截图之前调用，否则整个脚本拿到的是「虚拟化」坐标。
    //
    // 本机是 2560x1600 物理分辨率 + 150% 缩放，非 DPI-aware 的进程会看到
    // 1707x1067 的假分辨率：GetWindowRect 返回逻辑像素，而 GDI 的 BitBlt
    // 按物理像素取图——两者一混，截出来的是屏幕另一处完全不相干的内容
    // （曾因此误判成「窗口是透明的」）。
    [DllImport("user32.dll")] public static extern bool SetProcessDPIAware();
    [DllImport("user32.dll", CharSet=CharSet.Unicode, EntryPoint="FindWindowW")]
    public static extern IntPtr FindWindowClass(string cls, IntPtr win);
    [DllImport("user32.dll")] public static extern bool GetWindowRect(IntPtr h, out RECT r);
    [DllImport("user32.dll")] public static extern bool IsWindowVisible(IntPtr h);
    // 用 GetSystemMetrics 而不是 System.Windows.Forms.SystemInformation：
    // 后者要求调用方先 Add-Type 加载 WinForms，本脚本被别的脚本当子脚本调用时
    // 就找不到类型（曾因此整段截图静默失败，只留下一堆类型未找到的报错）。
    [DllImport("user32.dll")] public static extern int GetSystemMetrics(int i);
    [StructLayout(LayoutKind.Sequential)]
    public struct RECT { public int Left, Top, Right, Bottom; }
}
'@

[void][WinCap]::SetProcessDPIAware()

$hwnd = [WinCap]::FindWindowClass('ETS2TranslatorOverlayV2', [IntPtr]::Zero)
if ($hwnd -eq [IntPtr]::Zero) { Write-Output '未找到悬浮窗'; exit 2 }

$r = New-Object WinCap+RECT
[void][WinCap]::GetWindowRect($hwnd, [ref]$r)

$x = $r.Left - $Margin
$y = $r.Top - $Margin
$w = ($r.Right - $r.Left) + $Margin * 2
$h = ($r.Bottom - $r.Top) + $Margin * 2

# 屏幕外会被 CopyFromScreen 抛异常，夹回虚拟屏范围。
# SM_XVIRTUALSCREEN=76 SM_YVIRTUALSCREEN=77 SM_CXVIRTUALSCREEN=78 SM_CYVIRTUALSCREEN=79
$vsLeft = [WinCap]::GetSystemMetrics(76)
$vsTop = [WinCap]::GetSystemMetrics(77)
$vsW = [WinCap]::GetSystemMetrics(78)
$vsH = [WinCap]::GetSystemMetrics(79)
$x = [Math]::Max($vsLeft, [Math]::Min($x, $vsLeft + $vsW - 1))
$y = [Math]::Max($vsTop, [Math]::Min($y, $vsTop + $vsH - 1))
$w = [Math]::Min($w, $vsLeft + $vsW - $x)
$h = [Math]::Min($h, $vsTop + $vsH - $y)
if ($w -le 0 -or $h -le 0) { Write-Output "截图区域无效: ${w}x${h}"; exit 3 }

$bmp = New-Object System.Drawing.Bitmap($w, $h)
$g = [System.Drawing.Graphics]::FromImage($bmp)
$g.CopyFromScreen($x, $y, 0, 0, (New-Object System.Drawing.Size($w, $h)))
$g.Dispose()

$full = [System.IO.Path]::GetFullPath($Out)
$bmp.Save($full, [System.Drawing.Imaging.ImageFormat]::Png)
$bmp.Dispose()

Write-Output ("窗口 = {0},{1} {2}x{3}   截图 = {4},{5} {6}x{7}" -f $r.Left, $r.Top, ($r.Right - $r.Left), ($r.Bottom - $r.Top), $x, $y, $w, $h)
Write-Output $full
