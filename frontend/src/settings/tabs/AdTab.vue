<script setup lang="ts">
/**
 * 广告发送。
 *
 * 对应 V1 `main.py:_build_ad_tab`（约 250 行）+ 状态机
 * `_ad_start` / `_ad_stop` / `_ad_schedule_tick` / `_ad_advance_index` /
 * `_ad_highlight` / `_ad_send_current`。
 *
 * 结构与 V1 一致：
 *   使用说明卡片（含红色警告）→ 基本设置（倒计时/剩余秒数/当前发送）
 *   → **固定 5 条**带彩色圆点的消息框 → ▶开始 / ■暂停 → 发送日志
 *
 * 几处照抄 V1 的行为细节，别「优化」掉：
 *   · 倒计时单位是**分钟**，上限 1440（24 小时）
 *   · 跳过**空**消息：轮转时只会停在有内容的槽位
 *   · 当前条高亮 = 浅黄底 + 圆点变 ▶
 *   · 发送的是**原文**，不做翻译（V1 调 send_chat_message(text, "y")）
 *
 * ✅ 发送链路**已实现**：native 的输入模拟（`input.send`）与 Go 的
 * `internal/task.AdSender` 都已落地并验证。这里曾挂着一句「发送链路尚未实现」
 * 的横幅，实现之后忘了撤——界面说谎比缺说明更坏，用户会照着它排查不存在的问题。
 *
 * ⚠️ 本组件**不持有任何定时器**。状态机（倒计时、轮转、发送）全在 Go 侧：
 * 这里只通过 /api/ad/* 下命令、通过 SSE 的 ad.status / ad.log 显示结果。
 * 它曾经自己跑一套 setTimeout 循环，于是下面那句「关掉设置界面也不会中断」
 * 与事实正好相反——关掉页面 Vue 卸载组件、计时器被清掉，发送立刻就停了，
 * 等于把 V1 的 D17 缺陷原样换个地方复现。
 */
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { api, ApiError } from '../../api/client'
import type { AdLogPayload, AdStatusPayload } from '../../api/types'
import { currentConfig, patchConfig } from '../../composables/useConfig'
import { onEvent, onResync } from '../../composables/useEvents'


/** V1 固定 5 条消息槽 */
const SLOTS = 5
/** V1 的圆点配色 */
const DOT_COLORS = ['#e74c3c', '#e67e22', '#3cb371', '#4090e0', '#9b59b6']

const cfg = computed(() => currentConfig())

/** 补齐/截断到 5 个槽位，保证 v-for 稳定 */
const messages = computed<string[]>(() => {
  const list = cfg.value?.ad_messages ?? []
  return Array.from({ length: SLOTS }, (_, i) => list[i] ?? '')
})

const countdownText = computed({
  get: () => cfg.value?.ad_countdown ?? '5',
  set: (v: string) => setCountdown(v),
})

function setCountdown(v: string): void {
  patchConfig({ ad_countdown: v })
}

function setMessage(i: number, text: string): void {
  const list = [...messages.value]
  list[i] = text
  patchConfig({ ad_messages: list })
}

// ── 运行状态（与 V1 的同名变量一一对应）──
//
// ⚠️ 这几个全是**只读镜像**：真正的状态机在 Go 的 internal/task.AdSender，
// 这里只显示它推过来的快照，不自己算倒计时。
const running = ref(false)
const remaining = ref(0)
const currentIndex = ref(-1)
const adLog = ref<string[]>([])

function appendLog(text: string): void {
  const ts = new Date().toLocaleTimeString('zh-CN', { hour12: false })
  // 只保留最近 200 条，长时间运行不至于把内存吃光
  adLog.value = [...adLog.value.slice(-199), `${ts}  ${text}`]
}

/** 校验倒计时。错误文案照抄 V1 `_ad_validate_countdown`。 */
function validateCountdown(text: string): string | null {
  if (!text) return '请输入倒计时时间'
  const n = Number(text)
  if (!Number.isInteger(n)) return '请输入有效的整数'
  if (n <= 0) return '倒计时必须大于 0 分钟'
  if (n > 1440) return '倒计时不能超过 1440 分钟（24 小时）'
  return null
}

// ── 与 Go 状态机的连接 ──

/** 用一份状态快照刷新界面。 */
function applyStatus(st: AdStatusPayload): void {
  running.value = st.running
  currentIndex.value = st.currentIndex
  remaining.value = st.remainingSec
}

/**
 * 拉一次快照。
 * 首屏要拉，SSE 每次重连后也要拉——重连不会补发断线期间丢过的事件。
 */
async function refresh(): Promise<void> {
  try {
    applyStatus(await api.adStatus())
  } catch {
    // 广告能力未就绪（后端 s.opts.Ad == nil）时保持默认显示，
    // 不用一个红条去打扰用户：这时候他本来也没法开始发送。
  }
}

const offs: Array<() => void> = []

