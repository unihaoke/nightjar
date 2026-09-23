/** 路由表与导航守卫。 */
import { createRouter, createWebHistory, type RouteRecordRaw } from 'vue-router'
import { ElMessage } from 'element-plus'
import { setUnauthorizedHandler } from '@/api/http'
import { useUserStore } from '@/stores/user'

/** 路由元信息扩展。 */
declare module 'vue-router' {
  interface RouteMeta {
    /** 页面标题。 */
    title?: string
    /** 需要的权限点（任一满足即可）。 */
    permissions?: string[]
    /** 是否公开访问（无需登录）。 */
    public?: boolean
    /** 侧边导航分组。 */
    group?: string
    /** 是否在导航中隐藏。 */
    hidden?: boolean
  }
}

const routes: RouteRecordRaw[] = [
  {
    path: '/login',
    name: 'login',
    component: () => import('@/views/Login.vue'),
    meta: { title: '登录', public: true, hidden: true },
  },
  {
    path: '/',
    component: () => import('@/layouts/AppShell.vue'),
    redirect: '/dashboard',
    children: [
      {
        path: 'dashboard',
        name: 'dashboard',
        component: () => import('@/views/Dashboard.vue'),
        meta: { title: '全局大盘', group: '总览', permissions: ['system:overview'] },
      },
      {
        path: 'integrations',
        name: 'integrations',
        component: () => import('@/views/Integration/Index.vue'),
        meta: { title: '集成中心', group: '资源', permissions: ['middleware:read'] },
      },
      {
        path: 'middlewares',
        name: 'middlewares',
        component: () => import('@/views/Middleware/Index.vue'),
        meta: { title: '中间件纳管', group: '资源', permissions: ['middleware:read'] },
      },
      {
        path: 'middlewares/:id',
        name: 'middleware-detail',
        component: () => import('@/views/Middleware/Detail.vue'),
        meta: { title: '实例详情', permissions: ['middleware:read'], hidden: true },
      },
      {
        path: 'monitor',
        name: 'monitor',
        component: () => import('@/views/Monitor/Index.vue'),
        meta: { title: '统一监控', group: '资源', permissions: ['monitor:read'] },
      },
      {
        path: 'ai',
        name: 'ai-diagnose',
        component: () => import('@/views/AICenter/Diagnose.vue'),
        meta: { title: 'AI 诊断', group: '智能', permissions: ['ai:use'] },
      },
      {
        path: 'ai/history',
        name: 'ai-history',
        component: () => import('@/views/AICenter/History.vue'),
        meta: { title: '诊断历史', group: '智能', permissions: ['ai:use'] },
      },
      {
        path: 'ai/quality',
        name: 'ai-quality',
        component: () => import('@/views/AICenter/Quality.vue'),
        meta: { title: '质量与成本', group: '智能', permissions: ['ai:use'] },
      },
      {
        path: 'alerts',
        name: 'alerts',
        component: () => import('@/views/Alert/Index.vue'),
        meta: { title: '告警中心', group: '治理', permissions: ['alert:read'] },
      },
      {
        path: 'alerts/rules',
        name: 'alert-rules',
        component: () => import('@/views/Alert/Rules.vue'),
        meta: { title: '告警规则', group: '治理', permissions: ['alert:read'] },
      },
      {
        path: 'knowledge',
        name: 'knowledge',
        component: () => import('@/views/Knowledge/Index.vue'),
        meta: { title: '知识库', group: '治理', permissions: ['knowledge:read'] },
      },
      {
        path: 'log-alerts',
        name: 'log-alerts',
        component: () => import('@/views/LogAlert/Index.vue'),
        meta: { title: '日志告警', group: '日志', permissions: ['logalert:read'] },
      },
      {
        path: 'log-alerts/rules',
        name: 'log-alert-rules',
        component: () => import('@/views/LogAlert/Rules.vue'),
        meta: { title: '日志告警规则', group: '日志', permissions: ['logalert:read'] },
      },
      {
        path: 'fix',
        name: 'fix',
        component: () => import('@/views/Fix/Index.vue'),
        meta: { title: '修复执行', group: '安全', permissions: ['fix:preview'] },
      },
      {
        path: 'audit',
        name: 'audit',
        component: () => import('@/views/Audit/Index.vue'),
        meta: { title: '审计日志', group: '安全', permissions: ['audit:read'] },
      },
      {
        path: 'system/approvals',
        name: 'approvals',
        component: () => import('@/views/System/Approvals.vue'),
        meta: { title: '审批管理', group: '系统', permissions: ['approval:read'] },
      },
      {
        path: 'system/users',
        name: 'users',
        component: () => import('@/views/System/Users.vue'),
        meta: { title: '用户与角色', group: '系统', permissions: ['user:manage'] },
      },
      {
        path: 'system/ai-settings',
        name: 'ai-settings',
        component: () => import('@/views/System/AISettings.vue'),
        meta: { title: 'AI 设置', group: '系统', permissions: ['system:config'] },
      },
      {
        path: 'system/notify-channels',
        name: 'notify-channels',
        component: () => import('@/views/System/NotifyChannels.vue'),
        meta: { title: '通知渠道', group: '系统', permissions: ['system:config'] },
      },
      {
        path: 'system/security',
        name: 'security-settings',
        component: () => import('@/views/System/Security.vue'),
        meta: { title: '合规设置', group: '系统', permissions: ['system:config'] },
      },
      {
        path: 'system/info',
        name: 'system-info',
        component: () => import('@/views/System/Info.vue'),
        meta: { title: '系统信息', group: '系统', permissions: ['system:overview'] },
      },
      {
        path: 'profile',
        name: 'profile',
        component: () => import('@/views/System/Profile.vue'),
        meta: { title: '个人设置', hidden: true },
      },
    ],
  },
  {
    path: '/:pathMatch(.*)*',
    name: 'not-found',
    component: () => import('@/views/NotFound.vue'),
    meta: { title: '页面不存在', public: true, hidden: true },
  },
]

const router = createRouter({
  history: createWebHistory(),
  routes,
  scrollBehavior: () => ({ top: 0 }),
})

// 401 统一跳转登录（避免每个页面重复处理）。
setUnauthorizedHandler(() => {
  const current = router.currentRoute.value
  if (current.name !== 'login') {
    void router.replace({ name: 'login', query: { redirect: current.fullPath } })
  }
})

router.beforeEach(async (to) => {
  const store = useUserStore()
  document.title = to.meta.title ? `${to.meta.title} · 中间件智能问题解决平台` : '中间件智能问题解决平台'

  if (to.meta.public) {
    return true
  }

  if (!store.loaded) {
    try {
      await store.fetchProfile()
    } catch {
      store.reset()
      return { name: 'login', query: { redirect: to.fullPath } }
    }
  }

  if (!store.user) {
    return { name: 'login', query: { redirect: to.fullPath } }
  }

  const required = to.meta.permissions
  if (required && required.length > 0 && !store.canAny(required)) {
    ElMessage({ type: 'warning', message: `无访问权限：${to.meta.title || to.path}` })
    return { name: 'dashboard' }
  }
  return true
})

export default router
