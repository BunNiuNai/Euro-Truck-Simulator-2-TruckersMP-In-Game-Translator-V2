/**
 * 悬浮窗的拖动与边缘缩放 —— V1 overlay.py:466-533 的等价实现。
 *
 * 分工：
 *   前端只判断「鼠标按在了哪个区域」，把这个区域名发给 Go；Go 转给 native；
 *   native 在**主循环**里逐帧推进窗口几何，直到左键松开。
 *
 * 为什么不交给 Win32 自己（WM_NCHITTEST + WM_NCLBUTTONDOWN）：
 *   1. WebView2 的子窗口铺满客户区，宿主窗口在四边**收不到** WM_NCHITTEST——
 *      实测把光标停在窗口四边，指针始终是 IDC_ARROW。
 *   2. 退而求其次用 WM_NCLBUTTONDOWN 交给系统模态循环也不行：那条循环会阻塞
 *      native 主线程，而管道读取正跑在那个线程上。心跳每 1s 一次、超时 5s，
 *      一次稍长的拖动就会被判成断线并重连，窗口跟着重建。代价不可接受。
 *
 * V1 是「整个画布都能拖」（非边框区域一律 fleur 光标），V2 只有标题带能拖：
 * 整个画布可拖会让滚动、选中、输入全部失效。这是记录在案的实现差异。
 */
import { ref } from 'vue'
import { api, type WindowZone } from '../api/client'
import { showNotice } from './useNotice'

/**
 * 边缘缩放热区的宽度（CSS px）。
 * 与 V1 overlay.py:464 的 `BORDER = 8` 一致——V1 也是用窗口最外 8px 做缩放带，
 * 那 8px 同样压在文本框和它的滚动条上面。
 */
export const RESIZE_BORDER = 8

/** 四角热区尺寸。比 8px 大一些：纯 8×8 的角太难点中，而角正是斜向缩放最顺手的位置。 */
export const RESIZE_CORNER = 14

export function useWindowDrag() {
  /** 是否正在拖动/缩放。用于拖动期间关掉文本选中之类的干扰。 */
  const dragging = ref(false)

  /** 拖动失败只提示一次，否则每次拖都弹会把界面刷满。 */
  let warned = false

  /**
   * 开始一次拖动/缩放。绑到 mousedown 上。
   *
   * 左键限定与 preventDefault 都不能省：
   *   - 右键要留给右键菜单；
   *   - 不 preventDefault 的话 WebView 会自己开始文本选中 / 原生拖放，
   *     和我们的拖动抢鼠标。
   */
  function start(zone: WindowZone, e: MouseEvent): void {
    if (e.button !== 0) return
    e.preventDefault()
    e.stopPropagation()

    dragging.value = true

    // 故意**不 await**：这个请求在用户松开鼠标前不会真正「完成」，
    // 等它只会让拖动发滞。错误在下面单独处理。
    void api
      .beginWindowMove(zone)
      .catch((err: unknown) => {
        dragging.value = false
        if (warned) return
        warned = true
        const msg = err instanceof Error ? err.message : String(err)
        showNotice(`窗口拖动不可用：${msg}`, 'error', 4000)
      })
  }

  return { dragging, start }
}
