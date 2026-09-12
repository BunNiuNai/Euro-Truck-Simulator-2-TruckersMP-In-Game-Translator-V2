<script setup lang="ts">
/**
 * 消息区的右键菜单。
 *
 * V1 overlay.py:344-349 只有两项：`Settings / 设置` 与 `Exit / 退出`（中间一条分隔线）。
 * 这里保持一致——不加 V1 没有的菜单项（首要原则 P1）。
 *
 * 用 DOM 自绘而不是 native 弹出菜单：样式统一、跟随深色主题、中文不会有问题。
 * V1 用的是 tkinter 原生 Menu，视觉上有差异，功能一致。
 */
import { computed, onBeforeUnmount, onMounted } from 'vue'

const props = defineProps<{
  x: number
  y: number
}>()

const emit = defineEmits<{
  (e: 'settings'): void
  (e: 'exit'): void
  (e: 'close'): void
}>()

/** 与 .ctx-menu 的 min-width 及两项加分隔线的实际高度保持一致 */
const MENU_W = 150
const MENU_H = 62

/** 菜单不能超出窗口边界，否则会被裁掉一半。 */
const menuStyle = computed(() => {
  const maxX = Math.max(0, window.innerWidth - MENU_W - 2)
  const maxY = Math.max(0, window.innerHeight - MENU_H - 2)
  return {
    left: `${Math.min(props.x, maxX)}px`,
    top: `${Math.min(props.y, maxY)}px`,
  }
})

function onKey(e: KeyboardEvent): void {
  if (e.key === 'Escape') emit('close')
}

/** 点菜单外面关闭。用捕获阶段，避免被内部元素的 stopPropagation 挡住。 */
function onClickOutside(e: MouseEvent): void {
  const el = e.target as HTMLElement | null
  if (el && el.closest('.ctx-menu')) return
  emit('close')
}

onMounted(() => {
  window.addEventListener('keydown', onKey)
  window.addEventListener('mousedown', onClickOutside, true)
})

onBeforeUnmount(() => {
  window.removeEventListener('keydown', onKey)
  window.removeEventListener('mousedown', onClickOutside, true)
})
</script>

<template>
  <div class="ctx-menu" :style="menuStyle">
    <button class="item" @click="emit('settings')">Settings / 设置</button>
    <div class="divider" />
    <button class="item" @click="emit('exit')">Exit / 退出</button>
  </div>
</template>

<style scoped>
.ctx-menu {
  position: fixed;
  z-index: 100;
  min-width: 140px;
  /* ⚠️ 原来写死 #222222：切到浅色主题之后这个菜单还是深色的，
     看起来像「没跟着换主题」。浮出层一律用 token。 */
  background: var(--layer-flyout);
  border: 1px solid var(--stroke);
  border-radius: var(--radius);
  padding: 3px 0;
  box-shadow: 0 4px 14px rgba(0, 0, 0, 0.6);
}

.item {
  display: block;
  width: 100%;
  text-align: left;
  padding: 5px 12px;
  /* ⚠️ 这里**故意**不做 pt→px 换算。
     菜单其余部分都按 4/3 放大过（V1 是 tkinter 的磅），但 V1 的右键菜单是
     **tk 原生 Menu**，用的是系统菜单字体（9pt ≈ 12px），本来就是 12px。
     跟着放大反而会比系统菜单大一圈，与 V1 的观感不一致。 */
  font-size: 12px;
  color: var(--text);
}

.item:hover {
  background: var(--accent);
  /* 强调色之上的文字色跟随主题（深色主题的强调色是亮蓝，要用黑字） */
  color: var(--accent-on);
}

.divider {
  height: 1px;
  background: var(--stroke);
  margin: 3px 0;
}
</style>
