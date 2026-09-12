/**
 * 前端入口。
 *
 * 两个窗口各自加载同一份构建产物，用 `?page=` 区分挂载哪个根组件：
 *   ?page=overlay    悬浮窗（默认）
 *   ?page=settings   设置窗口
 *
 * **为什么不用 vue-router**：只有两个页面，而且它们是两个**独立的窗口**，
 * 窗口之间不存在导航——路由在这里只是多余的依赖。
 */
import { createApp, type Component } from 'vue'

import './styles/tokens.css'
import './styles/base.css'

import OverlayApp from './overlay/OverlayApp.vue'
import SettingsApp from './settings/SettingsApp.vue'
import { startEvents } from './composables/useEvents'
// 副作用导入：注册主题监听（跟随配置与系统设置）。
// 必须在挂载前执行，否则会先渲染一帧错误的配色。
import './composables/useTheme'

type PageName = 'overlay' | 'settings'

const pages: Record<PageName, Component> = {
  overlay: OverlayApp,
  settings: SettingsApp,
}

function resolvePage(): PageName {
  const q = new URLSearchParams(window.location.search).get('page')
  if (q === 'settings') return 'settings'
  if (q === 'overlay') return 'overlay'

  // 兼容 #/settings 这种写法，方便手工输入
  if (window.location.hash.startsWith('#/settings')) return 'settings'

  return 'overlay'
}

const page = resolvePage()
document.title = page === 'settings' ? 'ETS2 Translator — 设置' : 'ETS2 Translator'

// SSE 只订阅一次，由各状态模块分发给自己的处理器
startEvents()

createApp(pages[page]).mount('#app')
