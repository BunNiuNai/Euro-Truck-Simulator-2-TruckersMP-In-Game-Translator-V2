<script setup lang="ts">
/**
 * API / Provider 设置。
 *
 * 对应 V1 **实际在用**的那套 UI：`main.py:_build_api_tab` +
 * `_add_provider_widget` / `_add_provider` / `_remove_provider` /
 * `_move_provider` / `_toggle_provider` / `_test_all_providers`。
 *
 * ⚠️ 为什么不是主从布局：V1 里存在两套 Provider 编辑界面（缺陷 D3）——
 * `main.py:SettingsDialog` 的**卡片列表**（实际在用）与
 * `settings_ui.ProviderEditPanel` 的**左列表右详情**（未被使用）。
 * 早期版本我照着后者做了，与用户实际看到的 V1 不一致，现已改回卡片列表。
 *
 * 卡片字段也只有 V1 的四个（名称 / 地址 / 密钥 / 模型）+ 启用复选框 + ↑↓✕。
 * V1 的这两处没有 weight / timeout / api_format 输入框——它们只在从预设
 * 添加时由预设填充。这里保持一致。
 *
 * ⚠️ 密钥是**直接回显**的：GET /api/config 返回真实密钥，输入框显示的就是它
 * （与 V1 一致，`main.py:1318` 也是把真密钥填进输入框）。清空输入框 = 真的清空。
 *
 * 曾经有过一套「不回显 + `__UNCHANGED__` 哨兵」，已整套删除；删除原因见
 * backend/internal/api/api.go 的 handleConfig 说明。
 */
import { computed, ref } from 'vue'
import { api, ApiError } from '../../api/client'
import type { Provider } from '../../api/types'
import { currentConfig, patchConfig } from '../../composables/useConfig'
import { LANGUAGES, labelOf } from '../../constants/languages'
import Field from '../components/Field.vue'
import PresetDialog from '../dialogs/PresetDialog.vue'


const providers = computed<Provider[]>(() => currentConfig()?.llm_providers ?? [])
const cfg = computed(() => currentConfig())

const presetOpen = ref(false)
const testStatus = ref('')
const testing = ref(false)

/**
 * 该 Provider 是否已配置密钥。
 *
 * 只有**空串**才代表未配置——密钥直接回显，值是真实的。
 *
 * 历史坑（已随哨兵机制一起删除）：曾经把空串和 `__UNCHANGED__` 哨兵一起判成
 * "没有密钥"，于是保存过一次之后再打开设置，界面永远显示「尚未配置 / 输入新密钥」，
 * 而模型其实一直能用——用户看到的状态和实际状态相反，只会反复重填。
 * 现在没有哨兵了，判据只有一条，**别再往里加特例**。
 */
function hasKey(p: Provider | undefined): boolean {
  return !!p && p.api_key !== ''
}

function setProviders(list: Provider[]): void {
  patchConfig({ llm_providers: list })
}

function updateAt(i: number, patch: Partial<Provider>): void {
  setProviders(providers.value.map((p, idx) => (idx === i ? { ...p, ...patch } : p)))
}

// ── 增删排序（对应 V1 _add_provider / _remove_provider / _move_provider）──

/**
 * 添加空 Provider。
 * 字段与 V1 `_add_provider` 完全一致——只有这 5 个，其余留空由 Go 侧取零值。
 */
function addProvider(): void {
  const n = providers.value.length + 1
  setProviders([
    ...providers.value,
    {
      id: '',
      label: `Provider ${n}`,
      endpoint: '',
      api_key: '',
      model: '',
      enabled: true,
      preset_id: '',
      icon: '',
      api_format: 'openai',
      weight: 0,
      extra_headers: {},
      extra_body: {},
      timeout: 0,
    },
  ])
}

function removeProvider(i: number): void {
  clearModelPanels()
  setProviders(providers.value.filter((_, idx) => idx !== i))
}

/** ↑↓ 交换相邻两项。顺序有语义：Go 侧按列表顺序做竞速。 */
function moveProvider(i: number, dir: -1 | 1): void {
  const list = [...providers.value]
  const j = i + dir
  if (j < 0 || j >= list.length) return
  clearModelPanels()
  ;[list[i], list[j]] = [list[j], list[i]]
  setProviders(list)
}

