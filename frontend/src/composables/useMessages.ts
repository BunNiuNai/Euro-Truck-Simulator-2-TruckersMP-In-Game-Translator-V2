/**
 * 消息列表状态。
 *
 * 数据来源有**两条**，而且它们是并发跑的，必须能安全汇合：
 *   1. SSE 的 message.translated 事件（每完成一条翻译推一条）；
 *   2. GET /api/messages 的最近消息快照（连接建立时、以及后端丢帧后拉一次）。
 *
 * 为什么非要第二条：SSE 是「推」的，错过就没了。页面一刷新（或 WebView 因故
 * 重载），浏览器手里连 Last-Event-ID 都没有，光靠事件流永远拿不回刷新之前的
 * 聊天记录——而 V1 的悬浮窗列表是进程内长期持有的，刷新界面这个动作在 V1 里
 * 根本不存在。快照就是为这种情况补的入口。
 *
 * 容量上限由配置的 max_messages 决定，默认与 V1 一致（V1 的 Text 组件
 * 也是只保留有限条数，否则长时间挂机后内存会一直涨）。
 */
import { readonly, ref } from 'vue'
import { api } from '../api/client'
import type { RenderedMessage } from '../api/types'
import { onResync } from './useEvents'

/** V1 config.json 的 max_messages 默认值 */
const DEFAULT_MAX = 100

const messages = ref<RenderedMessage[]>([])
let maxMessages = DEFAULT_MAX

export function useMessages() {
  return {
    messages: readonly(messages),
    max: () => maxMessages,
  }
}

/** 超出上限时从头部丢弃——V1 也是只显示最近的若干条。 */
function trimToMax(list: RenderedMessage[]): RenderedMessage[] {
  return list.length > maxMessages ? list.slice(list.length - maxMessages) : list
}

/**
 * 追加一条消息。
 *
 * 去重按 id：ingest 层的去重键是 speaker+text+timestamp，
 * 正常不会重复推送，但重连后前端可能收到与快照重叠的帧。
 */
export function appendMessage(msg: RenderedMessage): void {
  const list = messages.value
  if (msg.id && list.length > 0 && list[list.length - 1].id === msg.id) {
    return
  }

  list.push(msg)
  if (list.length > maxMessages) {
    messages.value = trimToMax(list)
  }
}

/** 清空（切换日志文件/服务器时用）。 */
export function clearMessages(): void {
  messages.value = []
}

// ── 快照补齐 ──────────────────────────────────────────────────

/**
 * 把一份快照合并进当前列表。**不覆盖**，只按 id 去重后补缺。
 *
 * 合并规则（两条路径并发跑，谁也不能覆盖谁）：
 *   · 列表为空 → 整体填充。这是「页面刚刷新」的情形，快照就是全部历史，
 *     直接铺开即可，而且快照本身是时间正序的，正好是列表需要的顺序。
 *   · 列表非空 → 逐条合并：id 已在列表里的跳过，缺的插到**它该在的位置**。
 *
 * 为什么不能只往尾部追加：SSE 补发帧与快照请求是两条并发路径，快照里会有
 * 前端还没收到的消息，而它们**未必是最新的**。典型就是收到 resync.required
 * 的场景——列表里已有 m100 和 m108，快照里 m101..m107 都在：只往尾部追加
 * 会排成 m100, m108, m101..m107，顺序全乱。所以缺的那条要插在「列表中第一条
 * 能确定比它更靠后」的消息之前，这样空洞会原地补回时间线上，已有元素的
 * 相对顺序一个都不会动。
 *
 * ⚠️ 快照的 id **不是序号**，是 ingest 层的去重键（`speaker|text|timestamp`
 * 拼出来的），只能判「是不是同一条」，不能拿来排序。所以下面用快照里的
 * **位次**（order）当锚点，而不是比较 id 的大小。
 *
 * @param preRequest 这次快照请求**发出之前**就已经在列表里的那些条目（按对象
 *   引用传进来——合并只搬引用、从不复制，所以引用比对是可靠的）。
 */
