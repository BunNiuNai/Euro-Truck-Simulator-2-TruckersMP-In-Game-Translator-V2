<script setup lang="ts">
/**
 * 输入栏（发送方向的入口）。
 *
 * ⚠️ 与 V1 一致：这是**常驻显示**的，不是隐藏的。
 * V1 overlay.py:305 的注释写着 "Always visible"；所谓「呼出」只是把焦点给它
 * （_show_input 就是一句 focus_set）。这里同样：热键到来时聚焦，不改变可见性。
 *
 * 整条发送链路在 Go 的 `POST /api/compose/send`（internal/compose）：翻译 →
 * 校验 → 藏起悬浮窗 → 模拟按键 → 读聊天日志确认。这里只负责**渲染结果**——
 * 提示文案与是否往消息列表插一条 `(Sent)`，逐条对齐 V1 overlay.py:923-945。
 *
 * 一处已知差异：V1 分两步显示「翻译中...」→「正在发送到游戏...」，
 * 因为那两步在它自己的线程里；V2 一次请求走完，所以合成一句
 * 「翻译并发送中...」。文案不同，用户可感知的信息量相同。
 */
import { computed, nextTick, onBeforeUnmount, ref } from 'vue'
import { api, ApiError } from '../api/client'
import { appendSent, appendSystem } from '../composables/useMessages'

const props = defineProps<{
  sendHotkey: string
  fontSize: number
  /** 发送链路是否可用（native 未连接时置 false，只翻译不发送） */
  sendReady: boolean
}>()

const emit = defineEmits<{ (e: 'notice', text: string, level: string): void }>()

const text = ref('')
const input = ref<HTMLInputElement | null>(null)
const sending = ref(false)

/**
 * 输入栏右侧的状态提示。空串表示显示默认的「{热键} 呼出」。
 * 对应 V1 的 `self.send_hint` 标签。
 */
const hintText = ref('')
const hintLevel = ref<'info' | 'ok' | 'warn' | 'error' | 'dim'>('info')

/**
 * 提示条的复位定时器。
 *
 * V1 每次发送结束都会经 `_hide_input → _update_hotkey_hint` 把右侧提示复位成
 * 「{热键} 呼出」（overlay.py:741-744）。V2 早期版本被状态文案写过就**再也不
 * 复位**——用户发过一次消息后，「{热键} 呼出」这个唯一告诉他快捷键的提示
 * 就永久消失了。
 */
let hintTimer: number | undefined
const HINT_RESET_MS = 4000

function setHint(t: string, level: typeof hintLevel.value = 'info'): void {
  hintText.value = t
  hintLevel.value = level

  if (hintTimer !== undefined) window.clearTimeout(hintTimer)
  // 空串表示「回到默认的 {热键} 呼出」，不需要定时器
  if (t === '') return
  hintTimer = window.setTimeout(() => {
    hintText.value = ''
    hintTimer = undefined
  }, HINT_RESET_MS)
}

/** V1 overlay.py:750-753 —— 与统计条同一套格式化。 */
function formatHotkey(raw: string): string {
  return raw
    .trim()
    .split('+')
    .map((p) => p.trim())
    .filter(Boolean)
    .map((p) => p.charAt(0).toUpperCase() + p.slice(1).toLowerCase())
    .join('+')
}

/**
 * 输入框字号。
 *
 * ⚠️ V1 的 `cfg.font_size` 是 tkinter 的**磅**（96dpi 下 1pt = 4/3 px），
 * 直接当 px 用会小掉四分之一。V1 overlay.py:321 用的就是这个值。
 */
const entrySize = computed(() => `${Math.round((props.fontSize * 4) / 3)}px`)

/** 聚焦输入框。热键唤起时调用。 */
function focus(): void {  input.value?.focus()
}

/** 清空并失焦（V1 _hide_input 的等价物，Esc 触发）。 */
function clearAndBlur(): void {
  text.value = ''
  input.value?.blur()
}

async function onEnter(): Promise<void> {
  await submit()
}

/**
 * 提交输入栏内容。热键「发送」走的也是这里，保证两条路径行为一致
 * （否则键盘回车与热键会慢慢长出差异）。
 */
