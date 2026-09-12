<script setup lang="ts">
/**
 * 顶部标题栏（左：版本 · 服务器名   右：北京时间）。
 *
 * ⚠️ 这一行是**拖动窗口的抓取带**：父组件在这里绑了
 * `@mousedown="startDrag('caption', $event)"`，区域名经 Go → native
 * 变成窗口位移（见 composables/useWindowDrag.ts）。
 *
 * 曾经试过 CSS `app-region: drag`（配 native 的
 * ICoreWebView2Settings9::IsNonClientRegionSupportEnabled）——那是官方推荐的
 * 做法，但本机实测不产生任何位移，而且它只能拖、不能缩放。已放弃，详见
 * native/src/webview.cpp 里留的调查记录。
 *
 * 因此这里**不宜再放可交互控件**：放了会与拖动抢 mousedown。真要放就得给
 * 控件单独 stopPropagation。
 *
 * 与 V1 的实现差异：V1 整个画布都能拖（tkinter 自己算的，约 70 行），
 * V2 只有这 32px 能拖。整个画布可拖会让滚动、选中、输入全废。
 */
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { SERVER_WAITING } from '../composables/useHealth'

const props = defineProps<{
  version: string
  server: string
}>()

/**
 * 是否已经连上服务器 —— 决定服务器名用什么颜色。
 *
 * V1 `overlay.py:564-572` 的 `_poll_server_name` 每 2 秒读一次共享引用里的
 * 服务器名，并按「名字是否为空串」分两支：
 *   `:569`  name 非空 → `server_label.config(text=name, fg=ACCENT)`  主色蓝
 *   `:571`  name 为空 → 写回占位文案并 `fg=FG_DIM`                    次级色
 * 建 label 时的初始值也是 FG_DIM（`overlay.py:200-203`）。
 *
 * V2 的 `useHealth.setServerName` 在收到空名字时已经把值规整成占位文案，
 * 所以这里用「不等于占位文案」来等价表达 V1 的「name 非空」。
 * 直接判 `props.server !== ''` 是错的：那个判据在 V2 里恒为真，
 * 于是没连上服务器时名字也会是主色蓝，正好和 V1 反了。
 */
const connected = computed(() => props.server !== SERVER_WAITING)

/** 北京时间 HH:MM:SS —— V1 overlay.py:539-542 用的是 UTC+8 固定偏移。 */
const clock = ref('')
let timer: number | undefined

function tick(): void {
  const beijing = new Date(Date.now() + (new Date().getTimezoneOffset() + 480) * 60_000)
  const p = (n: number) => String(n).padStart(2, '0')
  clock.value = `${p(beijing.getHours())}:${p(beijing.getMinutes())}:${p(beijing.getSeconds())}`
}

onMounted(() => {
  tick()
  timer = window.setInterval(tick, 1000)
})

onBeforeUnmount(() => {
  if (timer !== undefined) window.clearInterval(timer)
})
</script>

<template>
  <div class="header">
    <div class="left">
      <span class="version">{{ version }}</span>
      <span class="dot">·</span>
      <span class="server" :class="{ connected }">{{ server }}</span>
    </div>
    <span class="clock">{{ clock }}</span>
  </div>
</template>

<style scoped>
.header {
  height: var(--caption-height);
  flex: none;
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 0 12px;

  /* 拖动窗口的抓取带：mousedown 由父组件接到 useWindowDrag('caption')。
     指针用 default 而不是 V1 的 fleur——V1 的 fleur 施在整个画布上，
     而这里只是一条 32px 的标题带，Windows 标题栏本来就是普通箭头。 */
  cursor: default;

  /* 拖拽带里文字不该被选中：选中会与其后的拖动抢鼠标 */
  user-select: none;
}

.left {
  display: flex;
  align-items: baseline;
  gap: 4px;
  min-width: 0;
}

/* ⚠️ 字号单位换算：V1 是 tkinter，字号单位是**磅**（96dpi 下 1pt = 4/3 px）。
   V1 `overlay.py:193-216` 的标题栏用的是 9pt，直接写成 9px 会小掉四分之一。
   9pt → 12px。 */
.version {
  font-size: 12px;
  font-weight: bold;
  color: var(--text);
}

.dot,
.server,
.clock {
  font-size: 12px;
  color: var(--text-secondary);
}

/* 连上服务器后服务器名改用主色强调 —— V1 overlay.py:569 的 `fg=ACCENT`
   （ACCENT 是 V1 的主色蓝 #4494FC，对应这里的 --accent 令牌，不写死色值）。
   没连上时保持上面的次级色，对应 V1 overlay.py:571 的 `fg=FG_DIM`。 */
.server.connected {
  color: var(--accent);
}

.server {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.clock {
  flex: none;
}
</style>