onMounted(() => {
  offs.push(
    onEvent((env) => {
      if (env.type === 'ad.status') {
        applyStatus(env.payload as AdStatusPayload)
      } else if (env.type === 'ad.log') {
        // 时间戳在收到的那一刻打，与 V1 的日志格式一致
        appendLog((env.payload as AdLogPayload).text)
      }
    }),
  )
  offs.push(onResync(() => void refresh()))
  void refresh()
})

onBeforeUnmount(() => {
  // 只退订，不动状态机——发送由 Go 侧继续驱动
  for (const off of offs) off()
})

/**
 * 开始。
 *
 * 本地先校验一遍，是为了保住 V1 的错误文案（`_ad_validate_countdown` 的
 * 那几句是固定的，Go 侧不逐字复制它们）；真要漏过去，Go 的 Start() 也会
 * 拒绝，下面 catch 里照实显示，不会静默什么都不做。
 */
async function start(): Promise<void> {
  if (!messages.value.some((m) => m.trim())) {
    appendLog('错误: 请至少填写一条广告消息')
    return
  }
  const err = validateCountdown(countdownText.value)
  if (err) {
    appendLog(`错误: ${err}`)
    return
  }

  try {
    // 把屏幕上的内容一起送过去：V1 的 `_ad_start` 直接读 Entry 控件，
    // 「改完不点保存就点开始」用的也是这份内容。只读已保存的配置会静默
    // 发送上一次保存的旧消息——界面显示新文本、发出去的是旧的。
    applyStatus(await api.adStart(messages.value, countdownText.value))
    // 「开始发送，间隔 N 分钟」由 Go 侧记进 ad.log 推回来，不在这里重复记
  } catch (e) {
    appendLog(`错误: ${e instanceof ApiError ? e.message : String(e)}`)
  }
}

/** 暂停。 */
async function stop(): Promise<void> {
  try {
    applyStatus(await api.adStop())
  } catch (e) {
    appendLog(`错误: ${e instanceof ApiError ? e.message : String(e)}`)
  }
}

const currentLabel = computed(() =>
  running.value && currentIndex.value >= 0 ? `消息 ${currentIndex.value + 1}` : '（未启动）',
)

/** V1 的使用说明原文 */
const HELP_TEXT = [
  'TMP广告软件 使用说明:',
  '',
  '1.发送快捷键:Y(固定)',
  '2.发送按键:Enter(固定)',
  '3.倒计时(分钟):设置每次发送消息的间隔时间',
  '4.发送消息1-5:设置需要循环发送的广告消息',
  '5.当前发送:显示正在发送第几条消息(黄色高亮)',
  '6.发送日志:记录每次发送的时间和内容',
  '7.开始按钮:开始自动发送消息',
  '8.暂停按钮:停止自动发送消息',
  '',
  '使用步骤:',
  '1.填写消息内容(需要发送的广告)',
  '2.设置倒计时时间(分钟)',
  '3.点击开始按钮,软件会按1、2、3、4、5、1的顺序循环发送消息',
  '4.点击暂停按钮停止发送',
  '',
  '注意:请在安全的环境中使用本软件,遵守当地法律法规。',
].join('\n')
</script>

<template>
  <div class="tab">
    <!-- ── 使用说明 ── -->
    <section class="card">
      <pre class="help">{{ HELP_TEXT }}</pre>
      <!-- V1 这里写的是「⚠ 使用广告发送请不要关闭设置界面」——那是缺陷 D17：
           它的状态机寄生在设置窗口的 tk after() 循环里，关窗即停。V2 已把状态机
           搬到 Go 侧（internal/task.AdSender），关掉窗口照常发送。
           那句警告留着会和实际行为正好相反，所以改成实话。 -->
      <p class="note">✓ 发送由后台持续驱动，关掉设置界面也不会中断。</p>
    </section>

    <!-- ── 基本设置 ── -->
    <section class="card">
      <h3>BASIC SETTINGS / 基本设置</h3>

      <div class="row">
        <label class="label">Countdown (min) / 倒计时(分钟)</label>
        <div class="control">
          <input v-model="countdownText" class="input narrow" type="number" min="1" max="1440" />
        </div>
      </div>

      <div class="row">
        <label class="label">Remaining (sec) / 剩余时间(秒)</label>
        <div class="control">
          <span class="remaining">{{ remaining }}</span>
        </div>
      </div>

      <div class="row">
        <label class="label">Current / 当前发送</label>
        <div class="control">
          <span class="current" :class="{ on: running }">{{ currentLabel }}</span>
        </div>
      </div>
    </section>

    <!-- ── 广告消息（固定 5 条）── -->
    <section class="card">
      <h3>AD MESSAGES / 广告消息</h3>

      <div v-for="(m, i) in messages" :key="i" class="msg-row">
        <span class="dot" :style="{ color: currentIndex === i ? 'var(--accent)' : DOT_COLORS[i] }">
          {{ currentIndex === i ? '▶' : '●' }}
        </span>
        <span class="msg-label">消息 {{ i + 1 }}</span>
        <textarea
          class="msg-area"
          :class="{ highlighted: currentIndex === i }"
          rows="2"
          :value="m"
          @input="setMessage(i, ($event.target as HTMLTextAreaElement).value)"
        />
      </div>
    </section>

    <!-- ── 开始 / 暂停 ── -->
    <div class="btn-row">
      <button class="big start" type="button" :disabled="running" @click="start">▶ 开始</button>
      <button class="big stop" type="button" :disabled="!running" @click="stop">■ 暂停</button>
    </div>

    <!-- ── 发送日志 ── -->
    <section class="card">
      <h3>SEND LOG / 发送日志</h3>
      <div class="ad-log">
        <p v-if="adLog.length === 0" class="empty">（暂无记录）</p>
        <div v-for="(l, i) in adLog" :key="i" class="log-line">{{ l }}</div>
      </div>
    </section>
  </div>