function mergeSnapshot(
  snapshot: readonly RenderedMessage[],
  preRequest: ReadonlySet<RenderedMessage>,
): void {
  if (snapshot.length === 0) {
    // 空快照什么都不做。真拿它去清列表是灾难：一次空响应当场抹掉用户
    // 正在看的全部记录，而且再也拿不回来。
    return
  }

  const current = messages.value

  if (current.length === 0) {
    messages.value = trimToMax(snapshot.slice())
    return
  }

  /** 快照里每个 id 的位次，用来当插入锚点 */
  const order = new Map<string, number>()
  snapshot.forEach((m, i) => {
    if (m.id) order.set(m.id, i)
  })

  const seen = new Set<string>()
  for (const m of current) if (m.id) seen.add(m.id)

  const merged = current.slice()
  let inserted = 0

  snapshot.forEach((msg, k) => {
    // 空 id 无法判断「是不是已经在列表里」。这种消息后端不会发（一定带
    // ingest 去重键或 sys-<纳秒>），真出现了宁可少这一条——插进去的话，
    // 之后每一轮 resync 都会再插一份，列表会一轮长一条。
    if (!msg.id || seen.has(msg.id)) return

    let at = merged.length
    for (let i = 0; i < merged.length; i++) {
      const m = merged[i]
      const pos = order.get(m.id)
      // 锚点 = 第一条能确定「比快照第 k 条更靠后」的条目，只有两种：
      //   · 它在快照里、位次 > k —— 直接可比；
      //   · 它不在快照里，但**快照请求之后**才进的列表 —— 那它比快照还新
      //     （本地插入的 (Sent)/System、以及请求在飞的时候 SSE 推过来的帧）。
      //
      // 不在快照里、请求之前就在列表里的条目**不能**当锚点：快照是请求之后
      // 才在后端拍的，那种条目只可能是「比快照窗口更老」。若把它当成更新的，
      // 断线久了（列表里全是旧消息、快照全是新的）时补回来的消息会被插到旧
      // 消息前面，再被 maxMessages 从尾部整段裁掉——用户一条新消息都看不到，
      // 而且下一轮 resync 还是同样的结果，永远回不到正确的状态。
      if ((pos !== undefined && pos > k) || (pos === undefined && !preRequest.has(m))) {
        at = i
        break
      }
    }

    merged.splice(at, 0, msg)
    seen.add(msg.id)
    inserted++
  })

  // 一条都没补上就别赋值：换掉数组引用会让整个列表重渲染，
  // 用户正在往回翻的时候白白闪一下。
  if (inserted === 0) return
  messages.value = trimToMax(merged)
}

/**
 * 拉一次消息快照并合并进列表。
 *
 * 注册在 onResync 上，因此**首次连接、每次重连、以及收到 resync.required
 * 时都会跑**。按当前的显示上限要条数：多要了也会被 appendMessage 的同一条
 * 规则裁掉，要少了则会在 max_messages 调大后显示不全。
 */
export async function resyncMessages(): Promise<void> {
  // 请求发出前先把列表原样记一份（按对象引用）。这是区分「不在快照里的条目
  // 到底是比快照旧还是比快照新」的唯一依据，理由见 mergeSnapshot。
  const preRequest = new Set(messages.value)
  try {
    const snap = await api.getMessages(maxMessages)
    mergeSnapshot(snap.messages ?? [], preRequest)
  } catch (e) {
    // 拉不到就保持原样：SSE 的增量照常往里进，比清空列表好得多
    console.warn('[messages] 拉取消息快照失败', e)
  }
}

/**
 * 插入一条「已发送」记录。
 *
 * V1 `overlay.py:957` `_insert_sent` 的等价物：speaker 固定为 `(Sent)`，
 * original 放**发出去的英文**、translated 放**用户打的中文**——与收到的消息
 * 正好相反，因为这条是我们自己发出去的，方向相反。
 *
 * 只有确认成功或超时才插：翻译无效 / 发送失败时 V1 是把文本回填到输入框，
 * 不往消息列表里塞东西（否则列表里会出现一条根本没发出去的消息）。
 */
export function appendSent(chinese: string, english: string): void {
  const now = new Date()
  const p = (n: number) => String(n).padStart(2, '0')
  appendMessage({
    id: '',
    speaker: '(Sent)',
    original: english,
    translated: chinese,
    lang: '',
    timestamp: `${p(now.getHours())}:${p(now.getMinutes())}:${p(now.getSeconds())}`,
    isSelf: true,
    isSystem: false,
    // 这条不来自 Provider，所以元信息一律留空——
    // 与其编一个假的模型名，不如让界面如实显示空白。
    provider: '',
    model: '',
    cacheState: '',
    latencyMs: 0,
    errCode: '',
  })
}

/**
 * 插入一条 System 提示消息。
 *
 * V1 `overlay.py:952` 的等价物：发送方向的翻译抛异常时，除了改提示文案，
 * 还往消息列表插一条 System 消息把原因留在时间线上——用户切走再回来时
 * 提示条早就消失了，只有列表里这条还在。
 */
export function appendSystem(text: string, detail: string): void {
  const now = new Date()
  const p = (n: number) => String(n).padStart(2, '0')
  appendMessage({
    id: '',
    speaker: 'System',
    original: detail,
    translated: text,
    lang: '',
    timestamp: `${p(now.getHours())}:${p(now.getMinutes())}:${p(now.getSeconds())}`,
    isSelf: true,
    isSystem: true,
    provider: '',
    model: '',
    cacheState: '',
    latencyMs: 0,
    errCode: '',
  })
}

/** 由配置同步上限。 */
export function setMaxMessages(n: number): void {
  if (n > 0) maxMessages = n
}

// ── 副作用：把「对齐快照」挂进 resync 流程 ────────────────────

// 注册点是**模块级**的，不是某个组件的 onMounted：
//   1. 消息列表本身就是模块级状态（不属于任何组件），对齐它的责任就该跟着
//      模块走。挂在组件上意味着「哪天换个窗口显示消息就会忘记注册」，而漏注册
//      的症状是消息静默地少了，正是最难发现的那种。
//   2. 时机安全：main.ts 里 import 的求值一定早于 startEvents()，所以首次
//      onOpen 触发 runResync 时这个处理器已经在集合里了——换成组件 onMounted
//      注册就得额外防「组件晚于首次连接挂载」。
// 不保存返回值是有意的：这是页面级订阅，页面还在它就该在。
onResync(resyncMessages)
