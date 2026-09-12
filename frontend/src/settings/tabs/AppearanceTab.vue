<script setup lang="ts">
/**
 * 外观设置 —— 布局与控件对齐 Win11「设置 → 个性化和系统 → 颜色」。
 *
 * V1 对应 main.py 的 `_build_appearance_tab`：字号、原文/语言标签开关、
 * 不透明度、窗口模式、鼠标穿透。**主题是 V2 新增的**（V1 没有主题概念，
 * 悬浮窗写死深色、设置页另有一套 VS Code 配色，见缺陷 D1）。
 *
 * window_mode 只有两个值（standalone / overlay），与 V1 一致（ADR-011）。
 * 使用前提必须写在界面上：独占全屏下任何独立窗口都显示不出来（ADR-014）。
 */
import { computed } from 'vue'
import { currentConfig, patchConfig } from '../../composables/useConfig'
import { previewTheme, type ThemeMode } from '../../composables/useTheme'
import Field from '../components/Field.vue'
import ToggleSwitch from '../components/ToggleSwitch.vue'


const cfg = computed(() => currentConfig())

function update(patch: Record<string, unknown>): void {
  patchConfig(patch as never)
}

/** Win11「选择模式」的三个选项 */
const themes: Array<{ key: ThemeMode; label: string; hint: string }> = [
  { key: 'light', label: '浅色', hint: '白底深字' },
  { key: 'dark', label: '深色', hint: '黑底浅字' },
  { key: 'system', label: '跟随系统', hint: '随 Windows 设置切换' },
]

// 窗口模式的取值定义已随选择器一起移除（见模板里的说明与缺陷 D20）。
// 配置字段 window_mode 仍然保留在 config 里，供老配置读入时兼容。

const currentTheme = computed<ThemeMode>(() => {
  const t = cfg.value?.theme
  return t === 'light' || t === 'system' || t === 'dark' ? t : 'dark'
})

/**
 * 切主题：立即预览 + 记进待保存的配置。
 * 立即预览很重要——主题这种东西不看到效果没法选。
 */
function pickTheme(t: ThemeMode): void {
  previewTheme(t)
  update({ theme: t })
}

/** 拖不透明度滑块时同步预览底色 */
function setOpacity(v: number): void {
  previewTheme(currentTheme.value, v)
  update({ window_opacity: v })
}

/**
 * 把当前主色应用到界面。
 * accent_color 在 V1 里从未被任何代码读过（缺陷 D1）；这里让它真正驱动
 * --accent，两个窗口统一生效。
 */
function applyAccent(hex: string): void {
  update({ accent_color: hex })
  if (/^#[0-9a-fA-F]{6}$/.test(hex)) {
    document.documentElement.style.setProperty('--accent', hex)
  }
}
</script>

