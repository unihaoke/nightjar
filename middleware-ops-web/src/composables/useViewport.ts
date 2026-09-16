/** 响应式断点组合式函数：统一视口监听与移动端判断。 */
import { onBeforeUnmount, onMounted } from 'vue'
import { useAppStore } from '@/stores/app'

/** 注册视口监听（在应用根组件调用一次即可）。 */
export function useViewport(): void {
  const store = useAppStore()

  const sync = (): void => store.setViewport(window.innerWidth)

  onMounted(() => {
    sync()
    window.addEventListener('resize', sync, { passive: true })
    window.addEventListener('orientationchange', sync, { passive: true })
  })

  onBeforeUnmount(() => {
    window.removeEventListener('resize', sync)
    window.removeEventListener('orientationchange', sync)
  })
}
