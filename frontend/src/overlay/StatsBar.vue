<script setup lang="ts">
/**
 * 底部统计条（左：计数   右：快捷键提示）。
 *
 * 三个计数与 V1 overlay.py:288-290 完全一致：已翻译 / 命中 / 节省。
 * 数字用主色蓝加粗，标签用暗灰——V1 的 FG_STATS_NUM / FG_STATS_LABEL。
 *
 * 数值全部来自 Go（/api/stats 与 stats.updated），前端不自己累加。
 */
import { computed } from 'vue'
import type { StatsPayload } from '../api/types'

const props = defineProps<{
  stats: StatsPayload
  sendHotkey: string
}>()

/** V1 overlay.py:750-753 的 _format_hotkey：按 + 拆开、首字母大写、重新拼接。 */
const hotkeyText = computed(() => {
  const parts = props.sendHotkey
    .trim()
    .split('+')
    .map((p) => p.trim())
    .filter(Boolean)
    .map((p) => p.charAt(0).toUpperCase() + p.slice(1).toLowerCase())
  return parts.join('+')
})
</script>

<template>
  <div class="bar">
    <div class="stats">
      <span class="pair"><span class="label">已翻译:</span><span class="num">{{ stats.translated }}</span></span>
      <span class="pair"><span class="label">命中:</span><span class="num">{{ stats.cached }}</span></span>
      <span class="pair"><span class="label">节省:</span><span class="num">{{ stats.savingsPct }}</span></span>
    </div>
    <div class="shortcuts">
      <span v-if="hotkeyText">{{ hotkeyText }} 呼出</span>
      <span class="sep">|</span>
      <span>Enter 发送</span>
    </div>
  </div>
</template>

<style scoped>
.bar {
  flex: none;
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
  background: var(--layer-card);
  padding: 2px 10px 3px;
}

.stats {
  display: flex;
  gap: 16px;
}

.pair {
  display: inline-flex;
  align-items: baseline;
  gap: 2px;
}

/* ⚠️ 字号单位换算：V1 是 tkinter，字号单位是**磅**（96dpi 下 1pt = 4/3 px）。
   把 V1 的 8/9/9 直接写成 8px/9px/9px 会让整条统计栏小掉四分之一。
   这里按 pt→px 换算：8pt→11px、9pt→12px。 */
.label,
.shortcuts {
  font-size: 11px;
  color: var(--text-secondary);
}

.label {
  font-size: 12px;
}

.num {
  font-size: 12px;
  font-weight: bold;
  color: var(--accent);
}

.shortcuts {
  display: flex;
  gap: 4px;
  flex: none;
}

.sep {
  opacity: 0.5;
}
</style>