// ── 预设（对应 V1 _add_provider_from_preset）──

function onPresetPick(p: Provider): void {
  presetOpen.value = false
  setProviders([...providers.value, p])
}

function onPresetManual(): void {
  presetOpen.value = false
  addProvider()
}

// ── 📥 拉取可用模型（V2 有意新增的能力）──
//
// ⚠️ 这个功能在 V1 里**没有可对照的真实行为**。它只存在于 V1 的死文件
// `ets2-translator/settings_ui.py`：`ModelFetchDialog`（约 580-620 行，标题
// 「可用模型 — {provider_name}」）与 `_on_fetch_models`（约 776-800 行）。
// 那个文件在 main.py:327 标着 DEPRECATED 且全仓无实例化点，`model_fetcher.py`
// 也只被它引用——也就是说这个界面元素在 V1 里**从未对用户出现过**。
// 所以下面只借用它的**文案与意图**（📥 图标、拉取 → 让用户选 → 写回 Model），
// 不复刻它的实现形态：V1 弹的是独立 Toplevel 模态框，而设置窗口本身已经是
// 独立窗口，再套一层模态除了逼用户多按一次 Esc，还要在 v-for 外面维护一个
// 「当前选中的是哪个 Provider」的下标。这里改成卡片内联面板。
//
// ⚠️ 另一个必须写死的区别：`/api/providers/models` 的 `error` 字段是
// **Go 的原始错误串（英文、面向开发者）**，而同一个 Provider 发给
// `/api/providers/test` 拿到的是 V1 格式的**中文提示**
// （如「[网络错误] 无法连接到 API 服务器，请检查地址和网络」）。
// 两个端点口径不同，别当成一回事，也别以后「统一」成一种：
// 一个是给用户看的，一个是给排查用的。

/** 一次拉取的结局。'' = 还没结束 */
type ModelsFail = '' | 'provider' | 'transport'

interface ModelsState {
  loading: boolean
  /**
   * 面板是否展开。单独存一份而不是从 models 反推：失败时 models 是 null，
   * 只靠它推就分不清「还没展开」和「展开了但是失败了」。
   */
  open: boolean
  /** null = 这一次没拿到列表 */
  models: string[] | null
  fail: ModelsFail
  /** 失败的技术细节（Go 的原始错误串），只给排查用；可能为空 */
  detail: string
  latencyMs: number
}

const EMPTY_MODELS_STATE: ModelsState = {
  loading: false,
  open: false,
  models: null,
  fail: '',
  detail: '',
  latencyMs: 0,
}

/**
 * 按**卡片下标**存状态，与上面的 keyDraft 同一套索引方式。
 * 卡片是 v-for 出来的，script 里没有「每张卡片」这个作用域，只能这样存。
 */
const modelState = ref<Record<number, ModelsState>>({})

function stateOf(i: number): ModelsState {
  return modelState.value[i] ?? EMPTY_MODELS_STATE
}

function modelsOf(i: number): string[] {
  return stateOf(i).models ?? []
}

/** 整体换一份新 Record：就地改外层对象不一定被 Vue 埋到，漏一次就是「点了按钮界面不动」 */
function setState(i: number, next: ModelsState): void {
  modelState.value = { ...modelState.value, [i]: next }
}

/**
 * 失败时那句**中文概括**。
 *
 * 分两种而不是一句话带过：拉取失败最常见的原因是 endpoint / 密钥填错，
 * 但「后端整个没响应」时原因完全不同——那时候提示「检查 Endpoint」会把用户
 * 引到错误的方向去查（真实原因是本地 Go 没起来，去改远程地址只会更糟）。
 */
function failSummary(i: number): string {
  return stateOf(i).fail === 'transport'
    ? '没能从本地服务拿到结果：后端可能没在运行，或返回了异常响应。'
    : '拉取模型列表失败，请检查 Endpoint 与 API Key 是否正确。'
}

/**
 * 点 📥：拿**界面上这一份**配置去换模型列表。
 *
 * 发的是 providers.value[i] 本身（与 testAll 一样），不是重新 GET /api/config
 * 拉一份——用户可能刚改完 endpoint 还没保存，用界面上这份才符合直觉。
 * 密钥同理：界面里就是真实密钥，前端不做任何特殊处理。
 */
