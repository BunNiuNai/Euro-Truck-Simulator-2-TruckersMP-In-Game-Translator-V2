<script setup lang="ts">
/**
 * 设置窗口根组件 —— 布局对齐 Win11「设置」应用。
 *
 * V1 main.py:SettingsDialog 是 5 个**顶部 tab**（_build_api_tab / _build_hotkeys_tab /
 * _build_appearance_tab / _build_logs_tab / _build_ad_tab）。V2 改成 Win11 的
 * **左侧导航 + 右侧内容**：这是 Win11 设置应用的标准形态，也是「模仿 win11」
 * 最直观的一处。功能项与 V1 一一对应，只是摆放方式变了。
 *
 * ⚠️ 与悬浮窗是两个独立窗口、两个独立 JS 环境：它们**不能共享内存状态**，
 * 各自从 Go 拉取、各自订阅 SSE。设置保存后由 Go 广播 config.reloaded，
 * 悬浮窗据此重新拉配置——这是两个窗口之间唯一的同步途径。
 */
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { ApiError } from '../api/client'
import { onEvent, onResync } from '../composables/useEvents'
import { currentConfig, loadConfig, reloadConfig, saveConfig, useConfig } from '../composables/useConfig'
import { showNotice, useNotice } from '../composables/useNotice'
import { refreshHealth, useHealth } from '../composables/useHealth'
import type { Envelope } from '../api/types'
import ApiTab from './tabs/ApiTab.vue'
import HotkeysTab from './tabs/HotkeysTab.vue'
import AppearanceTab from './tabs/AppearanceTab.vue'
import LogsTab from './tabs/LogsTab.vue'
import AdTab from './tabs/AdTab.vue'
import Credits from './components/Credits.vue'

type TabKey = 'api' | 'hotkeys' | 'appearance' | 'logs' | 'ad'

/** 导航项。图标码位来自 Segoe Fluent Icons，字体缺失时退化为空白。 */
const navItems: Array<{ key: TabKey; label: string; icon: string; desc: string }> = [
  { key: 'api', label: 'API / Provider', icon: '\uE774', desc: '翻译服务与密钥' },
  { key: 'hotkeys', label: '热键', icon: '\uE765', desc: '呼出与发送' },
  { key: 'appearance', label: '外观', icon: '\uE790', desc: '主题、字号、窗口' },
  { key: 'logs', label: '日志', icon: '\uE9D9', desc: '运行记录' },
  { key: 'ad', label: '广告发送', icon: '\uE8BD', desc: '循环发送内容' },
]

const active = ref<TabKey>('api')
const { config, saving, dirty } = useConfig()
const { notice } = useNotice()
const { health } = useHealth()

const activeItem = computed(() => navItems.find((i) => i.key === active.value))

/**
 * 保存。
 *
 * 脏标记与脏字段由 useConfig 管：saveConfig 成功后自己清（见那里的注释），
 * 这里只负责提示与错误处理。
 */
async function onSave(): Promise<void> {
  const cfg = currentConfig()
  if (!cfg) return
  try {
    await saveConfig({ ...cfg })
    showNotice('配置已保存', 'info', 2000)
  } catch (e) {
    const msg = e instanceof ApiError ? e.message : String(e)
    showNotice(`保存失败：${msg}`, 'error', 5000)
  }
}

/**
 * 重新加载：丢掉未保存的改动，从 Go 拉一份干净的。
 *
 * 必须用 reloadConfig 而不是 loadConfig —— 后者会把「用户改过但没保存」的
 * 字段合并回来（见 useConfig.mergeDirty），那样「重新加载」等于什么都没做。
 */
async function onReload(): Promise<void> {
  try {
    await reloadConfig()
    showNotice('已重新加载', 'info', 1500)
  } catch (e) {
    showNotice(`加载失败：${e instanceof Error ? e.message : String(e)}`, 'error', 5000)
  }
}

/**
 * 取消：放弃未保存的改动（从 Go 重新拉一份覆盖本地副本）。
 * V1 的 Cancel 是关掉对话框；V2 的设置是独立窗口，语义对应为「丢弃改动」。
 */
async function onCancel(): Promise<void> {
  await onReload()
  showNotice('已放弃未保存的改动', 'info', 1500)
}

