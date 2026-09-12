/**
 * 提示条状态。对应 V1 overlay.py:726-731 的 _show_notice。
 *
 * V1 的行为：显示 duration_ms 后自动消失；期间再来一条会取消上一个定时器。
 * 这里保持一致。
 */
import { readonly, ref } from 'vue'

export interface Notice {
  text: string
  level: string
}

const current = ref<Notice | null>(null)
let timer: number | undefined

/** 显示一条提示，默认 3 秒后自动消失（V1 的默认 duration_ms=3000）。 */
export function showNotice(text: string, level = 'error', durationMs = 3000): void {
  if (timer !== undefined) window.clearTimeout(timer)
  current.value = { text, level }
  timer = window.setTimeout(() => {
    current.value = null
    timer = undefined
  }, durationMs)
}

export function clearNotice(): void {
  if (timer !== undefined) {
    window.clearTimeout(timer)
    timer = undefined
  }
  current.value = null
}

export function useNotice() {
  return { notice: readonly(current) }
}
