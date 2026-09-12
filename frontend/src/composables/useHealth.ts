/**
 * 健康状况与来源状态。
 *
 * nativeAlive 反映 C++ 能力层是否在线（ADR-012）。它离线时翻译链路照常工作，
 * 只是显示层降级——UI 必须如实告诉用户，不能装作一切正常。
 */
import { readonly, ref } from 'vue'
import { api } from '../api/client'
import type { Health } from '../api/types'

const health = ref<Health | null>(null)

/**
 * 还没连上服务器时显示的占位文案。
 *
 * 文案本身来自 V1 `overlay.py:570-571`，V1 是逐字比对过的那一句。
 * ⚠️ 导出它而不是让组件再写一遍：判断「连上了没有」要拿它做比较
 * （见 OverlayHeader 的 connected），两处各写一份字面量的话，
 * 迟早有一处改了另一处没改，于是服务器名的颜色判断永远为假且查不出来。
 */
export const SERVER_WAITING = '等待服务器连接...'

const serverName = ref(SERVER_WAITING)

export function useHealth() {
  return {
    health: readonly(health),
    serverName: readonly(serverName),
  }
}

export async function refreshHealth(): Promise<void> {
  health.value = await api.health()
}

/** 服务器名来自 Go 的 `source.status` 事件（只有检测到服务器时才会推）。 */
export function setServerName(name: string): void {
  serverName.value = name || SERVER_WAITING
}

/** native 是否可用（未查询到时按不可用处理，宁可保守）。 */
export function nativeAlive(): boolean {
  return health.value?.nativeAlive === true
}
