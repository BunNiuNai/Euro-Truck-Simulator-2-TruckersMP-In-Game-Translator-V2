/**
 * 前端与 Go 后端的数据契约。
 *
 * 每一节都标注了对应的 Go 类型；**改一边必须同时改另一边**。
 * 契约的真实来源是 Go 侧——前端不做任何本地持久化，唯一事实源在 Go（ADR-009）。
 *
 * 注意 domain 里的 Go 结构体必须带 json tag，否则 encoding/json 会输出
 * Go 字段名（"Speaker"），前端按 camelCase 读就是 undefined。
 * 这个坑真实踩过一次，见 domain/types.go 的注释。
 */

// ── 信封 ──────────────────────────────────────────────────────

/** backend/internal/api/api.go: apiResponse */
export type ApiResponse<T> =
  | { ok: true; data: T }
  | { ok: false; error: { code: string; message: string } }

/** backend/internal/api/events.go: event —— SSE 每帧的 `data:` 内容 */
export interface Envelope<T = unknown> {
  type: EventType | string
  payload: T
}

// ── 领域对象 ──────────────────────────────────────────────────

/** backend/internal/domain/types.go: RenderedMessage */
export interface RenderedMessage {
  id: string
  speaker: string
  original: string
  translated: string
  lang: string
  timestamp: string
  isSelf: boolean
  isSystem: boolean
  provider: string
  model: string
  cacheState: string
  latencyMs: number
  errCode: string
}

/** backend/internal/domain/types.go: Stats —— 内部的三个计数 */
export interface Stats {
  translated: number
  cached: number
  selfSkipped: number
}

/**
 * GET /api/stats 与 stats.updated 事件的载荷。
 *
 * total 与 savingsPct **由 Go 算好返回**，前端不要自己再算一遍：
 * 它们是 V1 的口径（分子只含 cached 与 selfSkipped，且用截断而非四舍五入），
 * 两处各算一套迟早会算出不一致的结果。
 */
export interface StatsPayload extends Stats {
  targetLang: string
  total: number
  savingsPct: string
}

/** 统计条的初始值 */
export function emptyStats(targetLang = ''): StatsPayload {
  return { targetLang, translated: 0, cached: 0, selfSkipped: 0, total: 0, savingsPct: '0%' }
}

// ── 配置 ──────────────────────────────────────────────────────

/** backend/internal/config/config.go: Provider */
export interface Provider {
  id: string
  label: string
  endpoint: string
  api_key: string
  model: string
  enabled: boolean
  preset_id: string
  icon: string
  api_format: string
  weight: number
  extra_headers: Record<string, string>
  extra_body: Record<string, unknown>
  timeout: number
}

/** backend/internal/config/config.go: Config */
export interface AppConfig {
  target_language: string
  send_target_language: string

  window_opacity: number
  font_size: number
  max_messages: number

  player_name: string

  window_mode: string
  click_through: boolean
  win_x: number
  win_y: number
  win_w: number
  win_h: number

  settings_win_w: number
  settings_win_h: number

  chat_hotkey: string
  copy_hotkey: string
  paste_hotkey: string
  enter_hotkey: string
  send_hotkey: string

  debug_log: boolean
  show_language_label: boolean
  show_original_text: boolean
  accent_color: string

  /**
   * 界面主题："dark" | "light" | "system"。
   * **V2 新增字段**（V1 没有主题概念，见迁移对照表 D1）。
   */
  theme: string

  ad_messages: string[]
  ad_countdown: string

  llm_providers: Provider[]
}

// ── 接口响应 ──────────────────────────────────────────────────

/** backend/internal/api/api.go: handleHealth */
export interface Health {
  configPath: string
  configDir: string
  logDir: string
  targetLang: string
  providers: { total: number; enabled: number }
  nativeAlive: boolean
  sseClients: number
  /**
   * 程序版本号（如 "v2.0.0"）。悬浮窗 header 左上角显示的就是它。
   *
   * 可选：字段是新加的，老后端不返回。**不要**改成必填——那会让前端在
   * 版本号缺失时把整个 /api/health 判成坏响应，连 nativeAlive 都读不到。
   * 拿不到时由前端回退到常量（见 OverlayApp 的 VERSION_FALLBACK）。
   */
  version?: string
  providerHealth?: ProviderHealth[]
}

/** internal/providers 的 Health() 返回项 */
export interface ProviderHealth {
  id: string
  failures: number
  cooling: boolean
}

/** backend/internal/api/api.go: handleProviders 的 item（**不含 api_key**） */
export interface ProviderInfo {
  index: number
  id: string
  label: string
  endpoint: string
  model: string
  enabled: boolean
  apiFormat: string
  weight: number
  /** 是否已配置密钥——密钥本身不在这份列表里，见 GET /api/config */
  hasKey: boolean
}

/** backend/internal/api/api.go: handleProviderTest */
export interface TestResult {
  success: boolean
  message: string
}

