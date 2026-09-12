/**
 * 主题：深色 / 浅色 / 跟随系统。
 *
 * **V2 新增功能**（V1 没有主题概念：悬浮窗写死深色、设置页另有一套 VS Code
 * 配色，见缺陷 D1）。取值与 Win11 的「个性化 → 颜色 → 选择模式」一致。
 *
 * 与 native 的配合：窗口的 Mica/Acrylic 材质有深浅之分，而 native 侧的
 * `DWMWA_USE_IMMERSIVE_DARK_MODE` 原本硬编码为深色。浅色主题下必须让
 * native 也切过去，否则会出现「浅色内容 + 深色材质」的错配。
 * 这条链路是：配置 → Go → IPC `display.create` 的 dark 字段 → native。
 *
 * 两个窗口是独立的 JS 环境，各自应用各自的主题；同步靠 SSE 的
 * config.reloaded（见 useConfig 的 watch）。
 */
import { computed, readonly, ref, watch } from 'vue'
import { useConfig } from './useConfig'

export type ThemeMode = 'dark' | 'light' | 'system'
export type ResolvedTheme = 'dark' | 'light'

/** 配置里的取值可能是任意字符串，收敛到合法集合 */
function normalizeMode(raw: unknown): ThemeMode {
  return raw === 'light' || raw === 'system' || raw === 'dark' ? raw : 'dark'
}

const media = window.matchMedia('(prefers-color-scheme: light)')

/** localStorage 键。两个窗口同源（都由 Go 提供），因此这份缓存是共享的。 */
const STORAGE_KEY = 'ets2-theme'

/** 读缓存的主题。index.html 的内联脚本用同一份值做首屏防闪烁。 */
function readCached(): ThemeMode | null {
  try {
    const v = localStorage.getItem(STORAGE_KEY)
    return v === 'light' || v === 'dark' || v === 'system' ? v : null
  } catch {
    return null // 隐私模式下 localStorage 可能抛异常
  }
}

function writeCached(m: ThemeMode): void {
  try {
    localStorage.setItem(STORAGE_KEY, m)
  } catch {
    /* 写不进去不影响本次会话 */
  }
}

const mode = ref<ThemeMode>(readCached() ?? 'dark')
const systemTheme = ref<ResolvedTheme>(media.matches ? 'light' : 'dark')

/** 实际生效的主题（把 system 解析成具体的深/浅） */
const resolved = computed<ResolvedTheme>(() =>
  mode.value === 'system' ? systemTheme.value : mode.value,
)

/** 把结果写到 <html data-theme>，CSS 变量据此切换。 */
watch(
  resolved,
  (t) => {
    document.documentElement.dataset.theme = t
  },
  { immediate: true },
)

// 记住选择：下次启动时 index.html 的内联脚本据此先行应用，避免闪一下另一套配色
watch(mode, writeCached)

// 跟随系统时，系统切换要即时反映（Win11 里切模式，应用是立刻变的）
media.addEventListener('change', (e) => {
  systemTheme.value = e.matches ? 'light' : 'dark'
})

/** 底色不透明度：由配置的 window_opacity 驱动 CSS 变量。 */
function applyWindowOpacity(v: unknown): void {
  const n = typeof v === 'number' ? v : Number(v)
  const clamped = Number.isFinite(n) ? Math.min(1, Math.max(0.2, n)) : 0.85
  document.documentElement.style.setProperty('--window-alpha', clamped.toFixed(2))
}

/**
 * 从配置同步主题。
 * 放在 watch 里而不是只在挂载时调用一次：另一个窗口改了配置会通过 SSE
 * 触发 config.reloaded → useConfig 重新拉取 → 这里自动跟上。
 */
const { config } = useConfig()

watch(
  config,
  (c) => {
    if (!c) return
    mode.value = normalizeMode(c.theme)
    applyWindowOpacity(c.window_opacity)
  },
  { immediate: true },
)

export function useTheme() {
  return {
    mode: readonly(mode),
    resolved: readonly(resolved),
  }
}

/** 供设置页预览用：立即应用但**不写配置**（保存时才会落盘）。 */
export function previewTheme(next: ThemeMode, opacity?: number): void {
  mode.value = next
  if (opacity !== undefined) applyWindowOpacity(opacity)
}