<template>
  <div v-if="cfg" class="tab">
    <!-- ── 主题 ── -->
    <section class="card">
      <h3>主题模式</h3>
      <p class="note">
        与 Windows 的「个性化 → 颜色 → 选择模式」对应。切换会同时改变界面配色与
        native 窗口的材质深浅，避免出现「浅色内容 + 深色材质」的错配。
      </p>

      <div class="theme-grid">
        <button
          v-for="t in themes"
          :key="t.key"
          class="theme-card"
          :class="{ active: currentTheme === t.key }"
          @click="pickTheme(t.key)"
        >
          <!-- 预览缩略图：左侧栏 + 内容区，用固定配色展示主题本身 -->
          <span class="preview" :class="`preview-${t.key}`">
            <span class="pv-side" />
            <span class="pv-main">
              <span class="pv-line" />
              <span class="pv-line short" />
            </span>
          </span>
          <span class="pv-label">{{ t.label }}</span>
          <span class="pv-hint">{{ t.hint }}</span>
        </button>
      </div>
    </section>

    <!-- ── 窗口 ── -->
    <section class="card">
      <h3>窗口</h3>

      <!-- 窗口模式选择器**已移除**。
           V1 的「切换模式」是死功能（缺陷 D20）：`overlay.py:_apply_mode` 的文档
           字符串写着 "always borderless overlay"，它无条件把窗口设成无边框置顶，
           压根不读 `window_mode`。所以这个选择器在 V1 里点了也不会改变任何东西。
           与其保留一个点了没反应的单选组（界面说谎），不如按用户明确要求去掉：
           悬浮窗始终是悬浮窗。配置字段 `window_mode` 保留给老配置兼容。 -->

      <p class="warn">
        悬浮窗需要游戏运行在<strong>窗口化</strong>或<strong>无边框全屏</strong>模式。
        独占全屏下任何独立窗口都无法显示（ADR-014：不做进程注入）。
      </p>

      <Field label="Game Name / 游戏昵称" hint="用于识别自己发的消息（不翻译）">
        <input
          class="input wide"
          :value="cfg.player_name"
          placeholder="游戏内昵称"
          @input="update({ player_name: ($event.target as HTMLInputElement).value })"
        />
      </Field>

      <Field label="不透明度" :hint="`${Math.round(cfg.window_opacity * 100)}%`">
        <input
          class="slider"
          type="range"
          min="0.2"
          max="1"
          step="0.05"
          :value="cfg.window_opacity"
          @input="setOpacity(Number(($event.target as HTMLInputElement).value))"
        />
      </Field>

      <Field label="鼠标穿透" hint="开启后悬浮窗不再接收鼠标">
        <ToggleSwitch
          :model-value="cfg.click_through"
          @update:model-value="update({ click_through: $event })"
        />
      </Field>
    </section>

    <!-- ── 文字 ── -->
    <section class="card">
      <h3>文字</h3>

      <Field label="字号" :hint="`${cfg.font_size}px`">
        <input
          class="slider"
          type="range"
          min="9"
          max="24"
          step="1"
          :value="cfg.font_size"
          @input="update({ font_size: Number(($event.target as HTMLInputElement).value) })"
        />
      </Field>

      <Field label="显示原文" hint="在译文下方以次要色显示一行原文">
        <ToggleSwitch
          :model-value="cfg.show_original_text"
          @update:model-value="update({ show_original_text: $event })"
        />
      </Field>

      <Field label="语言标签" hint="在玩家名前显示 [英语] 这类标签">
        <ToggleSwitch
          :model-value="cfg.show_language_label"
          @update:model-value="update({ show_language_label: $event })"
        />
      </Field>

      <Field label="Max Messages / 最大消息数" hint="超出后从最旧的开始丢弃">
        <input
          class="input narrow"
          type="number"
          min="10"
          max="2000"
          :value="cfg.max_messages"
          @input="update({ max_messages: Number(($event.target as HTMLInputElement).value) })"
        />
      </Field>
    </section>

    <!-- ── 强调色 ── -->
    <section class="card">
      <h3>强调色</h3>
      <p class="note">
        V1 里这个配置项从未被任何代码读取（缺陷 D1），V2 起真正驱动两个窗口的主色。
      </p>
      <Field label="颜色">
        <input
          class="color"
          type="color"
          :value="/^#[0-9a-fA-F]{6}$/.test(cfg.accent_color) ? cfg.accent_color : '#4494fc'"
          @input="applyAccent(($event.target as HTMLInputElement).value)"
        />
        <input
          class="input"
          :value="cfg.accent_color"
          placeholder="#4494fc"
          @input="applyAccent(($event.target as HTMLInputElement).value)"
        />
      </Field>
    </section>
  </div>
</template>

<style scoped>
.tab {
  display: flex;
  flex-direction: column;
  gap: 12px;
}

/* Win11 卡片：圆角 + 一层极淡的填充 + 描边 */
.card {
  background: var(--layer-card);
  border: 1px solid var(--stroke);
  border-radius: var(--radius-card);
  padding: 14px 18px 16px;
}

h3 {
  margin: 0 0 6px;
  font-size: 14px;
  font-weight: 600;
  color: var(--text);
}

