<script setup lang="ts">
/**
 * 悬浮窗根组件 —— V1 overlay.py 的 7 段布局。
 *
 * V1 overlay.py:112-167 的网格（行号即顺序）：
 *   0  2px 主色蓝高亮条
 *   1  header（版本 · 服务器名 | 时钟）      ← 原生拖拽区
 *   2  分隔线
 *   3  消息区（滚动，占满剩余空间）
 *   4  输入栏（常驻）
 *   5  分隔线
 *   6  统计条
 *
 * 外层还有一圈 1px 描边线蓝（V1 用 padx=1/pady=1 的 Frame 实现）。
 *
 * 拖动与边缘缩放：前端只判断「按在哪个区域」（标题带 → caption，最外圈 →
 * 对应的边/角），区域名经 Go 转给 native，由 native 主循环逐帧推进窗口几何。
 * 完整理由见 composables/useWindowDrag.ts —— 简单说，WebView2 铺满客户区后
 * 宿主窗口在四边收不到 WM_NCHITTEST，而系统的模态移动循环又会阻塞 native
 * 主线程、拖垮管道心跳。
 *
 * V1 的等价实现是 tkinter 自己在 <Button-1>/<B1-Motion> 里算几何
 * （overlay.py:466-533，约 70 行）；V2 把「算几何」那半搬到了 native。
 */
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { onEvent, onResync } from '../composables/useEvents'
import { appendMessage, clearMessages, setMaxMessages, useMessages } from '../composables/useMessages'
import { applyStats, refreshStats, useStats } from '../composables/useStats'
import { refreshHealth, setServerName, useHealth } from '../composables/useHealth'
import { loadConfig, useConfig } from '../composables/useConfig'
import { showNotice, useNotice } from '../composables/useNotice'
import { RESIZE_BORDER, RESIZE_CORNER, useWindowDrag } from '../composables/useWindowDrag'
import { api } from '../api/client'
import type { Envelope, RenderedMessage, SourceStatusPayload, StatsPayload } from '../api/types'
import OverlayHeader from './OverlayHeader.vue'
import MessageList from './MessageList.vue'
import InputBar from './InputBar.vue'
import StatsBar from './StatsBar.vue'
import ContextMenu from './ContextMenu.vue'

/**
 * 版本号的兜底值。
 *
 * 真正的版本号由 Go 从 /api/health 的 `data.version` 给出（编译期注入，
 * 改版本只改一处）。这里只是**取不到时**的退路：请求还没回来、或者对着
 * 一个还没带 version 字段的旧后端跑。硬编码一个跟 Go 走散的版本号，
 * 会让用户在排查问题时照着界面上那个错误版本去比日志。
 */
const VERSION_FALLBACK = 'v2.0.0-dev'

const { messages } = useMessages()
const { stats } = useStats()
const { health, serverName } = useHealth()
const { display } = useConfig()
const { notice } = useNotice()

/**
 * header 左上角显示的版本号。
 *
 * 对应 V1 `overlay.py:191-195` 的 `version_label`（那里是 `VERSION` 常量）。
 * V2 改为读 /api/health，因为版本号在 Go 侧已经有一份权威来源；
 * `health` 初值为 null，所以首帧会短暂显示兜底值，resync 回来后即被替换。
 */
const version = computed(() => health.value?.version || VERSION_FALLBACK)

const listRef = ref<InstanceType<typeof MessageList> | null>(null)
const inputRef = ref<InstanceType<typeof InputBar> | null>(null)

// ── 拖动 / 边缘缩放 ──
// 前端只负责判断「按在哪个区域」，位移由 native 主循环推进。
// 完整理由见 composables/useWindowDrag.ts。
const { start: startDrag } = useWindowDrag()

// ── 右键菜单 ──
const menu = ref<{ visible: boolean; x: number; y: number }>({ visible: false, x: 0, y: 0 })

function openMenu(e: MouseEvent): void {
  // 必须阻止浏览器默认菜单，否则两个菜单会叠在一起
  e.preventDefault()
  menu.value = { visible: true, x: e.clientX, y: e.clientY }
}

function closeMenu(): void {
  menu.value.visible = false
}

