/**
 * 统计状态。
 *
 * 数据**全部来自 Go**（/api/stats 快照 + stats.updated 事件），
 * 前端不做任何本地累加——否则前后端各算一套，迟早对不上。
 */
import { readonly, ref } from 'vue'
import { api } from '../api/client'
import { emptyStats, type StatsPayload } from '../api/types'

const stats = ref<StatsPayload>(emptyStats())

export function useStats() {
  return { stats: readonly(stats) }
}

/** 从 stats.updated 事件更新。 */
export function applyStats(p: Partial<StatsPayload>): void {
  stats.value = { ...stats.value, ...p }
}

/** 重新拉一次快照（首次进入与重连后）。 */
export async function refreshStats(): Promise<void> {
  const s = await api.getStats()
  stats.value = s
}