async function fetchModelsFor(i: number): Promise<void> {
  const p = providers.value[i]
  // loading 是「避免连点」的第二道保险，第一道是按钮上的 :disabled
  if (!p || stateOf(i).loading) return

  // ⚠️ 这里只重置**本面板**的状态，绝不碰 p.model：拉取失败时用户已经手敲好的
  // 模型名必须原样留着，否则一次网络抖动就把他填好的东西悄悄抹掉了。
  setState(i, { loading: true, open: true, models: null, fail: '', detail: '', latencyMs: 0 })

  try {
    const r = await api.fetchModels(p)
    if (r.success) {
      setState(i, {
        loading: false,
        open: true,
        // 后端保证是数组，这里再兜一次底：真拿到 null 时下面的 v-for 会直接抛
        models: r.models ?? [],
        fail: '',
        detail: '',
        latencyMs: Math.round(r.latencyMs ?? 0),
      })
    } else {
      // success:false 是**业务失败**，不是异常——只写 try/catch 会把每一次
      // 失败都当成功。error 原文不进主提示，只作为技术细节（见文件头说明）。
      setState(i, {
        loading: false,
        open: true,
        models: null,
        fail: 'provider',
        detail: r.error ?? '',
        latencyMs: Math.round(r.latencyMs ?? 0),
      })
    }
  } catch (e) {
    // 走这里的是传输层 / 信封层失败（ApiError）：后端没起、超时、响应不是 JSON。
    // 与上面那条分支分开，因为给用户的下一步动作完全不同。
    setState(i, {
      loading: false,
      open: true,
      models: null,
      fail: 'transport',
      detail: e instanceof ApiError ? e.message : String(e),
      latencyMs: 0,
    })
  }
}

/** 选中一个模型 → 写回该 Provider 的 model 并收起面板 */
function pickModel(i: number, model: string): void {
  // 复用 updateAt：它经 patchConfig 走，会顺手把 llm_providers 标成脏字段，
  // 底部「有未保存的改动」才会亮起来。绕过它直接改就是一次静默丢失。
  updateAt(i, { model })
  closeModelPanel(i)
}

function closeModelPanel(i: number): void {
  setState(i, { ...stateOf(i), open: false })
}

/**
 * 清掉**所有**卡片的模型面板状态。
 *
 * ⚠️ 删除 / 上下移 Provider 时必须先调它：modelState 是按**卡片下标**存的，
 * 而这个文件里唯一按下标存的东西就是它自己（keyDraft 同病，只是危害小得多）。
 * 删掉第 1 张卡片后，原来第 2 张的状态会「落」到新的第 1 张上——面板会在
 * 另一个 Provider 底下继续显示上一个 Provider 的模型列表，用户点一下就把
 * 模型 id 写进了**另一个** Provider 的 model 字段。这种错配比状态丢失难查得多，
 * 所以宁可让面板收起重来。
 */
function clearModelPanels(): void {
  if (Object.keys(modelState.value).length === 0) return
  modelState.value = {}
}

// ── 测试所有（对应 V1 _test_all_providers）──

/**
 * V1 的行为：**只测启用的** Provider，结果逐行列出，全部通过才算成功。
 * 测试期间禁用按钮。
 */
async function testAll(): Promise<void> {
  testing.value = true
  testStatus.value = '正在测试…'
  try {
    const enabled = providers.value.filter((p) => p.enabled)
    if (enabled.length === 0) {
      testStatus.value = '没有启用的 Provider'
      return
    }
    const lines: string[] = []
    let allOK = true
    for (const p of enabled) {
      try {
        const r = await api.testProvider(p)
        if (!r.success) allOK = false
        lines.push(`${p.label || p.id}: ${r.success ? '✓' : '✗'} ${r.message}`)
      } catch (e) {
        allOK = false
        lines.push(`${p.label || p.id}: ✗ ${e instanceof ApiError ? e.message : String(e)}`)
      }
    }
    testStatus.value = lines.join('\n')
    testOk.value = allOK
  } finally {
    testing.value = false
  }
}

const testOk = ref(true)

// ── 语言（对应 V1 LANGUAGE 卡片）──