function onMenuSettings(): void {
  closeMenu()
  // window.open 是 WebView2 的标准入口：native 拦截 NewWindowRequested 后
  // 用**共享的 WebView2 Environment** 创建第二个窗口（ADR-013 的两窗口方案）。
  // 开发期在浏览器里就是开一个新标签页，可以直接调试设置页。
  const w = window.open('?page=settings', 'ets2-settings', 'width=980,height=720')
  if (!w) {
    // 走到这里说明 native 侧创建设置窗口失败了（正常情况下它会真的开出来）。
    // ⚠️ 这里以前写的是「浏览器拦截了新窗口，请允许弹出窗口」——**那是错的**，
    //    WebView2 里根本没有「允许弹出窗口」这种设置可改，用户照着做只会白费功夫。
    //    提示必须说真话并指出下一步去哪看。
    showNotice('无法打开设置窗口，详情见程序日志', 'error', 5000)
  }
}

function onMenuExit(): void {
  closeMenu()
  // 与 V1 一致：overlay.py:456 的 _on_exit → main.py:214 的 _shutdown，
  // 真退出（保存位置与配置、停托盘与后台线程、销毁窗口）。
  // 托盘菜单的 Quit 走的是同一条后端路径。
  void api
    .quit()
    .catch((e: unknown) =>
      showNotice(`退出失败：${e instanceof Error ? e.message : String(e)}`, 'error', 4000),
    )
}

// ── 事件订阅 ──
const disposers: Array<() => void> = []

function handleEvent(env: Envelope): void {
  switch (env.type) {
    case 'message.translated':
      appendMessage(env.payload as RenderedMessage)
      break
    case 'stats.updated':
      applyStats(env.payload as Partial<StatsPayload>)
      break
    case 'source.status':
      setServerName((env.payload as SourceStatusPayload).server)
      break
    case 'hotkey':
      onHotkey((env.payload as { id: string }).id)
      break
    case 'notice': {
      const p = env.payload as { text: string; level: string }
      showNotice(p.text, p.level)
      break
    }
    case 'config.reloaded':
      // 字号/显示开关可能刚被改过，重新拉一份
      void loadConfig().then(() => setMaxMessages(display.value.maxMessages))
      break
    default:
      // 其它事件（provider.health / log.appended / ad.* …）悬浮窗不关心
      break
  }
}

/**
 * 响应全局热键。
 *
 * 热键本身由 native 层检测（RegisterHotKey 为主、轮询降级，ADR-015），
 * 经 Go 转成 SSE 事件送过来。**动作在这里执行**——因为「当前译文是什么」
 * 「输入栏里有什么」只有掌握界面的这一侧知道，Go 那边看不到。
 *
 * 三个 id 与 V1 `_build_hotkeys_tab` 的三个热键一一对应：
 *   copy  → copy_hotkey   复制当前译文
 *   send  → enter_hotkey  发送输入栏内容
 *   focus → send_hotkey   呼出输入栏
 */
/**
 * 让输入栏获得焦点，并在 50ms / 150ms 后再各试一次。
 *
 * V1 `overlay.py:835-849` 就是这么做的：先 `send_entry.focus_set()`，
 * 再 `after(50, _retry_focus)` 与 `after(150, _retry_focus)` 各重试一次。
 *
 * 为什么必须重试——这不是保守起见：Go 侧在推 hotkey 事件**之前**才刚调
 * `SetForegroundWindow`，而 Windows 的前台锁定（foreground lock）会让
 * 「刚置前就设焦点」经常不生效。单次调用表现出来就是「按了热键没反应」，
 * 而用户只会以为热键坏了。重试是原作者踩出来才加的。
 *
 * 重试时若窗口已不可见（等价于 V1 的 `root.state() == "withdrawn"` 检查）就放弃：
 * 那说明悬浮窗被藏起来了，再抢焦点只会打断用户正在别的程序里打的字。
 */
function focusInputWithRetry(): void {
  const focusOnce = (): void => {
    if (document.visibilityState === 'hidden') return
    inputRef.value?.focus()
  }
  focusOnce()
  window.setTimeout(focusOnce, 50)
  window.setTimeout(focusOnce, 150)
}

