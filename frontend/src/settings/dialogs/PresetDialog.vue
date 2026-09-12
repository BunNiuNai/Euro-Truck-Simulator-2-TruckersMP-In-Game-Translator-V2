<script setup lang="ts">
/**
 * 预设选择对话框。
 *
 * 对应 V1 settings_ui.py:266-467 的 `PresetSelectorDialog`：
 * 搜索框 → 按分类分组的预设卡片 → 「手动输入」卡片 → 取消。
 *
 * 数据来自 `GET /api/presets`（后端的 resources/providers.json，20 个预设、
 * 4 个分类、自定义图标与配色）。
 *
 * ⚠️ 一个容易漏掉的细节（V1 provider_presets.py:28-33）：
 * 预设里存的是 **base URL**（如 `https://api.siliconflow.cn`），
 * 而 Provider 配置要的是**完整端点**。必须按 apiFormat 拼出路径，
 * 否则请求会打到根路径上。
 */
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { api, ApiError } from '../../api/client'
import type { Preset, PresetsPayload, Provider } from '../../api/types'

const emit = defineEmits<{
  (e: 'pick', provider: Provider): void
  (e: 'manual'): void
  (e: 'close'): void
}>()

const data = ref<PresetsPayload | null>(null)
const error = ref('')
const keyword = ref('')

/** 与 V1 provider_presets.build_endpoint 一致 */
function buildEndpoint(p: Preset): string {
  const base = (p.endpoint ?? '').replace(/\/+$/, '')
  return p.apiFormat === 'anthropic' ? `${base}/v1/messages` : `${base}/v1/chat/completions`
}

function matches(p: Preset): boolean {
  const k = keyword.value.trim().toLowerCase()
  if (!k) return true
  return (
    p.name.toLowerCase().includes(k) ||
    (p.description ?? '').toLowerCase().includes(k) ||
    p.id.toLowerCase().includes(k)
  )
}

/** 按分类分组，保持 categories 里的顺序（V1 也是这个顺序） */
const groups = computed(() => {
  const d = data.value
  if (!d) return []
  const cats = d.categories ?? []
  return cats
    .map((c) => ({
      id: c.id,
      label: c.label,
      items: (d.presets ?? []).filter((p) => p.category === c.id && matches(p)),
    }))
    .filter((g) => g.items.length > 0)
})

/** 不在任何已知分类里的预设也要显示出来，否则会「凭空消失」 */
const orphans = computed(() => {
  const d = data.value
  if (!d) return []
  const known = new Set((d.categories ?? []).map((c) => c.id))
  return (d.presets ?? []).filter((p) => !known.has(p.category) && matches(p))
})

const total = computed(() => groups.value.reduce((n, g) => n + g.items.length, 0) + orphans.value.length)

function iconOf(p: Preset): string {
  const d = data.value
  return d?.icons?.[p.icon] ?? '🔌'
}

function pick(p: Preset): void {
  // 字段映射与 V1 provider_presets.to_provider_dict 一致。
  // weight/timeout 的默认值也是 V1 的值（100 / 8），不是随手填的。
  emit('pick', {
    id: p.id,
    label: p.name,
    endpoint: buildEndpoint(p),
    api_key: '',
    model: p.defaultModel,
    enabled: true,
    preset_id: p.id,
    icon: p.icon,
    api_format: p.apiFormat,
    weight: 100,
    extra_headers: { ...(p.templateHeaders ?? {}) },
    extra_body: { ...(p.templateBody ?? {}) },
    timeout: 8,
  })
}

onMounted(async () => {
  try {
    data.value = await api.getPresets()
  } catch (e) {
    error.value = e instanceof ApiError ? e.message : String(e)
  }
  window.addEventListener('keydown', onKey)
})

onBeforeUnmount(() => window.removeEventListener('keydown', onKey))

function onKey(e: KeyboardEvent): void {
  if (e.key === 'Escape') emit('close')
}
</script>

