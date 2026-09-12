/**
 * 与 Go 后端通信的唯一入口（ADR-009）。
 *
 * 契约见 docs/architecture.md 第 9 节：
 *   REST  /api/*        命令与查询（配置、Provider、日志、翻译）
 *   SSE   /api/events   事件推送（ADR-016：用 SSE 而不是 WebSocket）
 *
 * **为什么全部用相对路径**：生产环境下页面与 API 都由 Go 提供，本来就同源；
 * 开发期 Vite 把 /api 反向代理到 Go（见 vite.config.ts）。
 * 硬编码 http://127.0.0.1:8791 会在开发期造成跨域、换端口时静默失效。
 *
 * **为什么是 SSE**：本前端只需要「服务端 → 前端」单向推送，命令走 REST；
 * EventSource 自带断线重连，且是纯 HTTP，不需要手写帧协议。
 *
 * ⚠️ EventSource 的重连不补发断线期间的事件；补发靠后端读 `Last-Event-ID`
 * 请求头把漏掉的帧重放一遍（events.go 的 replayAfter）。但那条路补不了
 * 两种空洞：**页面整个重新加载**（浏览器手里已经没有事件号了）与订阅者
 * 缓冲溢出（帧已被后端丢掉）。所以重连成功后各模块仍要重新拉一次快照
 * （health / config / stats / messages），客户端只把 onOpen 原样上报。
 */
import type {
  AdStatusPayload,
  ApiResponse,
  AppConfig,
  Envelope,
  Health,
  LogsPayload,
  MessagesPayload,
  ModelsPayload,
  PresetsPayload,
  Provider,
  ProviderInfo,
  RenderedMessage,
  StatsPayload,
  TestResult,
  TranslateResult,
} from './types'

/** 后端明确回报的错误（HTTP 非 2xx，或 ok=false）。 */
export class ApiError extends Error {
  readonly code: string
  readonly status: number

  constructor(code: string, message: string, status = 0) {
    super(message)
    this.name = 'ApiError'
    this.code = code
    this.status = status
  }
}

const JSON_HEADERS = { 'Content-Type': 'application/json' }

/**
 * 发一个 REST 请求并拆掉统一信封。
 *
 * 三条失败路径都要当成错误：
 *   1. 网络层失败 → fetch 抛异常
 *   2. 响应不是 JSON → 解析抛异常
 *   3. ok=false → 后端明确的业务失败
 * 只看 HTTP 状态码会漏掉第 3 条，那是难查的失效。
 */
async function rest<T>(path: string, init?: RequestInit & { timeoutMs?: number }): Promise<T> {
  const { timeoutMs, ...restInit } = init ?? {}

  let signal = restInit.signal ?? undefined
  if (timeoutMs && !signal) {
    signal = AbortSignal.timeout(timeoutMs)
  }

  let res: Response
  try {
    res = await fetch(path, { headers: JSON_HEADERS, ...restInit, signal })
  } catch (e) {
    throw new ApiError('NETWORK', e instanceof Error ? e.message : String(e))
  }

  let body: ApiResponse<T>
  try {
    body = (await res.json()) as ApiResponse<T>
  } catch {
    throw new ApiError('BAD_RESPONSE', `${path} 返回的不是 JSON（HTTP ${res.status}）`, res.status)
  }

  // 后端每个 /api/* 都必须返回统一信封。若这里缺 ok，说明要么某个端点
  // 漏包了信封，要么请求打到了别的服务上——直接读 body.error.code 会抛
  // TypeError，把「契约不一致」伪装成「前端崩了」。
  if (typeof body !== 'object' || body === null || !('ok' in body)) {
    throw new ApiError(
      'BAD_RESPONSE',
      `${path} 的响应缺少 ok 字段（后端未使用统一信封）`,
      res.status,
    )
  }

  if (!body.ok) {
    const code = body.error?.code ?? 'UNKNOWN'
    const message = body.error?.message ?? `${path} 失败（HTTP ${res.status}）`
    throw new ApiError(code, message, res.status)
  }
  return body.data
}

// ── REST ──────────────────────────────────────────────────────

/**
 * 窗口拖动/缩放的区域名。
 *
 * 前端只说语义，不碰 HTBOTTOMRIGHT 这类 Win32 命中码——让界面层知道
 * Windows 的内部常量是把平台细节漏到了错误的层。
 */
export type WindowZone =
  | 'caption'
  | 'left'
  | 'right'
  | 'top'
  | 'bottom'
  | 'topleft'
  | 'topright'
  | 'bottomleft'
  | 'bottomright'

