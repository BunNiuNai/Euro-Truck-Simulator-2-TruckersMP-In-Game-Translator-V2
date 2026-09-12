/**
 * 全应用**唯一**的 SSE 订阅点。
 *
 * 为什么必须是唯一的：每个 composable 各自 new EventSource 会开出多条长连接，
 * 后端 hub 的对端缓冲（256）各自独立，还可能看到不同的事件顺序。
 * 这里订阅一次，再分发给各状态模块。
 *
 * 重连策略：EventSource 自己会重连，本模块不重复实现。
 * 断线期间漏掉的帧由后端按 `Last-Event-ID` 补发（SSE 标准做法，见 events.go），
 * 但**补发盖不住两种情况**：页面整个重新加载（新会话没有事件号），
 * 以及订阅者缓冲溢出（帧已经被后端丢掉）。所以每次 onOpen（含首次与每次重连）
 * 都要触发一轮 resync，另外收到 resync.required 时也要立刻补一轮。
 */
import { readonly, ref, type Ref } from 'vue'
import { subscribe } from '../api/client'
import type { Envelope, ResyncRequiredPayload } from '../api/types'

type EventHandler = (env: Envelope) => void
type ResyncHandler = () => void | Promise<void>

const handlers = new Set<EventHandler>()
const resyncHandlers = new Set<ResyncHandler>()

/** SSE 连接是否处于建立状态。 */
const live = ref(false)

/** 已经建立过几次连接（含重连）。>1 说明发生过断线重连。 */
const connectCount = ref(0)

let stopFn: (() => void) | null = null

/**
 * 注册一个事件处理器，返回注销函数。
 * 在任何组件里调用都安全——处理器集合是模块级的。
 */
export function onEvent(fn: EventHandler): () => void {
  handlers.add(fn)
  return () => handlers.delete(fn)
}

/**
 * 注册一个「重新拉快照」处理器，返回注销函数。
 * 首次连接、每次重连成功，以及收到 resync.required 时都会调用。
 */
export function onResync(fn: ResyncHandler): () => void {
  resyncHandlers.add(fn)
  return () => resyncHandlers.delete(fn)
}

/** SSE 连接状态（只读）。 */
export function useConnection(): { live: Readonly<Ref<boolean>>; connectCount: Readonly<Ref<number>> } {
  return { live: readonly(live), connectCount: readonly(connectCount) }
}

async function runResync(): Promise<void> {
  for (const fn of resyncHandlers) {
    try {
      await fn()
    } catch (e) {
      // 单个模块拉取失败不该影响其它模块
      console.error('[events] resync 失败', e)
    }
  }
}

/**
 * 启动订阅。整个应用只调用一次（在 main.ts 里）。
 * 返回停止函数。
 */
export function startEvents(): () => void {
  if (stopFn) return stopFn

  stopFn = subscribe({
    onOpen: () => {
      live.value = true
      connectCount.value++
      void runResync()
    },
    onDown: () => {
      // EventSource 会自动重连，这里只反映状态，不主动干预
      live.value = false
    },
    onEvent: (env) => {
      // resync.required 是**订阅层自己的控制帧**，不是业务事件：后端腾出缓冲
      // 空位后才发现自己刚丢过帧，此时前端已经漏了内容而自己看不出来
      // （列表里凭空少几条消息，界面上没有任何痕迹）。唯一的补救是重新拉快照。
      //
      // 为什么不往下分发给 handlers：它描述的是「本次订阅」的健康状况，
      // 而订阅层该做的补救（runResync）这里已经做了；分发下去只会让每个
      // 业务模块都多一个什么都不做的 case 分支。
      if (env.type === 'resync.required') {
        const p = env.payload as Partial<ResyncRequiredPayload> | undefined
        // 读不到数字就说「未知」，不要编一个 0 冒充「一帧都没丢」
        const dropped = typeof p?.dropped === 'number' ? p.dropped : null
        // 前端没有统一的日志通道，控制台是唯一能留下痕迹的地方
        console.warn(
          `[events] SSE 缓冲溢出，后端丢弃了 ${dropped ?? '未知数量'} 帧，正在重新拉快照对齐`,
        )
        void runResync()
        return
      }

      for (const fn of handlers) {
        try {
          fn(env)
        } catch (e) {
          console.error(`[events] 处理 ${env.type} 失败`, e)
        }
      }
    },
  })

  return () => {
    stopFn?.()
    stopFn = null
    live.value = false
  }
}
