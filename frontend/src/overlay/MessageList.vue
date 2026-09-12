<script setup lang="ts">
/**
 * 消息列表。
 *
 * 滚动是 WebView 原生能力（ADR-011），不需要自己实现。
 * 新消息到达时自动滚到底部——除非用户已经往上翻了，那时不该把视线拽走。
 */
import { nextTick, ref, watch } from 'vue'
import type { RenderedMessage } from '../api/types'
import MessageItem from './MessageItem.vue'

const props = defineProps<{
  messages: readonly RenderedMessage[]
  showLanguage: boolean
  showOriginal: boolean
  fontSize: number
}>()

const scroller = ref<HTMLElement | null>(null)

/** 用户是否贴着底部。往上翻了就置 false，不再自动滚动。 */
const atBottom = ref(true)

const NEAR_BOTTOM_PX = 24

function onScroll(): void {
  const el = scroller.value
  if (!el) return
  atBottom.value = el.scrollHeight - el.scrollTop - el.clientHeight <= NEAR_BOTTOM_PX
}

watch(
  () => props.messages.length,
  async () => {
    if (!atBottom.value) return
    await nextTick()
    const el = scroller.value
    if (el) el.scrollTop = el.scrollHeight
  },
)

/** 供右键菜单使用：滚到底部。 */
function scrollToBottom(): void {
  const el = scroller.value
  if (el) el.scrollTop = el.scrollHeight
  atBottom.value = true
}

defineExpose({ scrollToBottom })
</script>

<template>
  <div ref="scroller" class="list" @scroll="onScroll">
    <div v-if="messages.length === 0" class="empty">等待聊天消息…</div>
    <MessageItem
      v-for="m in messages"
      :key="m.id || `${m.timestamp}-${m.speaker}-${m.original}`"
      :msg="m"
      :show-language="showLanguage"
      :show-original="showOriginal"
      :font-size="fontSize"
    />
  </div>
</template>

<style scoped>
.list {
  flex: 1 1 auto;
  min-height: 0; /* 没有它，flex 子项不会收缩，滚动条永远不出现 */
  overflow-y: auto;
  overflow-x: hidden;
  padding: 4px 10px;
}

.empty {
  color: var(--msg-timestamp);
  text-align: center;
  padding-top: 24px;
}
</style>
