<script setup lang="ts">
/**
 * 表单行：左标签 + 右控件。
 *
 * 对应 V1 main.py:429 的 `_row(card, r, label_text, widget, extra)`。
 * V1 的标签是右对齐的固定宽度列；Win11 的设置项改为**左对齐**，
 * 因为右对齐在浅色主题下会显得参差。这里跟随 Win11。
 *
 * 控件左对齐还能让「标签 → 控件」的视线更顺，也让长标签不会挤压控件。
 */
defineProps<{
  label: string
  hint?: string
}>()
</script>

<template>
  <div class="row">
    <label class="label">{{ label }}</label>
    <div class="control">
      <slot />
      <span v-if="hint" class="hint">{{ hint }}</span>
    </div>
  </div>
</template>

<style scoped>
.row {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 6px 0;
}

.label {
  flex: none;
  width: 116px;
  font-size: 12px;
  color: var(--text);
}

.control {
  flex: 1 1 auto;
  display: flex;
  align-items: center;
  gap: 8px;
  min-width: 0;
}

.hint {
  font-size: 11px;
  color: var(--text-tertiary);
}
</style>