<template>
  <div class="mask" @click.self="emit('close')">
    <div class="dialog" role="dialog" aria-label="选择预设">
      <header class="head">
        <h2>📦 选择预设</h2>
        <button class="x" type="button" title="关闭" @click="emit('close')">✕</button>
      </header>

      <div class="search-row">
        <span class="search-icon">🔍</span>
        <input
          v-model="keyword"
          class="search"
          type="text"
          placeholder="搜索服务商名称或描述…"
        />
        <span class="count">{{ total }} 个</span>
      </div>

      <div class="scroll">
        <p v-if="error" class="err">加载预设失败：{{ error }}</p>
        <p v-else-if="!data" class="hint">加载中…</p>
        <p v-else-if="total === 0" class="hint">没有匹配的预设</p>

        <section v-for="g in groups" :key="g.id" class="group">
          <h3 class="group-title">{{ g.label }}</h3>
          <div class="grid">
            <button v-for="p in g.items" :key="p.id" class="card" type="button" @click="pick(p)">
              <span class="icon" :style="{ background: p.iconColor || 'transparent' }">
                {{ iconOf(p) }}
              </span>
              <span class="meta">
                <span class="name">
                  {{ p.name }}
                  <span v-if="p.recommended" class="rec">推荐</span>
                </span>
                <span class="desc">{{ p.description }}</span>
              </span>
            </button>
          </div>
        </section>

        <section v-if="orphans.length" class="group">
          <h3 class="group-title">其他</h3>
          <div class="grid">
            <button v-for="p in orphans" :key="p.id" class="card" type="button" @click="pick(p)">
              <span class="icon" :style="{ background: p.iconColor || 'transparent' }">
                {{ iconOf(p) }}
              </span>
              <span class="meta">
                <span class="name">{{ p.name }}</span>
                <span class="desc">{{ p.description }}</span>
              </span>
            </button>
          </div>
        </section>

        <!-- 手动输入：V1 也把它放在预设列表的最后 -->
        <section class="group">
          <h3 class="group-title">自定义</h3>
          <div class="grid">
            <button class="card manual" type="button" @click="emit('manual')">
              <span class="icon">✏️</span>
              <span class="meta">
                <span class="name">手动输入</span>
                <span class="desc">不匹配任何预设，手动填写所有字段</span>
              </span>
            </button>
          </div>
        </section>
      </div>

      <footer class="foot">
        <button class="btn" type="button" @click="emit('close')">取消</button>
      </footer>
    </div>
  </div>
</template>

<style scoped>
.mask {
  position: fixed;
  inset: 0;
  z-index: 200;
  display: flex;
  align-items: center;
  justify-content: center;
  background: rgba(0, 0, 0, 0.45);
}

/* Win11 ContentDialog：圆角 8、实色浮出层、宽度收敛 */
.dialog {
  width: min(760px, 92vw);
  max-height: 84vh;
  display: flex;
  flex-direction: column;
  background: var(--layer-flyout);
  border: 1px solid var(--stroke);
  border-radius: var(--radius-window);
  box-shadow: 0 12px 40px rgba(0, 0, 0, 0.45);
  overflow: hidden;
}

.head {
  flex: none;
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 14px 18px 10px;
}

h2 {
  margin: 0;
  font-size: 16px;
  font-weight: 600;
  color: var(--text);
}

.x {
  width: 28px;
  height: 28px;
  border-radius: var(--radius-control);
  color: var(--text-secondary);
}

.x:hover {
  background: var(--layer-control-hover);
  color: var(--text);
}

.search-row {
  flex: none;
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 0 18px 10px;
}

.search-icon {
  font-size: 12px;
}

.search {
  flex: 1 1 auto;
  background: var(--layer-control);
  border: 1px solid var(--stroke);
  border-radius: var(--radius-control);
  color: var(--text);
  padding: 6px 10px;
  outline: none;
}

.search:focus {
  border-color: var(--accent);
  border-bottom-width: 2px;
}

.count {
  flex: none;
  font-size: 11px;
  color: var(--text-tertiary);
}

.scroll {
  flex: 1 1 auto;
  min-height: 0;
  overflow-y: auto;
  padding: 0 18px 8px;
}

.group {
  margin-bottom: 14px;
}

.group-title {
  margin: 6px 0 8px;
  font-size: 12px;
  font-weight: 600;
  color: var(--text-secondary);
}

.grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(300px, 1fr));
  gap: 8px;
}

.card {
  display: flex;
  align-items: flex-start;
  gap: 10px;
  padding: 10px 12px;
  border-radius: var(--radius-card);
  border: 1px solid var(--stroke);
  background: var(--layer-control);
  text-align: left;
  transition:
    border-color var(--motion-fast),
    background var(--motion-fast);
}

.card:hover {
  background: var(--layer-control-hover);
  border-color: var(--accent);
}

.icon {
  flex: none;
  width: 32px;
  height: 32px;
  display: flex;
  align-items: center;
  justify-content: center;
  border-radius: var(--radius-control);
  font-size: 17px;
}

.meta {
  display: flex;
  flex-direction: column;
  min-width: 0;
  gap: 2px;
}

.name {
  font-size: 12px;
  color: var(--text);
  display: flex;
  align-items: center;
  gap: 6px;
}

.rec {
  font-size: 9px;
  padding: 1px 5px;
  border-radius: 3px;
  background: var(--accent);
  color: var(--accent-on);
}

.desc {
  font-size: 11px;
  color: var(--text-tertiary);
  line-height: 1.45;
}

.manual .icon {
  background: var(--layer-control-hover);
}

.hint,
.err {
  font-size: 12px;
  color: var(--text-tertiary);
  padding: 12px 0;
}

.err {
  color: var(--msg-error);
}

.foot {
  flex: none;
  display: flex;
  justify-content: flex-end;
  padding: 10px 18px 14px;
  border-top: 1px solid var(--stroke);
}

.btn {
  min-width: 88px;
  padding: 5px 16px;
  font-size: 12px;
  border-radius: var(--radius-control);
  background: var(--layer-control);
  color: var(--text);
  border: 1px solid var(--stroke);
}

.btn:hover {
  background: var(--layer-control-hover);
}
</style>