</template>

<style scoped>
.tab {
  display: flex;
  flex-direction: column;
  gap: 12px;
}

.card {
  background: var(--layer-card);
  border: 1px solid var(--stroke);
  border-radius: var(--radius-card);
  padding: 14px 18px 16px;
}

/* 说明区是个大段等宽文本，保持 V1 的原样换行 */
.help {
  margin: 0;
  font-family: var(--font-ui);
  font-size: 11px;
  line-height: 1.7;
  color: var(--text-secondary);
  white-space: pre-wrap;
  user-select: text;
}

.danger {
  margin: 10px 0 0;
  font-size: 11px;
  font-weight: 600;
  color: var(--msg-error);
}

.banner {
  padding: 9px 12px;
  border-radius: var(--radius-card);
  font-size: 12px;
  line-height: 1.6;
}

.banner.warn {
  background: color-mix(in srgb, var(--warn) 12%, transparent);
  border: 1px solid color-mix(in srgb, var(--warn) 40%, transparent);
  color: var(--warn);
}

.note {
  margin: 8px 0 0;
  font-size: 12px;
  color: var(--success, #6a9955);
}

h3 {
  margin: 0 0 10px;
  font-size: 11px;
  font-weight: 600;
  letter-spacing: 0.04em;
  color: var(--text-tertiary);
}

.row {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 5px 0;
}

.label {
  flex: none;
  width: 240px;
  font-size: 12px;
  color: var(--text);
}

.control {
  flex: 1 1 auto;
  display: flex;
  align-items: center;
  min-width: 0;
}

/* V1 的剩余秒数是 22px 加粗的强调色——它是这个页面的视觉焦点 */
.remaining {
  font-size: 22px;
  font-weight: bold;
  color: var(--accent);
  line-height: 1;
}

.current {
  font-size: 13px;
  font-weight: 600;
  color: var(--text-tertiary);
}

.current.on {
  color: var(--accent);
}

/* ── 消息槽 ── */
.msg-row {
  display: flex;
  align-items: flex-start;
  gap: 6px;
  padding: 3px 0;
}

.dot {
  flex: none;
  width: 16px;
  text-align: center;
  font-size: 12px;
  font-weight: bold;
  line-height: 26px;
}

.msg-label {
  flex: none;
  width: 52px;
  font-size: 12px;
  color: var(--text);
  line-height: 26px;
}

.msg-area {
  flex: 1 1 auto;
  min-width: 0;
  background: var(--layer-control);
  border: 1px solid var(--stroke);
  border-radius: var(--radius-control);
  color: var(--text);
  padding: 5px 8px;
  font-family: var(--font-ui);
  font-size: 12px;
  line-height: 1.5;
  resize: vertical;
  outline: none;
  user-select: text;
}

.msg-area:focus {
  border-color: var(--accent);
}

/* V1 的当前条高亮：浅黄底 */
.msg-area.highlighted {
  background: #fff3cd;
  color: #1a1a1a;
  border-color: var(--accent);
}

/* ── 开始 / 暂停 ── */
.btn-row {
  display: flex;
  gap: 12px;
}

.big {
  padding: 9px 34px;
  font-size: 13px;
  font-weight: 600;
  border-radius: var(--radius-control);
  color: #ffffff;
  transition: background var(--motion-fast);
}

.big.start {
  background: #3cb371;
}

.big.start:hover:not(:disabled) {
  background: #2e965a;
}

.big.stop {
  background: #e74c3c;
}

.big.stop:hover:not(:disabled) {
  background: #c0392b;
}

.big:disabled {
  background: var(--layer-control);
  color: var(--text-disabled);
  cursor: default;
}

/* ── 发送日志 ── */
.ad-log {
  max-height: 190px;
  overflow-y: auto;
  padding: 8px 10px;
  background: var(--layer-control);
  border: 1px solid var(--stroke);
  border-radius: var(--radius-control);
  font-family: var(--font-mono);
  font-size: 11px;
  line-height: 1.6;
  color: var(--text-secondary);
  user-select: text;
}

.log-line {
  white-space: pre-wrap;
  word-break: break-all;
}

.empty {
  margin: 0;
  color: var(--text-tertiary);
  font-family: var(--font-ui);
}

code {
  background: var(--layer-control);
  padding: 1px 4px;
  border-radius: 3px;
  font-size: 11px;
}
</style>
