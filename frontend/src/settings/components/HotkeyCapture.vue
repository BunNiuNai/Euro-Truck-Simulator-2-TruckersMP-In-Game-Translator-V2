<script setup lang="ts">
/**
 * 热键捕获控件。
 *
 * 对应 V1 main.py:267-330 的 `HotkeyCapture`：**按下组合键即捕获**，
 * 而不是让用户手打字符串。V1 的实现要点：
 *   · 获得焦点后进入捕获态，显示「按下组合键…」
 *   · `_on_key` 里读 keysym，把 Control/Shift/Alt 映射成 ctrl/shift/alt
 *   · 只按修饰键不结束捕获（必须有一个主键）
 *   · 捕获后失焦并显示格式化后的组合
 *
 * 这里保持同样的交互，另外保留手动输入的能力——捕获控件在 WebView 里
 * 可能被浏览器的默认快捷键抢走（例如 Ctrl+W 关标签），手输是个必要退路。
 */
import { computed, ref } from 'vue'

const props = defineProps<{
  modelValue: string
  placeholder?: string
}>()

const emit = defineEmits<{
  (e: 'update:modelValue', v: string): void
}>()

const capturing = ref(false)
const editing = ref(false)
const draft = ref('')

/** V1 main.py:284 `_fmt`：按 + 拆开、首字母大写、重新拼接 */
function format(raw: string): string {
  return raw
    .trim()
    .split('+')
    .map((p) => p.trim())
    .filter(Boolean)
    .map((p) => p.charAt(0).toUpperCase() + p.slice(1).toLowerCase())
    .join('+')
}

const display = computed(() => (capturing.value ? '按下组合键…' : format(props.modelValue) || '（未设置）'))

/** 只按修饰键时不结束捕获——必须有主键，否则会捕获出 "ctrl+" 这种残缺值 */
const MODIFIER_KEYS = new Set(['Control', 'Shift', 'Alt', 'Meta', 'OS'])

function normalizeKey(e: KeyboardEvent): string | null {
  const k = e.key
  if (MODIFIER_KEYS.has(k)) return null

  if (k === ' ') return 'space'
  if (k === 'Escape') return null // Esc 取消捕获
  if (k.length === 1) return k.toLowerCase()

  // 特殊键保持原名（enter / f1 / arrowup …），与 V1 的 KEY_NAME_MAP 命名一致
  return k.toLowerCase()
}

function onKeydown(e: KeyboardEvent): void {
  if (!capturing.value) return

  // 捕获期间要吞掉所有按键，否则会触发浏览器自己的快捷键
  e.preventDefault()
  e.stopPropagation()

  if (e.key === 'Escape') {
    capturing.value = false
    return
  }

  const main = normalizeKey(e)
  if (main === null) return // 还在按修饰键

  const parts: string[] = []
  if (e.ctrlKey) parts.push('ctrl')
  if (e.shiftKey) parts.push('shift')
  if (e.altKey) parts.push('alt')
  // 主键本身是修饰键时已经 return，不必额外处理
  parts.push(main)

  emit('update:modelValue', parts.join('+'))
  capturing.value = false
}

function startCapture(): void {
  capturing.value = true
}

/** 手输模式：用户可以直接敲字符串（捕获被浏览器抢键时的退路） */
function commitDraft(): void {
  editing.value = false
  const v = draft.value.trim()
  if (v) emit('update:modelValue', v)
}
</script>

<template>
  <div class="capture">
    <button
      v-if="!editing"
      class="field"
      :class="{ capturing }"
      type="button"
      @click="startCapture"
      @keydown="onKeydown"
      @blur="capturing = false"
    >
      {{ display }}
    </button>
    <input
      v-else
      class="field"
      :value="draft"
      :placeholder="placeholder ?? 'shift+y'"
      @input="draft = ($event.target as HTMLInputElement).value"
      @keydown.enter.prevent="commitDraft"
      @keydown.esc.prevent="editing = false"
      @blur="commitDraft"
      autofocus
    />

    <button class="mode" type="button" :title="editing ? '改为按键捕获' : '改为手动输入'" @click="editing = !editing">
      {{ editing ? '⌨' : '✎' }}
    </button>
    <button
      v-if="modelValue"
      class="mode"
      type="button"
      title="清空"
      @click="emit('update:modelValue', '')"
    >
      ✕
    </button>
  </div>
</template>

<style scoped>
.capture {
  display: flex;
  align-items: center;
  gap: 4px;
}

.field {
  min-width: 160px;
  padding: 5px 10px;
  border-radius: var(--radius-control);
  border: 1px solid var(--stroke);
  background: var(--layer-control);
  color: var(--text);
  font-family: var(--font-ui);
  font-size: 12px;
  text-align: left;
  outline: none;
  transition:
    border-color var(--motion-fast),
    background var(--motion-fast);
}

.field:hover {
  background: var(--layer-control-hover);
}

/* 捕获态：强调色描边 + 淡强调底，明确告诉用户「正在听按键」 */
.field.capturing {
  border-color: var(--accent);
  background: color-mix(in srgb, var(--accent) 14%, var(--layer-control));
  color: var(--accent);
}

input.field {
  cursor: text;
}

.mode {
  flex: none;
  width: 26px;
  height: 26px;
  border-radius: var(--radius-control);
  border: 1px solid var(--stroke);
  background: var(--layer-control);
  color: var(--text-secondary);
  font-size: 12px;
  line-height: 1;
}

.mode:hover {
  background: var(--layer-control-hover);
  color: var(--text);
}
</style>
