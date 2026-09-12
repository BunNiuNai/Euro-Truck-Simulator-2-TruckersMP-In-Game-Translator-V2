# Translator V2 — WebView2 子进程回收探针
#
# 为什么需要它：native 退出后 WebView2 的 msedgewebview2.exe 是否回收，只能靠
# **数进程**来判定；手工操作既不可重复也说不清「哪几个进程是我们的」。
# 本脚本把「起 native → 建窗（可选带设置窗口）→ 走某种退出路径 → 数进程」
# 固化下来，并且顺带起一个只认 EventSource 的最小 HTTP 服务，用来观察
# 「宿主没了但页面还在，SSE 还在重连」这种幽灵订阅者。
#
# 用法（在任意目录）：
#   pwsh -File native\tools\leak-probe.ps1 -Mode shutdown
#   pwsh -File native\tools\leak-probe.ps1 -Mode shutdown -TwoWindows
#   pwsh -File native\tools\leak-probe.ps1 -Mode kill
#   pwsh -File native\tools\leak-probe.ps1 -Mode disconnect
#
# Mode 说明：
#   shutdown   往管道发 shutdown 帧（优雅退出，对应 Go 侧 /api/quit）
#   kill       Stop-Process -Force（强杀，只用于观察，不是要求修的场景）
#   disconnect 直接关掉管道客户端、native 继续活着（Go 崩溃时的真实形态）
#
# ⚠️ 进程归属判定：WebView2 会把宿主 exe 名与 user data 目录写进子进程命令行，
#    所以必须按 `--webview-exe-name=translator_native.exe` /
#    `ETS2 Translator V2\WebView2` 过滤。**不能**只数 msedgewebview2.exe 总数——
#    Windows 自己的 SearchHost.exe 也常年挂着一组同名的进程（实测 6 个），
#    混在一起数会得出完全错误的结论。

[CmdletBinding()]
param(
    [string]$PipeName = 'leak-probe',
    [int]$Port = 8791,
    [int]$SettleSeconds = 4,      # 建窗后等 WebView 建起来的时间
    [int]$AfterSeconds = 10,      # 退出后等回收的时间（题目要求的 10 秒）
    [switch]$TwoWindows,          # 是否连设置窗口一起开（共享 Environment 场景）
    [ValidateSet('shutdown', 'kill', 'disconnect', 'reconnect')]
    [string]$Mode = 'shutdown',
    [switch]$NoServer,            # 不起 HTTP 服务（只想数进程时用）
    [int]$TailPollSeconds = 0     # >0 时：T2 之后每 250ms 数一次，报「第几秒归零」
)

$ErrorActionPreference = 'Stop'
$root = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)   # ...\Translator-V2
$exe = Join-Path $root 'dist\translator_native.exe'
$pipeFull = "\\.\pipe\$PipeName"
$logDir = Join-Path $PSScriptRoot 'probe-logs'
New-Item -ItemType Directory -Force -Path $logDir | Out-Null
$stamp = Get-Date -Format 'yyyyMMdd-HHmmss'
$winTag = if ($TwoWindows) { 'two' } else { 'one' }
$outLog = Join-Path $logDir "native-$Mode-$winTag-$stamp.log"

# ── 数进程：把「我们的」和「别人的」分开 ──────────────────────
function Get-OurWebViews {
    $all = Get-CimInstance Win32_Process -Filter "Name='msedgewebview2.exe'" -ErrorAction SilentlyContinue
    $ours = @($all | Where-Object { $_.CommandLine -match 'ETS2 Translator V2' -or $_.CommandLine -match '--webview-exe-name=translator_native\.exe' })
    return $ours
}

function Get-WebViewSnapshot {
    $all = @(Get-CimInstance Win32_Process -Filter "Name='msedgewebview2.exe'" -ErrorAction SilentlyContinue)
    $ours = @(Get-OurWebViews)
    $browser = @($ours | Where-Object { $_.CommandLine -notmatch '--type=' })
    [pscustomobject]@{
        Total        = $all.Count
        Ours         = $ours.Count
        OursBrowsers = $browser.Count
        OursPids     = ($ours | ForEach-Object { $_.ProcessId }) -join ','
        BrowserPids  = ($browser | ForEach-Object { $_.ProcessId }) -join ','
    }
}

