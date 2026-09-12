<script setup lang="ts">
/**
 * 鸣谢与链接区块。
 *
 * 对应 V1 `main.py:542-600` 的 credits：GitHub 链接、Kook 频道、硅基流动
 * 邀请链接与邀请码、鸣谢名单、作者、座右铭、反馈文字，以及展示图。
 *
 * 展示图（V1 的 `xintubiao.png`）已迁移到 `frontend/public/xintubiao.png`，
 * 经 `/xintubiao.png` 提供。加载失败时整块隐藏而不是留一个破图标——图挂了
 * 不该让鸣谢区看起来像坏了（见下面的 logoOK）。
 */
import { onBeforeUnmount, onMounted, ref } from 'vue'

const links = [
  {
    label: 'GitHub',
    text: 'https://github.com/BunNiuNai/Euro-Truck-Simulator-2-TruckersMP-In-Game-Translator',
    url: 'https://github.com/BunNiuNai/Euro-Truck-Simulator-2-TruckersMP-In-Game-Translator',
  },
  { label: 'Kook', text: 'https://kook.vip/WJZ5Sj', url: 'https://kook.vip/WJZ5Sj' },
  {
    label: '硅基流动邀请链接',
    text: 'https://cloud.siliconflow.cn/i/OQZrwou4',
    url: 'https://cloud.siliconflow.cn/i/OQZrwou4',
  },
  {
    label: '小米 MiMo 邀请链接',
    text: '体验小米顶尖模型 MiMo V2.5 等。通过我的邀请码注册：可得 ¥10 API 体验金 + 首单 9 折。邀请码：KF8E7P。注册：https://platform.xiaomimimo.com?ref=KF8E7P（注册后自动填入 · 体验金 40 天有效）',
    url: 'https://platform.xiaomimimo.com?ref=KF8E7P',
  },
]

function open(url: string): void {
  // 在 WebView2 里由 native 拦截 NewWindowRequested；浏览器里就是新标签页
  window.open(url, '_blank', 'noopener')
}

/**
 * 展示图：V1 用 `_resource_path("xintubiao.png")` 加载打包进 exe 的图片。
 * V2 的图片资源还没进资源目录，这里先探测一次；取不到就整块不显示
 * （与 V1 的 try/except 行为一致——V1 加载失败时也是静默跳过）。
 */
const logoOK = ref(false)

function probeLogo(): void {
  const img = new Image()
  img.onload = () => {
    logoOK.value = true
  }
  img.onerror = () => {
    logoOK.value = false
  }
  img.src = '/xintubiao.png'
}

onMounted(probeLogo)
onBeforeUnmount(() => {
  /* 无需清理：Image 加载失败/成功都会被 GC */
})
</script>

<template>
  <section class="credits">
    <div v-for="l in links" :key="l.url" class="line">
      <button class="link" type="button" @click="open(l.url)">{{ l.label }}: {{ l.text }}</button>
      <span v-if="l.label === 'Kook'" class="note">（链接不跳转请搜索频道：39037626）</span>
      <span v-if="l.label === '硅基流动邀请链接'" class="note">邀请码: OQZrwou4</span>
    </div>

    <p class="plain">特别鸣谢：阿呆喵　川子　小mini</p>
    <p class="plain">作者：BinNiuNai　阿呆喵　川子　小mini</p>
    <p class="quote">"如果游戏的快乐都要被明码标价，那我们会站出来"　-- 小mini</p>

    <div class="feedback">
      <p class="plain">翻译器更新与使用反馈可在 Kook 频道留言，如需新功能欢迎提出</p>
      <!-- V1 把展示图放在这一行的右侧 -->
      <img v-if="logoOK" class="logo" src="/xintubiao.png" alt="" />
    </div>
  </section>
</template>

<style scoped>
.credits {
  display: flex;
  flex-direction: column;
  gap: 2px;
  padding: 4px 2px 0;
}

.line {
  display: flex;
  align-items: baseline;
  flex-wrap: wrap;
  gap: 4px;
}

.link {
  font-size: 11px;
  color: #6a9955;
  text-align: left;
  padding: 0;
}

.link:hover {
  color: var(--ok);
  text-decoration: underline;
}

.note,
.plain {
  font-size: 11px;
  color: var(--text-tertiary);
  margin: 0;
}

.quote {
  margin: 4px 0;
  font-size: 10px;
  font-style: italic;
  color: #569cd6;
}

/* 反馈文字在左、展示图在右（V1 的 bottom_row 就是这个布局） */
.feedback {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  margin-top: 2px;
}

.logo {
  flex: none;
  max-height: 40px;
  max-width: 160px;
  object-fit: contain;
  opacity: 0.85;
}
</style>
