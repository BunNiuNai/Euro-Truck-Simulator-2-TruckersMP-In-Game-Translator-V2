<script setup lang="ts">
/**
 * 单条消息的渲染。
 *
 * 格式严格对齐 V1 overlay.py:629-677 的 4 行布局：
 *   第 1 行  [语言标签][(You) ][玩家名]        时间戳（右对齐）
 *   第 2 行    译文（错误文案用红色）
 *   第 3 行    原文（灰色小字，可关闭）
 *   第 4 行    ── 分隔线 ──
 *
 * 系统消息是 3 行（无玩家名行，全灰）。
 *
 * 与 V1 的实现差异（视觉等价）：
 *   V1 用 `_pad_line` 拿空格把时间戳顶到右边（tkinter Text 没有布局能力）；
 *   这里用 flex 的 space-between。结果一样，且不受名字长短影响。
 *
 * ⚠️ 安全：玩家名与聊天内容来自联机服务器上的其他玩家，属于**不可信输入**。
 * V1 用 tkinter Text 渲染，天然没有 HTML 注入面；换成 WebView 后这个面出现了。
 * 因此这里**一律用 {{ }} 插值**（Vue 的插值是 textContent 语义，会自动转义），
 * 永远不要改成 v-html，也不要用 innerHTML 拼接任何动态值。
 */
import { computed } from 'vue'
import type { RenderedMessage } from '../api/types'

const props = defineProps<{
  msg: RenderedMessage
  showLanguage: boolean
  showOriginal: boolean
  fontSize: number
}>()

/**
 * V1 overlay.py:667 —— 只在 `trans != orig` 的分支里才判断是否为错误，
 * 判据是译文以 "[" 开头（V1 的错误文案全部形如 "[网络错误] …"）。
 */
const isError = computed(
  () => props.msg.translated !== props.msg.original && props.msg.translated.startsWith('['),
)

/** 第 1 行的左侧文本。语言标签只在开启且非自己消息时显示（V1 overlay.py:653）。 */
const playerText = computed(() => {
  const lang = props.showLanguage && props.msg.lang && !props.msg.isSelf ? `[${props.msg.lang}] ` : ''
  const prefix = props.msg.isSelf ? '(You) ' : ''
  return `${lang}${prefix}[${props.msg.speaker}]`
})

/** 第 2 行的内容：有译文显示译文，否则显示原文（V1 overlay.py:666-670）。 */
const bodyText = computed(() =>
  props.msg.translated !== props.msg.original ? props.msg.translated : props.msg.original,
)

/**
 * 第 3 行（原文）的显示条件，V1 overlay.py:673：
 * 开关打开 且 原文≠译文 且 译文不是错误文案。
 * 错误文案已经在第 2 行显示过了，再显示一遍原文没有意义。
 */
const showOriginalLine = computed(
  () => props.showOriginal && props.msg.original !== props.msg.translated && !isError.value,
)

/** 系统消息的原文行只判 开关 + 不同（V1 overlay.py:646）。 */
const showSystemOriginal = computed(
  () => props.showOriginal && props.msg.original !== props.msg.translated,
)

/**
 * 字号规则来自 V1 overlay.py:249-268，是一个以 font_size 为基准的阶梯。
 *
 * ⚠️ 单位换算：V1 是 tkinter，字号单位是**磅**（96dpi 下 1pt = 4/3 px）。
 * 直接把 V1 的数字当 px 用会小掉四分之一——这正是「悬浮窗文字太小」的根因。
 * 所以下面每一档都走 pt→px 换算。
 */
const sizes = computed(() => {
  const pt = (n: number) => `${Math.round((n * 4) / 3)}px`
  const fs = props.fontSize
  return {
    player: pt(fs),
    body: pt(fs),
    system: pt(Math.max(8, fs - 1)),
    original: pt(Math.max(8, fs - 2)),
    timestamp: pt(Math.max(8, fs - 1)),
  }
})
</script>

<template>
  <!-- 系统消息：3 行，整条灰色 -->
  <div v-if="msg.isSystem" class="item" :style="{ fontSize: sizes.system }">
    <div class="head">
      <span class="system-name">[System]</span>
      <span class="ts" :style="{ fontSize: sizes.timestamp }">{{ msg.timestamp }}</span>
    </div>
    <div class="body system selectable">{{ msg.translated }}</div>
    <div v-if="showSystemOriginal" class="body original selectable">{{ msg.original }}</div>
    <div class="rule" />
  </div>

  <!-- 普通消息：4 行 -->
  <div v-else class="item" :style="{ fontSize: sizes.body }">
    <div class="head">
      <span :class="msg.isSelf ? 'self-name' : 'player-name'" :style="{ fontSize: sizes.player }">
        {{ playerText }}
      </span>
      <span class="ts" :style="{ fontSize: sizes.timestamp }">{{ msg.timestamp }}</span>
    </div>
    <div class="body selectable" :class="isError ? 'error' : 'translation'">{{ bodyText }}</div>
    <div v-if="showOriginalLine" class="body original selectable" :style="{ fontSize: sizes.original }">
      {{ msg.original }}
    </div>
    <div class="rule" />
  </div>
</template>

<style scoped>
.item {
  padding: 0 2px;
}

.head {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
  gap: 8px;
}

.player-name {
  /* V1 PLAYER = HIGHLIGHT #70b8ff */
  color: var(--msg-player);
  font-weight: bold;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.self-name {
  /* V1 SELF_GREEN #4ec9b0 */
  color: var(--msg-self);
  font-weight: bold;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.system-name {
  color: var(--text-secondary);
}

.ts {
  /* V1 FG_TIMESTAMP #666666 */
  color: var(--msg-timestamp);
  flex: none;
}

.body {
  /* 译文与原文都缩进 2 个字符（V1 的 "  {text}"） */
  padding-left: 1em;
  word-break: break-word;
  white-space: pre-wrap;
}

.translation {
  /* V1 TRANSL #ffd700 */
  color: var(--msg-translation);
}

.error {
  /* V1 ERROR_RED #f44747 */
  color: var(--msg-error);
}

.original {
  color: var(--text-secondary);
}

.system {
  color: var(--text-secondary);
}

.rule {
  /* V1 用 70 个 "─" 字符；这里用 1px 线，视觉等价且不会在窄窗口溢出 */
  border-top: 1px solid var(--stroke-strong);
  opacity: 0.35;
  margin: 4px 0 6px;
}
</style>