export const api = {
  health: () => rest<Health>('/api/health'),

  getConfig: () => rest<AppConfig>('/api/config'),

  /** 保存配置。后端保存后会广播 config.reloaded。 */
  putConfig: (cfg: AppConfig) =>
    rest<{ saved: boolean }>('/api/config', {
      method: 'PUT',
      body: JSON.stringify(cfg),
      timeoutMs: 10_000,
    }),

  /** Provider 列表。**不含 api_key**——需要密钥的地方读 GET /api/config。 */
  getProviders: () => rest<ProviderInfo[]>('/api/providers'),

  /** 连通性测试。传完整 Provider（含密钥），后端只用来发一次请求，不落盘。 */
  testProvider: (p: Provider) =>
    rest<TestResult>('/api/providers/test', {
      method: 'POST',
      body: JSON.stringify(p),
      timeoutMs: 30_000,
    }),

  /**
   * 拉取某个 Provider 的可用模型列表（设置页「Model / 模型」右侧的 📥 按钮）。
   *
   * 请求体与 testProvider **完全一样**：就是把 Provider 配置对象本身发过去。
   * 这不是巧合——两个端点要的都是"用户此刻在界面上看到的那个 Provider"，
   * 而不是再去 GET /api/config 拉一份（用户可能刚改完、还没保存）。
   *
   * 密钥就是真密钥：`api_key` 早已不再走占位符（哨兵机制已整套删除，
   * 见 backend/internal/api/api.go 的 handleConfig 说明），后端拿到的就是
   * 内存里的真值。这里**不要**做任何密钥特殊处理。
   *
   * ⚠️ timeoutMs 必须**大于**后端的 15 秒。反过来（前端先超时）会得到最恼人
   * 的结果：界面报「超时」，而后端那次请求其实还在跑、甚至马上就成功了。
   * 用户看到的是假失败，重试一次又是 15 秒。给 20 秒留出余量。
   *
   * ⚠️ 返回 `success:false` 不是异常，见 ModelsPayload 的注释。
   */
  fetchModels: (p: Provider) =>
    rest<ModelsPayload>('/api/providers/models', {
      method: 'POST',
      body: JSON.stringify(p),
      timeoutMs: 20_000,
    }),

  getPresets: () => rest<PresetsPayload>('/api/presets'),

  getLogs: (n = 200) => rest<LogsPayload>(`/api/logs?n=${n}`),

  deleteLogs: () => rest<{ deleted: number; errors: string[] | null }>('/api/logs', { method: 'DELETE' }),

  /** 在系统文件管理器里打开日志目录（对应 V1 的「📂 打开日志文件夹」） */
  revealLogs: () => rest<{ dir: string }>('/api/logs/reveal', { method: 'POST', timeoutMs: 10_000 }),

  /**
   * 优雅退出。
   *
   * 对应 V1 悬浮窗右键菜单的 Exit：`overlay.py:456` 的 `_on_exit` 接到
   * `main.py:214` 的 `_shutdown`——保存位置与配置、停托盘、停后台线程、销毁窗口。
   * 与托盘菜单的 Quit 是同一条路径。
   */
  quit: () => rest<{ quitting: boolean }>('/api/quit', { method: 'POST', timeoutMs: 5000 }),

  /**
   * 把文本写进系统剪贴板（对应 V1 的「复制热键」）。
   *
   * 不用 navigator.clipboard：WebView2 里那个 API 需要用户手势或授权，
   * 而热键触发时并没有手势。走 Go → native 的 Win32 剪贴板最可靠。
   */
  copyToClipboard: (text: string) =>
    rest<{ copied: boolean; length: number }>('/api/clipboard', {
      method: 'POST',
      body: JSON.stringify({ text }),
      timeoutMs: 10_000,
    }),

  getStats: () => rest<StatsPayload>('/api/stats'),

  /**
   * 最近消息快照（**时间正序**，老的在前）。
   *
   * 为什么必须有这个端点：消息列表在 JS 内存里，页面一刷新（或 WebView
   * 因故重载）就清空了——而 V1 的悬浮窗列表是进程内长期持有的，刷新界面
   * 这个动作在 V1 里根本不存在。SSE 补发救不了这种「新会话」：
   * 浏览器手里已经没有 Last-Event-ID 了。
   *
   * limit 省略时后端返回 100 条；它保留的历史有上限（环形缓冲 200 条），
   * 要多了也只会拿到缓冲里现有的那些。
   */
  getMessages: (limit?: number) =>
    rest<MessagesPayload>(`/api/messages${limit && limit > 0 ? `?limit=${limit}` : ''}`),

  /**
   * 开始一次窗口拖动或缩放（标题带 / 四边）。
   *
   * ⚠️ **发完就放手**，不要 await 它再去更新界面：这个调用只表示「已经开始
   * 拖」，真正的位移在 native 主循环里逐帧推进，直到用户松开鼠标为止。
   * 等它返回再渲染会让拖动明显发滞。
   *
   * 为什么不用 CSS `app-region: drag`（那条路本来最省事）：
   * 实测开启 ICoreWebView2Settings9::IsNonClientRegionSupportEnabled 之后，
   * 真实鼠标拖动产生的窗口位移恒为 (0,0)；而且它**只能拖、不能缩放**。
   * 详见 native/src/webview.cpp 里的调查记录。
   */
  beginWindowMove: (zone: WindowZone) =>
    rest<{ zone: string }>('/api/window/move', {
      method: 'POST',
      body: JSON.stringify({ zone }),
      timeoutMs: 5000,
    }),

  translate: (text: string, mode: 'receive' | 'send' = 'receive') =>
    rest<TranslateResult>('/api/translate', {
      method: 'POST',
      body: JSON.stringify({ text, mode }),
      timeoutMs: 60_000,
    }),

  /**
   * 手动发送：中文 → 译文 → 发进游戏 → 聊天日志确认。
   *
   * ⚠️ 这个请求会**阻塞好几秒**（250ms 藏窗 + 约 1.5s 模拟按键 + 最多 2.5s
   * 确认），期间悬浮窗会被藏起来。返回的 result 决定提示文案与是否插入记录：
   * OK_CONFIRMED / OK_UNCONFIRMED / FAIL_SEND / FAIL_TRANSLATION / BUSY。
   *
   * 与 sendAd 的区别：这条会**翻译**（收发的是中文），sendAd 是把文本原样发出。
   */
  sendManual: (text: string) =>
    rest<{ result: string; chinese: string; english: string; message?: string }>(
      '/api/compose/send',
      {
        method: 'POST',
        body: JSON.stringify({ text }),
        timeoutMs: 60_000,
      },
    ),

  /**
   * 发送一条消息到游戏（**不翻译**）。
   * 后端会隐藏悬浮窗、模拟按键，并还原剪贴板。
   *
   * ⚠️ 这是「立刻发一条」的底层能力。广告循环**不走这里**——它由 Go 的
   * `internal/task.AdSender` 自己驱动（见下面 ad* 三个方法），
   * 前端不再持有定时器。
   */
  sendAd: (text: string) =>
    rest<{ sent: boolean }>('/api/send', {
      method: 'POST',
      body: JSON.stringify({ text }),
      timeoutMs: 30_000,
    }),

  // ── 广告发送（状态机在 Go 侧）────────────────────────────────
  //
  // V1 把状态机放在设置窗口的 tkinter `after()` 里，关掉窗口发送就停了，
  // 所以它自己在界面上写红字警告「请不要关闭设置界面」（缺陷 D17）。
  // V2 把状态机搬到 Go 侧 internal/task.AdSender：
  // 这三个端点只是遥控器，关掉设置窗口发送照常继续。

  /** 取一次状态快照（首屏与 SSE 重连后对齐用）。 */
  adStatus: () => rest<AdStatusPayload>('/api/ad/status'),

  /**
   * 开始循环发送。
   *
   * messages / countdown 一起带上，让 Go 用**屏幕上这份**内容而不是已保存的
   * 配置——V1 的 `_ad_start` 直接读 Entry 控件，改完不点保存就点开始也能生效。
   * 不传则回落到已保存的配置。
   */
  adStart: (messages?: string[], countdown?: string) =>
    rest<AdStatusPayload>('/api/ad/start', {
      method: 'POST',
      body: JSON.stringify({ messages, countdown }),
      timeoutMs: 15_000,
    }),

  /** 停止循环发送（幂等）。 */
  adStop: () =>
    rest<AdStatusPayload>('/api/ad/stop', {
      method: 'POST',
      timeoutMs: 15_000,
    }),
}