// ── 事件 ──
const disposers: Array<() => void> = []

function handleEvent(env: Envelope): void {
  switch (env.type) {
    case 'config.reloaded':
      // 可能是另一个窗口改的配置，重新拉一次保持一致。
      // ⚠️ 这里**不能**顺手加 clearDirty：拉取时 mergeDirty 会保留用户改过、
      // 还没保存的那些字段（没碰过的字段照常跟随后端），脏标记也必须留着——
      // 用户改了一半的东西不能因为别处保存了一次配置就被吃掉。
      void loadConfig()
      break
    case 'log.appended': {
      const p = env.payload as { level: string; text: string }
      if (p.level === 'ERROR' || p.level === 'WARN') showNotice(p.text, p.level.toLowerCase(), 4000)
      break
    }
    case 'notice': {
      const p = env.payload as { text: string; level: string }
      showNotice(p.text, p.level)
      break
    }
    default:
      break
  }
}

async function resync(): Promise<void> {
  await Promise.allSettled([loadConfig(), refreshHealth()])
}

onMounted(async () => {
  disposers.push(onEvent(handleEvent))
  disposers.push(onResync(resync))
  await resync()
})

onBeforeUnmount(() => {
  for (const d of disposers) d()
  disposers.length = 0
})

/** 顶部状态摘要：目标语言、Provider 数、native 是否在线。 */
const summary = computed(() => {
  const h = health.value
  if (!h) return '连接中…'
  return [
    `目标语言 ${h.targetLang}`,
    `Provider ${h.providers.enabled}/${h.providers.total}`,
    h.nativeAlive ? 'native 在线' : 'native 离线',
  ].join('  ·  ')
})
</script>

<template>
  <div class="settings">
    <!-- 标题带：与悬浮窗同高，将来由 native 接管拖拽 -->
    <header class="titlebar">
      <h1>设置</h1>
      <span class="summary">{{ summary }}</span>
    </header>

    <div class="body">
      <!-- 左侧导航 -->
      <nav class="nav">
        <button
          v-for="item in navItems"
          :key="item.key"
          class="nav-item"
          :class="{ active: active === item.key }"
          @click="active = item.key"
        >
          <span class="indicator" />
          <span class="icon" aria-hidden="true">{{ item.icon }}</span>
          <span class="labels">
            <span class="name">{{ item.label }}</span>
            <span class="desc">{{ item.desc }}</span>
          </span>
        </button>
      </nav>

      <!-- 右侧内容 -->
      <main class="content">
        <div v-if="!config" class="loading">加载配置中…</div>
        <template v-else>
          <h2 class="page-title">{{ activeItem?.label }}</h2>
          <!-- 各 tab 里的「改了哪些字段」由 useConfig 的 patchConfig 直接登记，
               底部那个 dirty 就是它的派生值，所以这里不再需要监听 tab 的
               change 事件（以前那个布尔标记只知道「脏了」，说不出**哪些**字段
               脏了，config.reloaded 来的时候就没法只保留脏字段）。 -->
          <ApiTab v-show="active === 'api'" />
          <HotkeysTab v-show="active === 'hotkeys'" />
          <AppearanceTab v-show="active === 'appearance'" />
          <LogsTab v-show="active === 'logs'" />
          <AdTab v-show="active === 'ad'" />
        </template>
      </main>
    </div>

    <!-- 底部操作条 -->
    <footer class="foot">
      <span v-if="notice" class="notice" :class="notice.level">{{ notice.text }}</span>
      <span v-else-if="dirty" class="state dirty">有未保存的改动</span>
      <span v-else class="state">已同步</span>

      <div class="actions">
        <!-- V1 是「Cancel / 取消」+「Save / 保存」两个按钮 -->
        <button class="btn" :disabled="saving || !dirty" @click="onCancel">Cancel / 取消</button>
        <button class="btn primary" :disabled="saving || !dirty" @click="onSave">
          {{ saving ? '保存中…' : 'Save / 保存' }}
        </button>
      </div>
    </footer>

    <!-- 鸣谢与链接（V1 放在设置对话框最底部、按钮下方） -->
    <Credits class="credits" />
  </div>