/**
 * 热键被按下时的动作。
 *
 *   copy  → copy_hotkey   复制最后一条译文
 *   send  → enter_hotkey  发送输入栏内容
 *   focus → send_hotkey   呼出输入栏
 */
function onHotkey(id: string): void {
  switch (id) {
    case 'focus':
      focusInputWithRetry()
      break

    case 'copy': {
      // 复制**最后一条**译文——V1 的复制热键就是这个语义
      const last = messages.value[messages.value.length - 1]
      if (!last) {
        showNotice('还没有可复制的译文', 'warn', 2000)
        return
      }
      void api
        .copyToClipboard(last.translated)
        .catch((e) => showNotice(`复制失败：${e instanceof Error ? e.message : String(e)}`, 'error', 3000))
      break
    }

    case 'send':
      // 这里**不用** focusInputWithRetry：submit() 是程序化提交，
      // 不依赖焦点落在输入栏上；而重试反而会在提交之后把焦点抢回来，
      // 让用户以为还想让他继续打字。
      inputRef.value?.focus()
      void inputRef.value?.submit()
      break

    default:
      break
  }
}

/**
 * 重连后补齐快照。
 * SSE 的自动重连**不会补发断线期间丢失的事件**，所以必须重新拉一次——
 * 否则统计条会停在旧数字，服务器名也可能过期。
 */
async function resync(): Promise<void> {
  await Promise.allSettled([refreshHealth(), refreshStats(), loadConfig()])
}

onMounted(async () => {
  disposers.push(onEvent(handleEvent))
  disposers.push(onResync(resync))

  // 首次进入也要拉一次，不能只依赖 onResync（组件可能晚于首次连接挂载）
  try {
    await resync()
    const cfg = display.value
    setMaxMessages(cfg.maxMessages)
  } catch (e) {
    showNotice(`无法连接后端：${e instanceof Error ? e.message : String(e)}`, 'error', 5000)
  }
})

onBeforeUnmount(() => {
  for (const d of disposers) d()
  disposers.length = 0
})

/** 供菜单/热键使用 */
defineExpose({
  scrollToBottom: () => listRef.value?.scrollToBottom(),
  focusInput: () => inputRef.value?.focus(),
  clearMessages,
})
</script>

<template>
  <!-- 右键菜单绑在根上：V1 只绑在消息文本上（overlay.py:349），那会导致
       在窗口别处右键「毫无反应」——用户会以为窗口坏了。V2 扩到整个窗口，
       行为是 V1 的超集（新增，不是改变）。 -->
  <div class="overlay" @contextmenu="openMenu">
    <!-- 行 0：2px 主色蓝高亮条 -->
    <div class="accent-line" />

    <!-- 行 1：header（32px，拖动窗口的抓取带） -->
    <OverlayHeader
      :version="version"
      :server="serverName"
      @mousedown="startDrag('caption', $event)"
    />

    <!-- 行 2：分隔线 -->
    <div class="rule" />

    <!-- 行 3：消息区（右键菜单由根元素统一处理） -->
    <MessageList
      ref="listRef"
      :messages="messages"
      :show-language="display.showLanguage"
      :show-original="display.showOriginal"
      :font-size="display.fontSize"
    />

    <!-- 行 4：提示条（V1 动态插入在输入栏上方） -->
    <div v-if="notice" class="notice" :class="notice.level">{{ notice.text }}</div>

    <!-- 行 5（V1 的输入栏，常驻） -->
    <InputBar
      ref="inputRef"
      :send-hotkey="display.sendHotkey"
      :font-size="display.fontSize"
      :send-ready="true"
      @notice="showNotice"
    />

    <!-- 行 6：分隔线 -->
    <div class="rule" />

    <!-- 行 7：统计条 -->
    <StatsBar :stats="stats" :send-hotkey="display.sendHotkey" />

    <ContextMenu
      v-if="menu.visible"
      :x="menu.x"
      :y="menu.y"
      @settings="onMenuSettings"
      @exit="onMenuExit"
      @close="closeMenu"
    />

    <!-- 边缘缩放热区。
         必须盖在内容之上（z-index 90/91），否则点不到；又必须**低于**
         右键菜单（ContextMenu 的 z-index 100），否则贴着边的菜单项会被
         热区吃掉点击。
         宽度用 RESIZE_BORDER 而不是写死数字：那个常量同时说明了它与
         V1 overlay.py:464 `BORDER = 8` 的对应关系。 -->
    <div class="rz rz-n" :style="{ height: `${RESIZE_BORDER}px` }" @mousedown="startDrag('top', $event)" />
    <div class="rz rz-s" :style="{ height: `${RESIZE_BORDER}px` }" @mousedown="startDrag('bottom', $event)" />
    <div class="rz rz-w" :style="{ width: `${RESIZE_BORDER}px` }" @mousedown="startDrag('left', $event)" />
    <div class="rz rz-e" :style="{ width: `${RESIZE_BORDER}px` }" @mousedown="startDrag('right', $event)" />
    <div class="rz rz-c rz-nw" :style="{ width: `${RESIZE_CORNER}px`, height: `${RESIZE_CORNER}px` }" @mousedown="startDrag('topleft', $event)" />
    <div class="rz rz-c rz-ne" :style="{ width: `${RESIZE_CORNER}px`, height: `${RESIZE_CORNER}px` }" @mousedown="startDrag('topright', $event)" />
    <div class="rz rz-c rz-sw" :style="{ width: `${RESIZE_CORNER}px`, height: `${RESIZE_CORNER}px` }" @mousedown="startDrag('bottomleft', $event)" />
    <div class="rz rz-c rz-se" :style="{ width: `${RESIZE_CORNER}px`, height: `${RESIZE_CORNER}px` }" @mousedown="startDrag('bottomright', $event)" />
  </div>