// ── SSE ───────────────────────────────────────────────────────

export interface SubscriptionHandlers {
  /** 收到一帧事件。解析失败的帧会被丢弃，不会调用这里。 */
  onEvent: (env: Envelope) => void
  /** 连接建立（含 EventSource 的自动重连成功）。 */
  onOpen?: () => void
  /** 连接断开。EventSource 会自动重连，这里只用于更新 UI 状态。 */
  onDown?: () => void
}

/**
 * 订阅 SSE 事件流，返回取消订阅函数。
 *
 * 重连由 EventSource 负责，本函数不重复实现——重复的重连逻辑只会互相打架。
 */
export function subscribe(handlers: SubscriptionHandlers): () => void {
  const es = new EventSource('/api/events')

  es.onopen = () => handlers.onOpen?.()
  es.onerror = () => handlers.onDown?.()

  es.onmessage = (ev: MessageEvent<string>) => {
    let env: Envelope
    try {
      env = JSON.parse(ev.data) as Envelope
    } catch {
      // 单帧坏了不该拖垮整个订阅
      return
    }
    handlers.onEvent(env)
  }

  return () => es.close()
}

/** 便捷类型守卫：把 payload 断言成具体事件载荷。 */
export function payloadAs<T>(env: Envelope): T {
  return env.payload as T
}

/** message.translated 事件的载荷就是 RenderedMessage。 */
export function isTranslated(env: Envelope): env is Envelope<RenderedMessage> {
  return env.type === 'message.translated'
}
