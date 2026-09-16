/** 全局 UI 状态（侧边栏折叠、移动端抽屉）。 */
import { defineStore } from 'pinia'
import { computed, ref } from 'vue'

const COLLAPSE_KEY = 'mwops_sidebar_collapsed'

/** UI Store。 */
export const useAppStore = defineStore('app', () => {
  const collapsed = ref(localStorage.getItem(COLLAPSE_KEY) === 'true')
  const mobileNavOpen = ref(false)
  const viewportWidth = ref(typeof window === 'undefined' ? 1440 : window.innerWidth)

  /** 是否为移动端视口（<768px）。 */
  const isMobile = computed(() => viewportWidth.value < 768)

  /** 是否为平板视口（768-1023px）。 */
  const isTablet = computed(() => viewportWidth.value >= 768 && viewportWidth.value < 1024)

  /** 桌面端侧边栏是否收起。 */
  const sidebarCollapsed = computed(() => collapsed.value && !isTablet.value)

  /** 同步视口宽度。 */
  function setViewport(width: number): void {
    viewportWidth.value = width
    if (width >= 1024) {
      mobileNavOpen.value = false
    }
  }

  /** 折叠/展开侧边栏。 */
  function toggleCollapse(): void {
    collapsed.value = !collapsed.value
    localStorage.setItem(COLLAPSE_KEY, String(collapsed.value))
  }

  /** 打开/关闭移动端导航抽屉。 */
  function setMobileNav(open: boolean): void {
    mobileNavOpen.value = open
  }

  return { collapsed, sidebarCollapsed, mobileNavOpen, isMobile, isTablet, setViewport, toggleCollapse, setMobileNav }
})