const targetLabel = computed(() => labelOf(cfg.value?.target_language ?? 'zh-CN'))
const sendTargetLabel = computed(() => labelOf(cfg.value?.send_target_language ?? 'en'))

function setLanguage(field: 'target_language' | 'send_target_language', code: string): void {
  patchConfig({ [field]: code } as never)
}
</script>

<template>
  <div class="tab">
    <!-- ══ LLM PROVIDERS ══ -->
    <section class="section">
      <h3 class="section-label">LLM PROVIDERS / 语言模型提供商</h3>

      <p v-if="providers.length === 0" class="empty">
        还没有 Provider。用「📦 预设」快速添加，或点「+ 添加 Provider」手动填写。
      </p>

      <!-- 每个 Provider 一张卡片 -->
      <div v-for="(p, i) in providers" :key="`${i}-${p.id}`" class="card provider">
        <div class="p-head">
          <label class="enable">
            <input
              type="checkbox"
              :checked="p.enabled"
              @change="updateAt(i, { enabled: ($event.target as HTMLInputElement).checked })"
            />
            <span class="p-name">{{ p.label || `Provider ${i + 1}` }}</span>
          </label>
          <div class="p-actions">
            <button type="button" title="上移" :disabled="i === 0" @click="moveProvider(i, -1)">↑</button>
            <button
              type="button"
              title="下移"
              :disabled="i === providers.length - 1"
              @click="moveProvider(i, 1)"
            >
              ↓
            </button>
            <button type="button" class="danger" title="删除" @click="removeProvider(i)">✕</button>
          </div>
        </div>

        <Field label="Label / 名称">
          <input
            class="input wide"
            :value="p.label"
            @input="updateAt(i, { label: ($event.target as HTMLInputElement).value })"
          />
        </Field>

        <Field label="Endpoint / 地址">
          <input
            class="input wide"
            :value="p.endpoint"
            placeholder="https://api.example.com/v1/chat/completions"
            @input="updateAt(i, { endpoint: ($event.target as HTMLInputElement).value })"
          />
        </Field>

        <Field label="API Key / 密钥">
          <!-- 密钥**直接回显**（V1 的做法：`main.py:1318` 把真实密钥填进输入框，
               `:1377` 保存时读它）。type=password 只是视觉遮挡，不改变"框里就是
               当前值"这个语义——所以「留空」等于清空，想保留就别动它。 -->
          <input
            class="input wide"
            type="password"
            autocomplete="off"
            placeholder="尚未配置密钥"
            :value="p.api_key"
            @input="updateAt(i, { api_key: ($event.target as HTMLInputElement).value })"
          />
          <button class="btn tiny" type="button" :disabled="!hasKey(p)" @click="updateAt(i, { api_key: '' })">
            清除
          </button>
        </Field>

        <Field label="Model / 模型">
          <input
            class="input wide"
            :value="p.model"
            placeholder="手动填写，或点右侧 📥 拉取"
            @input="updateAt(i, { model: ($event.target as HTMLInputElement).value })"
          />
          <!-- 📥：拉取该 Provider 的可用模型。V2 新增，V1 活代码里没有这个按钮
               （只存在于从未被实例化的 settings_ui.py，见脚本区注释）。 -->
          <button
            class="btn tiny fetch"
            type="button"
            title="拉取可用模型列表"
            :disabled="stateOf(i).loading"
            @click="fetchModelsFor(i)"
          >
            {{ stateOf(i).loading ? '⏳' : '📥' }}
          </button>
        </Field>

        <!-- 拉取结果：内联在卡片里，对齐到控件列（标签 116px + 间距 12px）。
             不弹二级窗口——设置窗口本身就是独立窗口，理由见脚本区注释。 -->
        <div v-if="stateOf(i).open" class="picker">
          <div class="picker-head">
            <span class="picker-title">可用模型 — {{ p.label || `Provider ${i + 1}` }}</span>
            <span v-if="!stateOf(i).loading && !stateOf(i).fail && modelsOf(i).length" class="picker-meta">
              {{ modelsOf(i).length }} 个 · {{ stateOf(i).latencyMs }}ms
            </span>
            <button class="picker-close" type="button" title="收起" @click="closeModelPanel(i)">✕</button>
          </div>

          <p v-if="stateOf(i).loading" class="picker-hint">正在拉取模型列表…</p>

          <!-- 失败：先给一句中文概括说清「去查什么」，再把原始错误串作为
               技术细节放在下面。后者是 Go 的英文错误，不能当提示语直接用。 -->
          <template v-else-if="stateOf(i).fail">
            <p class="picker-hint bad">{{ failSummary(i) }}</p>
            <p v-if="stateOf(i).detail" class="picker-detail">技术细节：{{ stateOf(i).detail }}</p>
          </template>

          <!-- success 但没有模型：这是成功的一种，必须说清楚。
               渲染成一片空白会让用户以为按钮坏了。 -->
          <p v-else-if="modelsOf(i).length === 0" class="picker-hint warn">
            服务器可达但没有返回模型列表（该服务商可能不提供 GET /v1/models）。
          </p>

          <ul v-else class="picker-list">
            <li v-for="(m, mi) in modelsOf(i)" :key="mi">
              <button
                class="picker-item"
                type="button"
                :class="{ active: m === p.model }"
                @click="pickModel(i, m)"
              >
                {{ m }}
              </button>
            </li>
          </ul>
        </div>
      </div>

      <div class="add-row">
        <button class="btn" type="button" @click="addProvider">+ 添加 Provider</button>
        <button class="btn primary" type="button" @click="presetOpen = true">📦 预设</button>
      </div>
    </section>

    <!-- ══ LANGUAGE ══ -->
    <section class="section">
      <h3 class="section-label">LANGUAGE / 语言设置</h3>
      <div class="card">
        <Field label="Target Language / 目标语言" :hint="targetLabel">
          <select
            class="input"
            :value="cfg?.target_language"
            @change="setLanguage('target_language', ($event.target as HTMLSelectElement).value)"
          >
            <option v-for="l in LANGUAGES" :key="l.code" :value="l.code">{{ l.label }}</option>
          </select>
        </Field>
        <Field label="Send Language / 发送目标语言" :hint="sendTargetLabel">
          <select
            class="input"
            :value="cfg?.send_target_language"
            @change="setLanguage('send_target_language', ($event.target as HTMLSelectElement).value)"
          >
            <option v-for="l in LANGUAGES" :key="l.code" :value="l.code">{{ l.label }}</option>
          </select>
        </Field>
      </div>
    </section>

    <!-- ══ 测试 ══ -->
    <div class="test-row">
      <button class="btn" type="button" :disabled="testing" @click="testAll">
        {{ testing ? '测试中…' : '🔍 测试所有 Provider' }}
      </button>
      <pre v-if="testStatus" class="test-status" :class="{ bad: !testOk }">{{ testStatus }}</pre>
    </div>

    <PresetDialog v-if="presetOpen" @pick="onPresetPick" @manual="onPresetManual" @close="presetOpen = false" />
  </div>