.note {
  margin: 0 0 12px;
  font-size: 11px;
  line-height: 1.6;
  color: var(--text-secondary);
}

.warn {
  margin: 6px 0 10px;
  padding: 8px 10px;
  font-size: 11px;
  line-height: 1.6;
  color: var(--warn);
  background: color-mix(in srgb, var(--warn) 12%, transparent);
  border-radius: var(--radius-control);
}

/* ── 主题选择卡（Win11「选择模式」的形态）── */
.theme-grid {
  display: grid;
  grid-template-columns: repeat(3, 1fr);
  gap: 10px;
}

.theme-card {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 3px;
  padding: 10px 8px 12px;
  border-radius: var(--radius-card);
  border: 1px solid var(--stroke);
  background: var(--layer-control);
  transition:
    border-color var(--motion-fast),
    background var(--motion-fast);
}

.theme-card:hover {
  background: var(--layer-control-hover);
}

.theme-card.active {
  border-color: var(--accent);
  /* 选中态：强调色描边 + 淡强调底，与 Win11 一致 */
  background: color-mix(in srgb, var(--accent) 10%, var(--layer-control));
}

/* 预览缩略图 */
.preview {
  width: 100%;
  height: 52px;
  display: flex;
  border-radius: var(--radius-control);
  overflow: hidden;
  border: 1px solid var(--stroke);
  margin-bottom: 6px;
}

.pv-side {
  width: 32%;
}

.pv-main {
  flex: 1;
  display: flex;
  flex-direction: column;
  justify-content: center;
  gap: 5px;
  padding: 0 8px;
}

.pv-line {
  height: 4px;
  border-radius: 2px;
}

.pv-line.short {
  width: 60%;
}

/* 预览用固定配色，不随当前主题变——它们要「展示」主题本身 */
.preview-light {
  background: #f3f3f3;
}
.preview-light .pv-side {
  background: #e5e5e5;
}
.preview-light .pv-line {
  background: #b8b8b8;
}

.preview-dark {
  background: #202020;
}
.preview-dark .pv-side {
  background: #2c2c2c;
}
.preview-dark .pv-line {
  background: #5a5a5a;
}

/* 跟随系统：左右半张，直观表达「跟着系统变」 */
.preview-system {
  background: linear-gradient(90deg, #f3f3f3 0 50%, #202020 50% 100%);
}
.preview-system .pv-side {
  background: linear-gradient(90deg, #e5e5e5 0 50%, #2c2c2c 50% 100%);
}
.preview-system .pv-line {
  background: linear-gradient(90deg, #b8b8b8 0 50%, #5a5a5a 50% 100%);
}

.pv-label {
  font-size: 12px;
  color: var(--text);
}

.pv-hint {
  font-size: 10px;
  color: var(--text-tertiary);
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

/* V1 的窗口模式是并列单选按钮，这里同样并排显示 */
.radio {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  margin-right: 16px;
  font-size: 12px;
  color: var(--text);
  cursor: pointer;
}

.radio input {
  accent-color: var(--accent);
}

.input:hover {
  background: var(--layer-control-hover);
}

/* Win11 输入框聚焦时底部一条强调色下划线 */
.input:focus {
  border-color: var(--accent);
  border-bottom-width: 2px;
}

.input.narrow {
  width: 90px;
}

.input.wide {
  flex: 1 1 auto;
  min-width: 0;
  max-width: 320px;
}

.slider {
  width: 220px;
  accent-color: var(--accent);
}

.color {
  width: 40px;
  height: 28px;
  padding: 0;
  border: 1px solid var(--stroke);
  border-radius: var(--radius-control);
  background: var(--layer-control);
}

.btn {
  padding: 5px 14px;
  font-size: 12px;
  border-radius: var(--radius-control);
  background: var(--layer-control);
  color: var(--text);
  border: 1px solid var(--stroke);
}

.btn:hover {
  background: var(--layer-control-hover);
}

.btn.tiny {
  padding: 3px 10px;
  font-size: 11px;
}
</style>