</template>

<style scoped>
.settings {
  height: 100%;
  display: flex;
  flex-direction: column;
  background: var(--layer-bg);
  user-select: none;
}

/* ── 标题带 ── */
.titlebar {
  flex: none;
  height: var(--caption-height);
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  padding: 0 16px;
}

h1 {
  margin: 0;
  font-size: 13px;
  font-weight: 600;
  color: var(--text);
}

.summary {
  font-size: 11px;
  color: var(--text-tertiary);
}

/* ── 主体：左导航 + 右内容 ── */
.body {
  flex: 1 1 auto;
  min-height: 0;
  display: flex;
}

.nav {
  flex: none;
  width: 208px;
  padding: 4px 8px;
  display: flex;
  flex-direction: column;
  gap: 2px;
  overflow-y: auto;
}

/* Win11 的导航项：选中时左侧一条圆角强调色竖条 + 背景高亮 */
.nav-item {
  position: relative;
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 7px 10px 7px 14px;
  border-radius: var(--radius-control);
  text-align: left;
  color: var(--text);
  transition: background var(--motion-fast);
}

.nav-item:hover {
  background: var(--layer-control-hover);
}

.nav-item.active {
  background: var(--layer-control);
}

.indicator {
  position: absolute;
  left: 4px;
  top: 50%;
  transform: translateY(-50%) scaleY(0);
  width: 3px;
  height: 16px;
  border-radius: 2px;
  background: var(--accent);
  transition: transform var(--motion-normal);
}

.nav-item.active .indicator {
  transform: translateY(-50%) scaleY(1);
}

.icon {
  flex: none;
  width: 16px;
  font-family: 'Segoe Fluent Icons', 'Segoe MDL2 Assets', var(--font-ui);
  font-size: 14px;
  text-align: center;
  color: var(--text-secondary);
}

.nav-item.active .icon {
  color: var(--accent);
}

.labels {
  display: flex;
  flex-direction: column;
  min-width: 0;
}

.name {
  font-size: 12px;
  line-height: 1.35;
}

.desc {
  font-size: 10px;
  color: var(--text-tertiary);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.content {
  flex: 1 1 auto;
  min-width: 0;
  overflow-y: auto;
  padding: 6px 20px 20px 12px;
  /* 内容区与导航区用一条极淡的分隔，而不是硬边框 */
  border-left: 1px solid var(--stroke);
}

.page-title {
  margin: 4px 0 14px;
  font-size: 20px;
  font-weight: 600;
  color: var(--text);
}

.loading {
  color: var(--text-secondary);
  padding: 20px 0;
}

/* ── 底部操作条 ── */
.foot {
  flex: none;
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  padding: 10px 16px;
  border-top: 1px solid var(--stroke);
  background: var(--layer-card);
}

/* 鸣谢区固定在底部按钮下方（V1 也是这个位置），字号压小以免占太多竖向空间 */
.credits {
  flex: none;
  padding: 6px 16px 10px;
  border-top: 1px solid var(--stroke);
  background: var(--layer-card);
}

.notice {
  font-size: 12px;
}

.notice.error {
  color: var(--msg-error);
}

.notice.warn {
  color: var(--warn);
}

.notice.info {
  color: var(--ok);
}

.state {
  font-size: 11px;
  color: var(--text-tertiary);
}

.state.dirty {
  color: var(--warn);
}

.actions {
  display: flex;
  gap: 8px;
}

/* Win11 按钮：实色填充 + 1px 描边，悬停时底色抬升一档 */
.btn {
  min-width: 88px;
  padding: 5px 16px;
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

.btn:active:not(:disabled) {
  background: var(--layer-control-pressed);
}

.btn:disabled {
  color: var(--text-disabled);
  background: var(--layer-card);
  cursor: default;
}

.btn.primary {
  background: var(--accent);
  border-color: transparent;
  color: var(--accent-on);
  font-weight: 600;
}

.btn.primary:hover:not(:disabled) {
  background: var(--accent-hover);
}

.btn.primary:active:not(:disabled) {
  background: var(--accent-pressed);
}

.btn.primary:disabled {
  background: var(--layer-card);
  color: var(--text-disabled);
}
</style>