</template>

<style scoped>
.tab {
  display: flex;
  flex-direction: column;
  gap: 16px;
}

.section-label {
  margin: 0 0 8px;
  font-size: 11px;
  font-weight: 600;
  letter-spacing: 0.04em;
  color: var(--text-tertiary);
}

.card {
  background: var(--layer-card);
  border: 1px solid var(--stroke);
  border-radius: var(--radius-card);
  padding: 10px 16px 12px;
}

.provider {
  margin-bottom: 8px;
}

.p-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
  padding-bottom: 6px;
  border-bottom: 1px solid var(--stroke);
  margin-bottom: 6px;
}

/* V1 的启用复选框文字就是 Provider 名称 */
.enable {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  font-size: 13px;
  font-weight: 600;
  color: var(--text);
  cursor: pointer;
}

.p-name {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.p-actions {
  display: flex;
  gap: 2px;
  flex: none;
}

.p-actions button {
  width: 26px;
  height: 26px;
  border-radius: var(--radius-control);
  color: var(--text-secondary);
  font-size: 12px;
  line-height: 1;
}

.p-actions button:hover:not(:disabled) {
  background: var(--layer-control-hover);
  color: var(--text);
}

.p-actions button:disabled {
  opacity: 0.3;
  cursor: default;
}

.p-actions button.danger:hover {
  color: var(--msg-error);
}

.empty {
  margin: 0 0 10px;
  font-size: 12px;
  color: var(--text-tertiary);
}

.add-row {
  display: flex;
  gap: 8px;
  margin-top: 10px;
}

/* ── 控件 ── */
.input {
  background: var(--layer-control);
  border: 1px solid var(--stroke);
  border-radius: var(--radius-control);
  color: var(--text);
  padding: 5px 8px;
  outline: none;
  transition: border-color var(--motion-fast);
}

.input:hover {
  background: var(--layer-control-hover);
}

.input:focus {
  border-color: var(--accent);
  border-bottom-width: 2px;
}

.input.wide {
  flex: 1 1 auto;
  min-width: 0;
}

.btn {
  padding: 5px 14px;
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

.btn.primary {
  background: var(--accent);
  border-color: transparent;
  color: var(--accent-on);
  font-weight: 600;
}

.btn.primary:hover:not(:disabled) {
  background: var(--accent-hover);
}

.btn.tiny {
  padding: 3px 10px;
  font-size: 11px;
  flex: none;
}

/* 📥 按钮在 ⏳ / 📥 之间换字：定宽居中，免得一拉取整行输入框就跟着抖一下 */
.btn.tiny.fetch {
  width: 34px;
  padding-left: 0;
  padding-right: 0;
  text-align: center;
  font-size: 13px;
  line-height: 1.2;
}

/* ── 📥 模型选择面板 ── */
/* 左边距 128px = Field 的标签列 116px + 间距 12px，让面板与控件列对齐 */
.picker {
  margin: 2px 0 4px 128px;
  padding: 6px 8px 8px;
  background: var(--layer-control);
  border: 1px solid var(--stroke);
  border-radius: var(--radius-card);
}

.picker-head {
  display: flex;
  align-items: center;
  gap: 8px;
  padding-bottom: 5px;
  border-bottom: 1px solid var(--stroke);
}

.picker-title {
  flex: 1 1 auto;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-size: 11px;
  font-weight: 600;
  color: var(--text-secondary);
}

.picker-meta {
  flex: none;
  font-size: 11px;
  color: var(--text-tertiary);
}

.picker-close {
  flex: none;
  width: 20px;
  height: 20px;
  border-radius: var(--radius-control);
  color: var(--text-secondary);
  font-size: 11px;
  line-height: 1;
}

.picker-close:hover {
  background: var(--layer-control-hover);
  color: var(--text);
}

/* 模型可能上百个（OpenRouter 之类），必须限高内滚，否则卡片会被撑得很长 */
.picker-list {
  list-style: none;
  margin: 6px 0 0;
  padding: 0;
  max-height: 168px;
  overflow-y: auto;
}

.picker-item {
  display: block;
  width: 100%;
  padding: 4px 8px;
  border-radius: var(--radius-control);
  text-align: left;
  font-family: var(--font-mono);
  font-size: 11px;
  color: var(--text);
  transition: background var(--motion-fast);
}

.picker-item:hover {
  background: var(--layer-control-hover);
}

/* 当前已填的模型：选中态用强调色描边而不是整块铺色，
   因为「已选中」和「鼠标悬停」在同一行上要能同时看清 */
.picker-item.active {
  color: var(--accent);
  box-shadow: inset 0 0 0 1px var(--accent);
}

.picker-hint {
  margin: 6px 0 0;
  font-size: 11px;
  line-height: 1.5;
  color: var(--text-secondary);
}

.picker-hint.bad {
  color: var(--msg-error);
}

.picker-hint.warn {
  color: var(--warn);
}

/* 原始错误串：小字、次要色、等宽、可选中复制——它是排查线索，
   不是提示语，视觉上必须与上面那句中文概括分得开 */
.picker-detail {
  margin: 4px 0 0;
  font-family: var(--font-mono);
  font-size: 10px;
  line-height: 1.5;
  color: var(--text-tertiary);
  word-break: break-all;
  user-select: text;
}

/* ── 测试状态 ── */
.test-row {
  display: flex;
  align-items: flex-start;
  gap: 12px;
}

.test-status {
  flex: 1 1 auto;
  margin: 0;
  font-family: var(--font-mono);
  font-size: 11px;
  line-height: 1.6;
  color: var(--ok);
  white-space: pre-wrap;
  word-break: break-all;
  user-select: text;
}

.test-status.bad {
  color: var(--msg-error);
}
</style>