function Show-Snapshot([string]$label) {
    $s = Get-WebViewSnapshot
    Write-Host ("  [{0}] msedgewebview2 总数={1}  属于本程序={2}（其中浏览器主进程 {3} 个: {4}）" -f `
        $label, $s.Total, $s.Ours, $s.OursBrowsers, $s.BrowserPids)
    if ($s.OursPids) { Write-Host ("        本程序子进程 PID: {0}" -f $s.OursPids) }
    return $s
}

# ── 最小 HTTP + SSE 服务：页面里挂一个 EventSource ────────────
if (-not $NoServer) {
    $csharp = @'
using System;
using System.Net;
using System.Net.Sockets;
using System.Text;
using System.Threading;

public static class ProbeServer
{
    static TcpListener listener;
    static int subscribers;
    static int totalAccepted;
    static volatile bool running;

    public static int Subscribers { get { return subscribers; } }
    public static int TotalAccepted { get { return totalAccepted; } }

    public static bool Start(int port)
    {
        try
        {
            listener = new TcpListener(IPAddress.Loopback, port);
            listener.Start();
        }
        catch (Exception) { return false; }
        running = true;
        Thread t = new Thread(AcceptLoop);
        t.IsBackground = true;
        t.Start();
        return true;
    }

    public static void Stop()
    {
        running = false;
        try { if (listener != null) listener.Stop(); } catch (Exception) { }
    }

    static void AcceptLoop()
    {
        while (running)
        {
            TcpClient c;
            try { c = listener.AcceptTcpClient(); }
            catch (Exception) { return; }
            Thread w = new Thread(delegate() { Handle(c); });
            w.IsBackground = true;
            w.Start();
        }
    }

    static void Handle(TcpClient client)
    {
        bool isSse = false;
        try
        {
            client.NoDelay = true;
            NetworkStream s = client.GetStream();
            s.ReadTimeout = 5000;

            // 只读请求行与头部，够用即可
            StringBuilder head = new StringBuilder();
            byte[] one = new byte[1];
            while (head.Length < 8192)
            {
                int n = s.Read(one, 0, 1);
                if (n <= 0) break;
                head.Append((char)one[0]);
                string h = head.ToString();
                if (h.EndsWith("\r\n\r\n")) break;
            }
            string first = head.ToString().Split('\n')[0].Trim();
            bool sse = first.Contains("/api/events");
            isSse = sse;

            if (sse)
            {
                Interlocked.Increment(ref subscribers);
                Interlocked.Increment(ref totalAccepted);
                byte[] hdr = Encoding.ASCII.GetBytes(
                    "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nCache-Control: no-cache\r\n" +
                    "Connection: keep-alive\r\nAccess-Control-Allow-Origin: *\r\n\r\n" +
                    "event: hello\ndata: {\"ok\":true}\n\n");
                s.Write(hdr, 0, hdr.Length);
                s.Flush();
                byte[] ping = Encoding.ASCII.GetBytes(": ping\n\n");
                while (running)
                {
                    Thread.Sleep(1000);
                    s.Write(ping, 0, ping.Length);
                    s.Flush();
                }
            }
            else
            {
                byte[] body = Encoding.UTF8.GetBytes(
                    "<!doctype html><html><head><meta charset=\"utf-8\"><title>leak-probe</title></head>" +
                    "<body style=\"font-family:system-ui;background:#111;color:#eee\">" +
                    "<h1>leak-probe</h1><p id=\"st\">connecting…</p>" +
                    "<script>" +
                    "var es=new EventSource('/api/events');" +
                    "es.onopen=function(){document.getElementById('st').textContent='SSE open';};" +
                    "es.onerror=function(){document.getElementById('st').textContent='SSE error';};" +
                    "</script></body></html>");
                byte[] hdr = Encoding.ASCII.GetBytes(
                    "HTTP/1.1 200 OK\r\nContent-Type: text/html; charset=utf-8\r\nContent-Length: " +
                    body.Length + "\r\nConnection: close\r\nCache-Control: no-store\r\n\r\n");
                s.Write(hdr, 0, hdr.Length);
                s.Write(body, 0, body.Length);
                s.Flush();
                client.Close();
                return;
            }
        }
        catch (Exception) { }
        finally
        {
            try { if (client.Connected) client.Close(); } catch (Exception) { }
            // 走到这里说明这条 SSE 连接结束了（客户端没了）。订阅者计数必须跟着减，
            // 否则「残留订阅者」这个判据永远是正的，什么都没法证明。
            if (isSse) Interlocked.Decrement(ref subscribers);
        }
    }
}
'@
    Add-Type -TypeDefinition $csharp -Language CSharp

    if (-not [ProbeServer]::Start($Port)) {
        Write-Warning "端口 $Port 起不来，改用 -NoServer 或换 -Port"
        exit 3
    }
    Write-Host "SSE 探针服务已启动: http://127.0.0.1:$Port/"
}

# ── 管道客户端：帧格式 [4字节小端长度][UTF-8 JSON] ────────────
function Connect-Pipe([int]$timeoutMs = 8000) {
    $p = [System.IO.Pipes.NamedPipeClientStream]::new('.', $PipeName,
        [System.IO.Pipes.PipeDirection]::InOut, [System.IO.Pipes.PipeOptions]::None)
    $p.Connect($timeoutMs)
    return $p
}

function Send-Frame($pipe, [hashtable]$msg) {
    $json = $msg | ConvertTo-Json -Depth 8 -Compress
    $bytes = [System.Text.Encoding]::UTF8.GetBytes($json)
    $len = [BitConverter]::GetBytes([int]$bytes.Length)
    $pipe.Write($len, 0, 4)
    $pipe.Write($bytes, 0, $bytes.Length)
    $pipe.Flush()
}

function Read-Exact($pipe, [int]$n) {
    $buf = New-Object byte[] $n
    $got = 0
    while ($got -lt $n) {
        $r = $pipe.Read($buf, $got, $n - $got)
        if ($r -le 0) { throw "管道读取中断" }
        $got += $r
    }
    return $buf
}

# 读到 id 匹配的回复为止；期间到达的事件帧（无 id）会被打印出来但不阻断。
function Read-Reply($pipe, [double]$id, [int]$timeoutMs = 8000) {
    $deadline = (Get-Date).AddMilliseconds($timeoutMs)
    while ((Get-Date) -lt $deadline) {
        $lenBytes = Read-Exact $pipe 4
        $len = [BitConverter]::ToInt32($lenBytes, 0)
        $payload = Read-Exact $pipe $len
        $text = [System.Text.Encoding]::UTF8.GetString($payload)
        $obj = $text | ConvertFrom-Json
        if ($obj.id -ne $null -and [double]$obj.id -eq $id) { return $obj }
        Write-Host ("        (事件) {0}" -f $text)
    }
    throw "等待 id=$id 的回复超时"
}

$nextId = 0
function Invoke-Native($pipe, [string]$type, $payload) {
    $script:nextId++
    $msg = @{ type = $type; id = $script:nextId }
    if ($null -ne $payload) { $msg.payload = $payload }
    Send-Frame $pipe $msg
    $reply = Read-Reply $pipe $script:nextId
    $json = $reply | ConvertTo-Json -Depth 8 -Compress
    Write-Host ("  → {0}  {1}" -f $type, $json)
    return $reply
}

# ── 主流程 ───────────────────────────────────────────────────
Write-Host "=== WebView2 子进程回收探针（mode=$Mode, twoWindows=$([bool]$TwoWindows)）==="

$running = @(Get-Process translator_native -ErrorAction SilentlyContinue)
if ($running.Count -gt 0) {
    Write-Warning "已有 translator_native 在跑（PID $($running.Id -join ',')），单实例互斥体会让本次启动直接退出。"
    exit 2
}

Write-Host '--- T0：启动前 ---'
$t0 = Show-Snapshot 'T0'

$url = if ($NoServer) { 'about:blank' } else { "http://127.0.0.1:$Port/" }
$proc = Start-Process -FilePath $exe -ArgumentList @('--pipe', $pipeFull) `
    -RedirectStandardOutput $outLog -RedirectStandardError "$outLog.err" -PassThru
Write-Host "native 已启动 PID=$($proc.Id)，日志 $outLog"

Start-Sleep -Milliseconds 400
$pipe = Connect-Pipe 8000
Invoke-Native $pipe 'native.hello' @{ protocolVersion = 1 } | Out-Null

$spec = @{
    x = 120; y = 80; width = 620; height = 360
    topmost = $true; opacity = 0.85; blur = 'auto'
    title = 'leak-probe'; visible = $true; url = $url
}
Invoke-Native $pipe 'display.create' $spec | Out-Null

if ($TwoWindows) {
    Start-Sleep -Milliseconds 800
    Invoke-Native $pipe 'settings.open' @{ url = $url } | Out-Null
    Write-Host "  设置窗口已请求（第二个 controller，与悬浮窗共享同一个 Environment）"
}

Write-Host "  等待 ${SettleSeconds}s 让 WebView / 页面起来…"
Start-Sleep -Seconds $SettleSeconds

if (-not $NoServer) {
    Write-Host ("  SSE 订阅者={0}  累计接受连接={1}" -f [ProbeServer]::Subscribers, [ProbeServer]::TotalAccepted)
}

Write-Host '--- T1：窗口建好之后 ---'
$t1 = Show-Snapshot 'T1'

Write-Host "--- 走退出路径：$Mode ---"
switch ($Mode) {
    'shutdown' {
        Invoke-Native $pipe 'shutdown' @{} | Out-Null
    }
    'kill' {
        Stop-Process -Id $proc.Id -Force
        Write-Host "  已强杀 native PID=$($proc.Id)"
    }
    'disconnect' {
        Write-Host "  直接关闭管道客户端（native 会继续活着等重连）"
    }
    'reconnect' {
        try { $pipe.Dispose() } catch { }
        Write-Host '  已断开管道（模拟 Go 崩溃/退出）…'
        Start-Sleep -Seconds 3
        $pipe = Connect-Pipe 8000
        Invoke-Native $pipe 'native.hello' @{ protocolVersion = 1 } | Out-Null
        Invoke-Native $pipe 'native.replay' @{ registry = @{ display = $spec } } | Out-Null
        if ($TwoWindows) {
            Start-Sleep -Milliseconds 800
            Invoke-Native $pipe 'settings.open' @{ url = $url } | Out-Null
        }
        Start-Sleep -Seconds $SettleSeconds
        Write-Host '--- T1b：重连 + 重放之后（页面应当回来）---'
        Show-Snapshot 'T1b' | Out-Null
        if (-not $NoServer) {
            Write-Host ("  SSE 订阅者={0} 累计={1}（重连后必须达到期望值：低于它说明新页面根本没加载）" -f `
                [ProbeServer]::Subscribers, [ProbeServer]::TotalAccepted)
        }
    }
}

try { $pipe.Dispose() } catch { }

Write-Host "  等待 ${AfterSeconds}s 观察回收…"
Start-Sleep -Seconds $AfterSeconds

$alive = Get-Process -Id $proc.Id -ErrorAction SilentlyContinue
Write-Host ("  native 进程: {0}" -f $(if ($alive) { "仍在运行 PID=$($proc.Id)" } else { '已退出' }))

Write-Host '--- T2：退出之后 ---'
$t2 = Show-Snapshot 'T2'

if (-not $NoServer) {
    Write-Host ("  SSE 订阅者={0}  累计接受连接={1}（T2 时点仍 >0 = 页面还活着在订阅）" -f `
        [ProbeServer]::Subscribers, [ProbeServer]::TotalAccepted)
}

Write-Host ''
Write-Host '=== 结论 ==='
Write-Host ("  T0 本程序子进程 {0} → T1 {1} → T2 {2}" -f $t0.Ours, $t1.Ours, $t2.Ours)
if ($t2.Ours -eq 0) {
    Write-Host '  ✓ 退出后本程序的 WebView2 子进程已回收干净'
} else {
    Write-Host ("  ✗ 退出后仍有 {0} 个残留（PID {1}）" -f $t2.Ours, $t2.OursPids)
}

# 收尾：强杀模式下 native 已死，shutdown 模式下应已自退；
# disconnect / reconnect 模式下 native 还在（这正是要观察的），但探针结束时
# 要收掉它，免得留下下一轮跑不起来的单实例互斥体。
if ($Mode -eq 'disconnect' -or $Mode -eq 'reconnect') {
    Write-Host "  （$Mode 模式：现在收掉仍在运行的 native）"
    Get-Process -Id $proc.Id -ErrorAction SilentlyContinue | Stop-Process -Force
    Start-Sleep -Seconds 3
    Show-Snapshot '收尾' | Out-Null
}

# 精确归零时刻：T2 只是「退出后 N 秒时点」的快照，说不出到底多久收干净。
# 每 250ms 数一次（Get-CimInstance 单次约 0.1~0.2s，所以分辨率是 ±0.3s 上下），
# 用它把「系统自己回收」和「显式等待后回收」的耗时摆在一起比。
if ($TailPollSeconds -gt 0) {
    Write-Host ("--- 精确归零时刻（每 250ms 数一次，最多 {0}s）---" -f $TailPollSeconds)
    $sw = [System.Diagnostics.Stopwatch]::StartNew()
    $zeroAt = -1.0
    while ($sw.Elapsed.TotalSeconds -lt $TailPollSeconds) {
        if ((Get-OurWebViews).Count -eq 0) {
            $zeroAt = [math]::Round($sw.Elapsed.TotalSeconds, 2)
            break
        }
        Start-Sleep -Milliseconds 250
    }
    if ($zeroAt -ge 0) {
        Write-Host ("  本程序子进程数归零于退出后 {0} 秒（含轮询分辨率 ±0.3s）" -f $zeroAt)
    } else {
        Write-Host ("  到 {0}s 仍有残留" -f $TailPollSeconds)
    }
}

if (-not $NoServer) { [ProbeServer]::Stop() }
