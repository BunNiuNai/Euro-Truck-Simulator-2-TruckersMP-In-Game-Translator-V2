/**
 * 配置状态（设置页与悬浮窗共用）。
 *
 * 唯一事实源在 Go：本地只保留一份用于渲染的副本，保存时整体回传。
 *
 * 「脏字段」为什么要按**字段粒度**记：设置窗口与悬浮窗是两个独立窗口、
 * 两个独立 JS 环境，任一窗口保存配置，Go 都广播 config.reloaded 让另一处重拉。
 * 用户在设置窗口改了一半、还没点保存时收到这个事件，若整体覆盖，改动就没了
 * 而且没有任何提示。所以重拉时只让**没被碰过**的字段跟随后端，碰过的保持
 * 用户输入。只记一个「脏/不脏」的布尔是不够的——那不知道哪些字段该保留。
 *
 * 与 V1 的对应：V1 只有 SettingsDialog 一个设置界面、改完即写配置，不存在
 * 「另一处把配置改了」这件事，因此这条合并规则是 V2 双窗口架构（ADR-013）
 * 带来的新问题，V1 无可对照的实现。
 */
import { computed, readonly, ref } from 'vue'
import { api } from '../api/client'
import type { AppConfig } from '../api/types'

const config = ref<AppConfig | null>(null)
const loading = ref(false)
const saving = ref(false)

/**
 * 用户改过、但还没保存的**顶层字段名**。
 *
 * 记录点只有 patchConfig 一处：设置页四个 tab 的全部改动都从那里过，
 * 记在那一处就不必让每个输入控件自己上报字段名——漏一个就是一次静默丢失。
 */
const dirtyKeys = ref<Set<keyof AppConfig>>(new Set())

/** 把若干字段标成「用户改过」。 */
function markDirty(keys: Array<keyof AppConfig>): void {
  // 每次**换一个新 Set** 而不是就地 add：就地改依赖 Vue 对集合的埋点，
  // 换引用则是任何读取路径（含 readonly 包装与 computed）都不会漏通知的写法。
  // 字段总共二十来个，重建的代价可以忽略。
  const next = new Set(dirtyKeys.value)
  for (const k of keys) next.add(k)
  dirtyKeys.value = next
}

/**
 * 清空脏标记。
 *
 * 两条路都要调：保存成功（改动已经落到 Go 上），以及用户明确放弃改动
 * （Cancel / 重新加载）。⚠️ 放弃改动时必须**先清再拉取**，否则 loadConfig
 * 会把用户那些未保存的值又合并回来，「取消」就等于什么都没做。
 */
export function clearDirty(): void {
  dirtyKeys.value = new Set()
}

export function useConfig() {
  return {
    config: readonly(config),
    loading: readonly(loading),
    saving: readonly(saving),
    /** 是否有未保存的改动。设置窗口底部据此显示提示、决定 Save/Cancel 是否可点。 */
    dirty: computed(() => dirtyKeys.value.size > 0),
    /** 悬浮窗需要的几个展示开关，缺配置时给出与 V1 一致的默认值 */
    display: computed(() => ({
      fontSize: config.value?.font_size ?? 12,
      showLanguage: config.value?.show_language_label ?? false,
      showOriginal: config.value?.show_original_text ?? true,
      sendHotkey: config.value?.send_hotkey ?? 'shift+y',
      maxMessages: config.value?.max_messages ?? 100,
      opacity: config.value?.window_opacity ?? 0.8,
    })),
  }
}

/**
 * 从 Go 重新拉一份配置，**保留**本窗口未保存的改动。
 *
 * ⚠️ 拉回来的不是「后端那份」，而是与用户未保存改动合并后的结果，见 mergeDirty。
 * 之所以把合并放在这里而不是各个调用点：调用 loadConfig 的地方有三处
 * （config.reloaded 事件、SSE 重连后的 resync、首屏），任何一处漏了合并
 * 都会重新变成「静默吃掉用户改动」。
 */
export function loadConfig(): Promise<void> {
  return fetchConfig(false)
}

/**
 * 从 Go 重新拉一份配置，并**丢弃**本窗口未保存的改动。
 *
 * 只有用户明确说「我不要这些改动了」才走这里：设置窗口的 Cancel / 重新加载。
 */
export function reloadConfig(): Promise<void> {
  return fetchConfig(true)
}