</template>

<style scoped>
.overlay {
  height: 100%;
  display: flex;
  flex-direction: column;
  /* V1 的 1px 描边线蓝外框（overlay.py 用 padx=1 的 Frame 实现） */
  border: 1px solid var(--stroke-strong);
  overflow: hidden;
}

.accent-line {
  flex: none;
  height: 2px;
  background: var(--accent);
}

/* ── 边缘缩放热区 ──
   完全透明，只用来接鼠标。尺寸由 :style 从 useWindowDrag 的常量注入，
   这里不管宽高，只负责定位与指针形状。 */
.rz {
  position: fixed;
  z-index: 90;
  background: transparent;
}

.rz-n {
  top: 0;
  left: 0;
  right: 0;
  cursor: ns-resize;
}

.rz-s {
  bottom: 0;
  left: 0;
  right: 0;
  cursor: ns-resize;
}

.rz-w {
  top: 0;
  bottom: 0;
  left: 0;
  cursor: ew-resize;
}

.rz-e {
  top: 0;
  bottom: 0;
  right: 0;
  cursor: ew-resize;
}

/* 角压在边之上：8px 的边与 14px 的角重叠的那一小块应该是斜向缩放。 */
.rz-c {
  z-index: 91;
}

.rz-nw {
  top: 0;
  left: 0;
  cursor: nwse-resize;
}

.rz-ne {
  top: 0;
  right: 0;
  cursor: nesw-resize;
}

.rz-sw {
  bottom: 0;
  left: 0;
  cursor: nesw-resize;
}

.rz-se {
  bottom: 0;
  right: 0;
  cursor: nwse-resize;
}

.rule {
  flex: none;
  height: 1px;
  background: var(--stroke-strong);
  opacity: 0.5;
  margin: 0 10px;
}

.notice {
  flex: none;
  margin: 2px 10px 0;
  padding: 2px 8px;
  background: var(--layer-flyout);
  /* V1 的提示条沿用的是 10pt 那档 → 13px */
  font-size: 13px;
  font-weight: bold;
  text-align: center;
}

.notice.error {
  color: var(--msg-error);
}

.notice.warn {
  color: var(--warn);
}

.notice.info {
  color: var(--text);
}

/* 「翻译了 N 条消息」——V1 overlay.py:721 用的是 SELF_GREEN 前景 + 深绿底。
   这里保留绿色语义，底色用主题 token 合成（浅色主题下深绿底会很难看）。 */
.notice.ok {
  color: var(--msg-self);
  background: color-mix(in srgb, var(--msg-self) 18%, var(--layer-flyout));
}
</style>