/**
 * backend/internal/api/api.go: handleProviderModels —— 设置页 📥 按钮的响应。
 *
 * ⚠️ 两条容易踩的坑：
 *
 * 1. **判断成败只能看 `success`，不能看 catch。** 拉取失败（endpoint 或密钥
 *    填错）不是 HTTP 错误，后端用 200 + `success:false` 回报。只靠 try/catch
 *    会把每一次失败都当成成功，然后拿一个空列表去渲染。
 *
 * 2. **`error` 是 Go 的原始错误串，面向开发者，不是中文提示。**
 *    实测样本：
 *      `Get "https://x.invalid/v1/models": dial tcp: lookup x.invalid: no such host`
 *      `Get "/v1/models": unsupported protocol scheme ""`
 *    对比同一个 Provider 发给 `/api/providers/test`，那边给的是 V1 格式的中文
 *    （`[网络错误] 无法连接到 API 服务器，请检查地址和网络`）——**两个端点的
 *    错误口径不一样**，别把这里的 `error` 直接糊到界面上当提示语。
 *    它有两个用处：排查时的技术细节，以及「后端确实失败了」这个事实本身。
 */
export interface ModelsPayload {
  success: boolean
  /**
   * 可用模型名。后端保证是**数组**（nil 已被规整成 `[]`，否则序列化成 null，
   * 前端 `for...of` 会直接抛），但**可以是空的**——空表示没有拿到列表，
   * 不是「加载中」，不能渲染成一片空白。
   */
  models: string[]
  /** Go 的原始错误串（英文/技术措辞），用户在界面上看到的应是前端自己写的中文概括 */
  error: string
  /** 这次请求的耗时（毫秒）。Go 侧是 float64，可能是小数。 */
  latencyMs: number
}

/** backend/internal/api/api.go: handleLogs */
export interface LogsPayload {
  lines: string[]
  dir: string
  files: string[]
}

/** backend/internal/api/messages.go: handleMessages */
export interface MessagesPayload {
  /**
   * 时间**正序**（老的在前）。
   *
   * 顺序不能想当然：前端是往列表尾部追加的，后端要是倒着发，
   * 整段历史会在界面上从新到旧地「倒着长出来」。
   */
  messages: RenderedMessage[]
  count: number
}

/** backend/internal/api/api.go: handleTranslate */
export interface TranslateResult {
  id: string
  translation: string
  provider: string
  model: string
  cache: string
  detected: string
}

// ── 资源文件 ──────────────────────────────────────────────────

/** resources/providers.json: presets[] */
export interface Preset {
  id: string
  name: string
  websiteUrl: string
  apiKeyUrl: string
  endpoint: string
  apiFormat: string
  defaultModel: string
  modelsUrl: string
  icon: string
  iconColor: string
  category: string
  templateHeaders: Record<string, string>
  templateBody: Record<string, unknown>
  description: string
  recommended: boolean
}

/** resources/providers.json: categories[] */
export interface Category {
  id: string
  label: string
}

/** GET /api/presets 直接返回 providers.json 的内容 */
export interface PresetsPayload {
  schemaVersion?: number
  categories: Category[]
  icons: Record<string, string>
  presets: Preset[]
}

// ── SSE 事件 ──────────────────────────────────────────────────

/** 架构文档第 9.2 节定义的事件类型 */
export type EventType =
  | 'hello'
  | 'message.translated'
  | 'message.sent'
  | 'stats.updated'
  | 'provider.health'
  | 'log.appended'
  | 'source.status'
  | 'config.reloaded'
  | 'native.status'
  | 'notice'
  // Go 的广告状态机（internal/task.AdSender）会把状态与日志推上来。
  // 之前这三个事件后端一直在发、前端却没有订阅者，「广告状态机在 Go 侧」
  // 只停留在注释里：真正的循环其实跑在 AdTab.vue 的 setTimeout 上。
  | 'ad.status'
  | 'ad.log'
  // 全局热键被按下（呼出输入栏 / 发送），由前端决定具体动作。
  | 'hotkey'
  // 后端因为本订阅者的缓冲（256 帧）满了、不得不丢帧时，补发的一帧通知。
  // **收到它就意味着前端已经漏了内容而且自己看不出来**（消息列表会凭空
  // 少几条且没有任何提示），所以必须主动重新对齐快照。
  | 'resync.required'

export interface NoticePayload {
  text: string
  level: string
  durationMs?: number
}

/** resync.required 的载荷：这次一共丢了（补不回来的）多少帧。 */
export interface ResyncRequiredPayload {
  dropped: number
}

export interface SourceStatusPayload {
  server: string
}

export interface NativeStatusPayload {
  alive: boolean
  capabilities: string[]
  degraded: string[]
}

/**
 * 广告发送状态快照。
 *
 * 字段与 Go 的 `internal/task.Status` 一一对应——状态机在 Go 侧，
 * 前端只负责显示，不再自己算倒计时。
 */
export interface AdStatusPayload {
  running: boolean
  /** 当前发送到第几条，从 0 开始；-1 表示未启动 */
  currentIndex: number
  /** 距下次发送剩余秒数 */
  remainingSec: number
  /** 已生效的间隔（分钟） */
  intervalMin: number
  /** 消息槽总数 */
  total: number
  /** 已成功发送次数 */
  sent: number
  /** 失败次数 */
  failed: number
  lastError?: string
}

export interface AdLogPayload {
  level: string
  text: string
}