async function fetchConfig(discardDirty: boolean): Promise<void> {
  loading.value = true
  try {
    const fresh = await api.getConfig()
    // ⚠️ 清脏标记必须发生在**拉取成功之后**。先清再拉的话，一旦这次请求
    //    失败（后端没起来、超时），界面上的输入还是用户改过的值，脏标记却
    //    已经没了——Save 按钮变灰，用户连补救都做不到。
    if (discardDirty) {
      config.value = fresh
      clearDirty()
    } else {
      config.value = mergeDirty(fresh)
    }
  } finally {
    loading.value = false
  }
}

export async function saveConfig(next: AppConfig): Promise<void> {
  saving.value = true
  try {
    await api.putConfig(next)
    config.value = next
    // 改动已经落到 Go 上，这些字段不再是「未保存的改动」。收尾放在这里而不是
    // 调用方，是为了让「保存成功」只有一处负责清标记，换调用方也不会漏清。
    clearDirty()
  } finally {
    saving.value = false
  }
}

/** 就地更新一个字段（设置页表单双向绑定用）。 */
export function patchConfig(patch: Partial<AppConfig>): void {
  if (!config.value) return
  config.value = { ...config.value, ...patch }
  // 顺手记下被改过的字段名——这就是「脏字段」的唯一登记点。
  // patch 的键在运行时就是字段名，即使类型层面被 `as never` 放宽过也一样。
  markDirty(Object.keys(patch) as Array<keyof AppConfig>)
}

/**
 * 把后端刚拉到的配置与「用户改过但没保存」的字段合并。
 *
 *   · 没碰过的字段 → 用后端的（否则「在别处改了配置」在这个窗口里永远看不到）
 *   · 碰过的字段   → 用用户输入的值（否则未保存的改动被静默吃掉）
 *   · 碰过又改回原值的字段 → 结构相等即视为没改，交还给后端，
 *     免得底部的「有未保存的改动」一直挂着、Save 一直亮着
 */
function mergeDirty(fresh: AppConfig): AppConfig {
  const local = config.value
  // 悬浮窗那边从不调 patchConfig，脏集合恒为空，走的也是这一支——
  // 两个窗口共用同一个 composable，但各自是独立的 JS 环境，互不影响。
  if (!local || dirtyKeys.value.size === 0) return fresh

  const kept = new Set<keyof AppConfig>()
  const merged: AppConfig = { ...fresh }
  for (const key of dirtyKeys.value) {
    if (deepEqual(local[key], fresh[key])) continue
    kept.add(key)
    takeField(merged, local, key)
  }
  dirtyKeys.value = kept
  return merged
}

/**
 * 逐字段赋值。
 *
 * 不能直接写 `target[key] = source[key]`：key 的类型是 `keyof AppConfig` 这个
 * **联合**，TS 会把它当成「同时是 string 又是 number…」，报「不能赋给 never」。
 * 把 key 提成泛型参数 K，每个调用点上 `AppConfig[K]` 就是同一个具体类型。
 */
function takeField<K extends keyof AppConfig>(target: AppConfig, source: AppConfig, key: K): void {
  target[key] = source[key]
}

/**
 * 结构相等比较。
 *
 * 为什么不能直接用 `!==`/`===`：llm_providers / ad_messages / extra_body 这些
 * 每次拉取回来都是**新对象**，引用比较必然报「不同」，于是「用户改完又改回原值」
 * 会被一直判成脏字段。所以必须比较值本身，而不是引用。
 */
function deepEqual(a: unknown, b: unknown): boolean {
  if (a === b) return true

  // NaN 在配置里不会出现（都是 JSON 值），但 === 对它失效，顺手挡一下不吃亏
  if (typeof a === 'number' && typeof b === 'number') {
    return Number.isNaN(a) && Number.isNaN(b)
  }

  if (typeof a !== 'object' || typeof b !== 'object' || a === null || b === null) return false
  // 数组与对象不可混同：[] 与 {} 键数都是 0，不判这一条会被当成相等
  if (Array.isArray(a) !== Array.isArray(b)) return false

  if (Array.isArray(a) && Array.isArray(b)) {
    if (a.length !== b.length) return false
    return a.every((v, i) => deepEqual(v, b[i]))
  }

  const ra = a as Record<string, unknown>
  const rb = b as Record<string, unknown>
  const ka = Object.keys(ra)
  const kb = Object.keys(rb)
  if (ka.length !== kb.length) return false
  return ka.every((k) => Object.prototype.hasOwnProperty.call(rb, k) && deepEqual(ra[k], rb[k]))
}

/** 配置对象本体（保存时用）。 */
export function currentConfig(): AppConfig | null {
  return config.value
}
