<script setup lang="ts">
/**
 * 日志查看。
 *
 * 对应 V1 `main.py:_build_logs_tab`：
 *   · 等宽字体的日志文本区
 *   · **三色标签**：info 灰 / warn 橙 / error 红（V1 用 Text 的 tag 实现）
 *   · 底部三个按钮：📂 打开日志文件夹、🔄 刷新、🗑 删除日志
 *
 * 数据来自 `GET /api/logs` → `{ lines, dir, files }`。
 * 另外订阅 SSE 的 `log.appended` 实时追加——V1 是轮询，这里改成推送（ADR-016），
 * 行为一致但不再有轮询延迟。
 */
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { api, ApiError } from '../../api/client'
import { onEvent } from '../../composables/useEvents'
import { showNotice } from '../../composables/useNotice'
import type { Envelope } from '../../api/types'

const lines = ref<string[]>([])
const dir = ref('')
const files = ref<string[]>([])
const loading = ref(false)
const autoScroll = ref(true)

const viewer = ref<HTMLElement | null>(null)

/**
 * 判定行的级别。
 * 日志格式：`2026-09-11 16:20:07 [SYS] [INFO] 翻译器启动 | …`
 * 级别出现在第二个方括号里，所以按 `[LEVEL]` 直接匹配即可。
 */
function levelOf(line: string): 'info' | 'warn' | 'error' {
  if (line.includes('[ERROR]')) return 'error'
  if (line.includes('[WARN]')) return 'warn'
  return 'info'
}

const rendered = computed(() => lines.value.map((text) => ({ text, level: levelOf(text) })))

async function reload(): Promise<void> {
  loading.value = true
  try {
    const r = await api.getLogs(300)
    lines.value = r.lines ?? []
    dir.value = r.dir ?? ''
    files.value = r.files ?? []
    scrollToEnd()
  } catch (e) {
    showNotice(`读取日志失败：${e instanceof ApiError ? e.message : String(e)}`, 'error', 4000)
  } finally {
    loading.value = false
  }
}

async function removeAll(): Promise<void> {
  if (!window.confirm('确定删除全部日志？此操作不可撤销。')) return
  try {
    const r = await api.deleteLogs()
    showNotice(`已删除 ${r.deleted} 个日志文件`, 'info', 2500)
    await reload()
  } catch (e) {
    showNotice(`删除失败：${e instanceof ApiError ? e.message : String(e)}`, 'error', 4000)
  }
}

/** 在文件管理器里打开日志目录（V1 的 📂 打开日志文件夹） */
async function revealDir(): Promise<void> {
  try {
    await api.revealLogs()
  } catch (e) {
    showNotice(`打开目录失败：${e instanceof ApiError ? e.message : String(e)}`, 'error', 4000)
  }
}

function scrollToEnd(): void {
  if (!autoScroll.value) return
  requestAnimationFrame(() => {
    const el = viewer.value
    if (el) el.scrollTop = el.scrollHeight
  })
}

const disposers: Array<() => void> = []

function handleEvent(env: Envelope): void {
  if (env.type !== 'log.appended') return
  const p = env.payload as { level?: string; module?: string; text?: string }
  lines.value = [...lines.value, `[${p.level ?? '?'}] [${p.module ?? '?'}] ${p.text ?? ''}`]
  scrollToEnd()
}

onMounted(async () => {
  disposers.push(onEvent(handleEvent))
  await reload()
})

onBeforeUnmount(() => {
  for (const d of disposers) d()
  disposers.length = 0
})
</script>

<template>
  <div class="tab">
    <div class="toolbar">
      <button class="btn" :disabled="loading" @click="reload">🔄 刷新</button>
      <button class="btn" @click="revealDir">📂 打开日志文件夹</button>
      <span class="spacer" />
      <label class="check">
        <input v-model="autoScroll" type="checkbox" />
        <span>自动滚动</span>
      </label>
      <button class="btn danger" @click="removeAll">🗑 删除日志</button>
    </div>

    <div class="path">
      {{ dir || '（未知目录）' }}
      <span class="count">{{ lines.length }} 行 / {{ files.length }} 个文件</span>
    </div>

    <div ref="viewer" class="viewer">
      <p v-if="rendered.length === 0" class="empty">暂无日志</p>
      <div v-for="(l, i) in rendered" :key="i" class="line" :class="l.level">{{ l.text }}</div>
    </div>
  </div>
</template>

<style scoped>
.tab {
  display: flex;
  flex-direction: column;
  gap: 8px;
  height: 100%;
}

.toolbar {
  display: flex;
  align-items: center;
  gap: 8px;
}

.spacer {
  flex: 1 1 auto;
}

.btn {
  padding: 5px 12px;
  font-size: 12px;
  border-radius: var(--radius-control);
  background: var(--layer-control);
  color: var(--text);
  border: 1px solid var(--stroke);
  transition: background var(--motion-fast);
}

.btn:hover:not(:disabled) {
  background: var(--layer-control-hover);
}

.btn:disabled {
  color: var(--text-disabled);
  cursor: default;
}

.btn.danger:hover {
  color: var(--msg-error);
}

.check {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  font-size: 11px;
  color: var(--text-tertiary);
}

.check input {
  accent-color: var(--accent);
}

.path {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
  gap: 12px;
  font-size: 11px;
  color: var(--text-tertiary);
  word-break: break-all;
}

.count {
  flex: none;
}

.viewer {
  flex: 1 1 auto;
  min-height: 260px;
  max-height: 54vh;
  overflow: auto;
  padding: 8px 10px;
  background: var(--layer-control);
  border: 1px solid var(--stroke);
  border-radius: var(--radius-card);
  font-family: var(--font-mono);
  font-size: 11px;
  line-height: 1.55;
  user-select: text;
}

.line {
  white-space: pre-wrap;
  word-break: break-all;
}

/* 三色标签，取值与 V1 的 tag_configure 对齐 */
.line.info {
  color: var(--text-secondary);
}

.line.warn {
  color: #f59e0b;
}

.line.error {
  color: #ef4444;
}

.empty {
  margin: 0;
  color: var(--text-tertiary);
  font-family: var(--font-ui);
}
</style>
