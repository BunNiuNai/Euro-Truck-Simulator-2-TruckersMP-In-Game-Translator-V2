<script setup lang="ts">
/**
 * Win11 风格的开关（ToggleSwitch）。
 *
 * 规格取自 WinUI 3：轨道 40×20 全圆角，圆点 12px、四边留 4px。
 * 关闭态是「透明轨道 + 描边」，开启态是「强调色实心轨道」——
 * 这个差异是 Win11 开关最显著的特征，不做成两种颜色填充。
 */
defineProps<{
  modelValue: boolean
  disabled?: boolean
}>()

const emit = defineEmits<{
  (e: 'update:modelValue', v: boolean): void
}>()
</script>

<template>
  <button
    class="toggle"
    type="button"
    role="switch"
    :aria-checked="modelValue"
    :disabled="disabled"
    :class="{ on: modelValue }"
    @click="emit('update:modelValue', !modelValue)"
  >
    <span class="knob" />
  </button>
</template>

<style scoped>
.toggle {
  position: relative;
  flex: none;
  width: 40px;
  height: 20px;
  border-radius: 10px;
  /* 关闭态：透明填充 + 指向文字色的描边 */
  background: transparent;
  border: 1px solid var(--text-secondary);
  padding: 0;
  transition:
    background var(--motion-normal),
    border-color var(--motion-normal);
}

.toggle:hover:not(:disabled) {
  background: var(--layer-control-hover);
}

.toggle.on {
  background: var(--accent);
  border-color: var(--accent);
}

.toggle.on:hover:not(:disabled) {
  background: var(--accent-hover);
  border-color: var(--accent-hover);
}

.toggle:disabled {
  opacity: 0.4;
  cursor: default;
}

.knob {
  position: absolute;
  top: 3px;
  left: 3px;
  width: 12px;
  height: 12px;
  border-radius: 50%;
  /* 关闭态圆点用文字色，开启态用强调色之上的对比色 */
  background: var(--text-secondary);
  transition:
    transform var(--motion-normal),
    background var(--motion-normal);
}

.toggle.on .knob {
  transform: translateX(20px);
  background: var(--accent-on);
}
</style>