async function submit(): Promise<void> {
  const value = text.value.trim()
  if (!value || sending.value) return

  if (!props.sendReady) {
    emit('notice', '发送链路不可用（native 未连接或未配置 Provider）', 'error')
    return
  }

  sending.value = true
  // V1 在提交瞬间就清空并禁用输入框（overlay.py:879-880）：发送要好几秒，
  // 期间用户再敲的字会被后半段流程当成新一轮输入，状态会很乱。
  text.value = ''
  setHint(' 翻译并发送中... ', 'info')

  try {
    const out = await api.sendManual(value)
    applyOutcome(out, value)
  } catch (e) {
    setHint(' 发送失败 ', 'error')
    text.value = value // 出错时把内容还给用户，别让他重打一遍
    emit('notice', e instanceof ApiError ? e.message : String(e), 'error')
  } finally {
    sending.value = false
  }
}

/** 按后端返回的 result 渲染结果。分支与文案逐条对齐 V1 overlay.py:928-943。 */
function applyOutcome(
  out: { result: string; english: string; message?: string },
  chinese: string,
): void {
  switch (out.result) {
    case 'OK_CONFIRMED':
      appendSent(chinese, out.english)
      setHint(' 已发送并确认 ✓ ', 'ok')
      break

    case 'OK_UNCONFIRMED':
      appendSent(chinese, out.english)
      setHint(' 已发送（未确认） ', 'warn')
      break

    case 'FAIL_TRANSLATION':
      // V1 overlay.py:947-955：翻译异常除了改提示，还往消息列表插一条
      // System 消息把原因留在时间线上（提示条几秒就没了，列表里的还在）。
      appendSystem('发送翻译失败', out.message || '翻译失败')
      refill(out.english || chinese)
      setHint(' 翻译无效，未发送 ', 'error')
      break

    case 'FAIL_SEND':
      refill(out.english || chinese)
      setHint(' 发送失败 ', 'error')
      break

    case 'BUSY':
      refill(chinese)
      setHint(' 上一次发送还没结束 ', 'warn')
      break

    default:
      setHint(' 未知状态 ', 'dim')
  }
}

/**
 * 把文本放回输入框并**全选**。
 *
 * V1 `overlay.py:936,940` 回填后还会 `select_range(0, END)`：用户可以直接
 * 重新输入覆盖掉它。只回填不选中，用户得先手动清空一次才能重打。
 */
function refill(value: string): void {
  text.value = value
  void nextTick(() => {
    input.value?.focus()
    input.value?.select()
  })
}

onBeforeUnmount(() => {
  if (hintTimer !== undefined) window.clearTimeout(hintTimer)
})

defineExpose({ focus, clearAndBlur, submit })
</script>

<template>
  <div class="input-area">
    <div class="row">
      <input
        ref="input"
        v-model="text"
        class="entry"
        type="text"
        :style="{ fontSize: entrySize }"
        :disabled="sending"
        @keydown.enter.prevent="onEnter"
        @keydown.esc.prevent="clearAndBlur"
      />
      <span class="hint" :class="hintLevel">{{ hintText || `${formatHotkey(sendHotkey)} 呼出` }}</span>
    </div>
  </div>
</template>

<style scoped>
.input-area {
  flex: none;
  background: var(--layer-card);
  padding: 2px 10px;
}

.row {
  display: flex;
  align-items: center;
  gap: 6px;
}

.entry {
  flex: 1 1 auto;
  min-width: 0;
  background: var(--layer-control);
  color: var(--text);
  border: none;
  outline: none;
  padding: 4px 6px;
  font-family: inherit;
  caret-color: var(--text);
}

.entry:disabled {
  opacity: 0.6;
}

.hint {
  flex: none;
  /* V1 overlay.py:329 的 send_hint 是 9pt → 12px */
  font-size: 12px;
  color: var(--text-secondary);
}

/* 状态提示配色沿用 V1 的语义：成功绿、未确认黄、失败红。
   色值取主题 token，不写死——D1 那条「三套品牌色并存」就是写死色值造成的。 */
.hint.ok {
  color: var(--success, #6a9955);
}

.hint.warn {
  color: var(--warn, #dcdcaa);
}

.hint.error {
  color: var(--msg-error, #f48771);
}

.hint.dim {
  color: var(--text-secondary);
}
</style>
