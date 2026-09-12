<script setup lang="ts">
/**
 * 热键设置。
 *
 * 对应 V1 `main.py:_build_hotkeys_tab`（3 行）+ `HotkeyCapture` 控件。
 *
 * ⚠️ 只暴露 **3 个**热键，与 V1 一致：
 *   Copy Hotkey  → copy_hotkey   复制译文
 *   Send Hotkey  → enter_hotkey  发送（回车确认）
 *   Focus Key    → send_hotkey   呼出输入框
 *
 * V1 的 config 里还有 chat_hotkey / paste_hotkey 两个字段，但设置界面
 * **没有**暴露它们（缺陷 D7 记录的四处热键解析器也与此有关）。
 * 早期版本我按 config 字段做了 5 个，比 V1 多，现已对齐。
 *
 * ✅ 这些热键**已经生效**：native 的 `native/hotkey` 已实现并验证
 * （`RegisterHotKey` 为主、被占用时降级为轮询，ADR-015 / Q9）。
 * 曾经这里挂着一句「该模块尚未实现，配置的热键暂时不会生效」——那时是真的，
 * 但实现之后忘了改，界面就一直在说谎。这类过期文案比缺文案更坏：用户会照着
 * 它去排查一个根本不存在的问题。
 */
import { computed } from 'vue'
import { currentConfig, patchConfig } from '../../composables/useConfig'
import HotkeyCapture from '../components/HotkeyCapture.vue'


const cfg = computed(() => currentConfig())

/** 标签与绑定的字段严格照抄 V1 —— 名称容易混，不能凭语义猜 */
const items = [
  { key: 'copy_hotkey', label: 'Copy Hotkey / 复制', desc: '复制当前译文' },
  { key: 'enter_hotkey', label: 'Send Hotkey / 发送', desc: '发送（回车确认）' },
  { key: 'send_hotkey', label: 'Focus Key / 呼出输入框', desc: '把焦点移到输入框' },
] as const

type HotkeyKey = (typeof items)[number]['key']

function valueOf(key: HotkeyKey): string {
  return cfg.value ? (cfg.value[key] as string) : ''
}

function setValue(key: HotkeyKey, v: string): void {
  patchConfig({ [key]: v } as never)
}
</script>

<template>
  <div class="tab">
    <div class="banner info">
      全局热键由 native 层注册（<code>RegisterHotKey</code>，ADR-015）。
      若某个组合已被别的程序占用，会自动降级为轮询，并在启动日志里如实列出降级项。
    </div>

    <section class="card">
      <h3>HOTKEYS / 快捷键</h3>

      <div v-for="it in items" :key="it.key" class="row">
        <label class="label">{{ it.label }}</label>
        <div class="control">
          <HotkeyCapture
            :model-value="valueOf(it.key)"
            @update:model-value="setValue(it.key, $event)"
          />
          <span class="hint">{{ it.desc }}</span>
        </div>
      </div>

      <!-- V1 也把这句提示放在热键卡片里，且用红色 -->
      <p class="capture-hint">按下组合键进行捕获</p>
    </section>
  </div>
</template>

<style scoped>
.tab {
  display: flex;
  flex-direction: column;
  gap: 14px;
}

.banner {
  padding: 9px 12px;
  border-radius: var(--radius-card);
  font-size: 12px;
  line-height: 1.6;
}

.banner.info {
  background: color-mix(in srgb, var(--accent) 12%, transparent);
  border: 1px solid color-mix(in srgb, var(--accent) 40%, transparent);
  color: var(--text);
}

.banner.warn {
  background: color-mix(in srgb, var(--warn) 12%, transparent);
  border: 1px solid color-mix(in srgb, var(--warn) 40%, transparent);
  color: var(--warn);
}

.card {
  background: var(--layer-card);
  border: 1px solid var(--stroke);
  border-radius: var(--radius-card);
  padding: 14px 18px 16px;
}

h3 {
  margin: 0 0 10px;
  font-size: 14px;
  font-weight: 600;
  color: var(--text);
}

.row {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 6px 0;
}

.label {
  flex: none;
  width: 176px;
  font-size: 12px;
  color: var(--text);
}

.control {
  flex: 1 1 auto;
  display: flex;
  align-items: center;
  gap: 10px;
  min-width: 0;
}

.hint {
  font-size: 11px;
  color: var(--text-tertiary);
}

.capture-hint {
  margin: 10px 0 0;
  font-size: 11px;
  color: var(--msg-error);
}

code {
  background: var(--layer-control);
  padding: 1px 4px;
  border-radius: 3px;
  font-size: 11px;
}
</style>
