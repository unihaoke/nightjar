<script setup lang="ts">
/**
 * 侧边导航。
 *
 * 移动端适配策略：
 *   - 桌面（≥1024px）：固定侧栏，可折叠为图标栏；
 *   - 平板（768-1023px）：默认收起为图标栏；
 *   - 手机（<768px）：改为抽屉（Drawer）从左侧滑出，点击遮罩或选中项后关闭。
 */
import { computed } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useAppStore } from '@/stores/app'
import { useUserStore } from '@/stores/user'

const app = useAppStore()
const store = useUserStore()
const route = useRoute()
const router = useRouter()

/** 导航项定义（含移动端抽屉中的中文分组标题）。 */
interface NavItem {
  name: string
  title: string
  icon: string
  permission?: string
}

interface NavGroup {
  group: string
  items: NavItem[]
}

const groups: NavGroup[] = [
  {
    group: '总览',
    items: [{ name: 'dashboard', title: '全局大盘', icon: 'Odometer', permission: 'system:overview' }],
  },
  {
    group: '资源',
    items: [
      { name: 'integrations', title: '集成中心', icon: 'Connection', permission: 'middleware:read' },
      { name: 'middlewares', title: '中间件纳管', icon: 'Coin', permission: 'middleware:read' },
      { name: 'monitor', title: '统一监控', icon: 'TrendCharts', permission: 'monitor:read' },
    ],
  },
  {
    group: '智能',
    items: [
      { name: 'ai-diagnose', title: 'AI 诊断', icon: 'MagicStick', permission: 'ai:use' },
      { name: 'ai-history', title: '诊断历史', icon: 'Clock', permission: 'ai:use' },
      { name: 'ai-quality', title: '质量与成本', icon: 'DataAnalysis', permission: 'ai:use' },
    ],
  },
  {
    group: '治理',
    items: [
      { name: 'alerts', title: '告警中心', icon: 'Bell', permission: 'alert:read' },
      { name: 'alert-rules', title: '告警规则', icon: 'SetUp', permission: 'alert:read' },
      { name: 'knowledge', title: '知识库', icon: 'Collection', permission: 'knowledge:read' },
    ],
  },
  {
    group: '应用',
    items: [
      { name: 'log-alerts', title: '日志告警', icon: 'Document', permission: 'logalert:read' },
      { name: 'servers', title: '服务器与仓库', icon: 'Monitor', permission: 'logalert:read' },
    ],
  },
  {
    group: '安全',
    items: [
      { name: 'fix', title: '修复执行', icon: 'Tools', permission: 'fix:preview' },
      { name: 'audit', title: '审计日志', icon: 'Tickets', permission: 'audit:read' },
    ],
  },
  {
    group: '系统',
    items: [
      { name: 'approvals', title: '审批管理', icon: 'Stamp', permission: 'approval:read' },
      { name: 'users', title: '用户与角色', icon: 'UserFilled', permission: 'user:manage' },
      { name: 'ai-settings', title: 'AI 设置', icon: 'MagicStick', permission: 'system:config' },
      { name: 'notify-channels', title: '通知渠道', icon: 'Bell', permission: 'system:config' },
      { name: 'system-info', title: '系统信息', icon: 'InfoFilled', permission: 'system:overview' },
    ],
  },
]

/** 过滤无权限的导航项。 */
const visibleGroups = computed(() =>
  groups
    .map((group) => ({
      ...group,
      items: group.items.filter((item) => !item.permission || store.can(item.permission)),
    }))
    .filter((group) => group.items.length > 0),
)

/** 当前激活菜单项。 */
const activeName = computed(() => String(route.name || ''))

/** 收起态下是否只显示图标。 */
const iconOnly = computed(() => !app.isMobile && app.sidebarCollapsed)

/** 跳转并关闭移动端抽屉。 */
function go(name: string): void {
  void router.push({ name })
  if (app.isMobile) {
    app.setMobileNav(false)
  }
}

/** 面板状态：AI 引擎可用性提示语。 */
const engineHint = computed(() => {
  const engine = store.engine
  if (!engine) {
    return ''
  }
  if (!engine.available) {
    return 'AI 引擎不可用，诊断走规则引擎'
  }
  if (engine.degraded || engine.circuit_open) {
    return 'AI 引擎降级中'
  }
  return ''
})
</script>

<template>
  <nav class="side-nav" :class="{ 'is-icon-only': iconOnly }" aria-label="主导航">
    <div class="brand" @click="go('dashboard')">
      <div class="brand-mark" aria-hidden="true">MW</div>
      <div v-show="!iconOnly" class="brand-text">
        <strong>中间件智能运维</strong>
        <span>问题解决平台 v1.0</span>
      </div>
    </div>

    <div class="nav-scroll">
      <div v-for="group in visibleGroups" :key="group.group" class="nav-group">
        <p v-show="!iconOnly" class="nav-group-title">{{ group.group }}</p>
        <button
          v-for="item in group.items"
          :key="item.name"
          type="button"
          class="nav-item"
          :class="{ active: activeName === item.name }"
          :title="item.title"
          @click="go(item.name)"
        >
          <el-icon class="nav-icon"><component :is="item.icon" /></el-icon>
          <span v-show="!iconOnly" class="nav-label">{{ item.title }}</span>
        </button>
      </div>
    </div>

    <div v-show="!iconOnly && engineHint" class="nav-foot">
      <el-icon><WarningFilled /></el-icon>
      <span>{{ engineHint }}</span>
    </div>
  </nav>
</template>

<style scoped>
.side-nav {
  display: flex;
  flex-direction: column;
  height: 100%;
  width: 100%;
  background: var(--c-surface);
  border-right: 1px solid var(--c-border);
}

.brand {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 14px 14px 12px;
  border-bottom: 1px solid var(--c-border);
  cursor: pointer;
  min-height: var(--header-h);
}

.brand-mark {
  width: 30px;
  height: 30px;
  flex: 0 0 30px;
  border-radius: var(--r-md);
  background: var(--c-accent);
  color: #fff;
  font-size: 12px;
  font-weight: 700;
  letter-spacing: 0.02em;
  display: grid;
  place-items: center;
}

.brand-text {
  display: flex;
  flex-direction: column;
  line-height: 1.25;
  min-width: 0;
}

.brand-text strong {
  font-size: 13.5px;
  font-weight: 600;
  white-space: nowrap;
}

.brand-text span {
  font-size: 11px;
  color: var(--c-text-3);
  white-space: nowrap;
}

.nav-scroll {
  flex: 1;
  overflow-y: auto;
  padding: 10px 8px 16px;
}

.nav-group + .nav-group {
  margin-top: 14px;
}

.nav-group-title {
  margin: 0 0 4px 8px;
  font-size: 11px;
  font-weight: 600;
  letter-spacing: 0.06em;
  text-transform: uppercase;
  color: var(--c-text-3);
}

.nav-item {
  display: flex;
  align-items: center;
  gap: 10px;
  width: 100%;
  border: 0;
  background: transparent;
  color: var(--c-text-2);
  border-radius: var(--r-md);
  padding: 8px 10px;
  font-size: 13.5px;
  font-family: inherit;
  cursor: pointer;
  text-align: left;
  transition: background 120ms ease, color 120ms ease;
}

.nav-item:hover {
  background: var(--c-surface-2);
  color: var(--c-text);
}

.nav-item:active {
  transform: translateY(1px);
}

.nav-item.active {
  background: var(--c-accent-soft);
  color: var(--c-accent);
  font-weight: 600;
}

.nav-icon {
  font-size: 16px;
  flex: 0 0 16px;
}

.nav-label {
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}

.is-icon-only .nav-item {
  justify-content: center;
  padding: 10px 0;
}

.nav-foot {
  display: flex;
  align-items: center;
  gap: 6px;
  padding: 10px 12px calc(10px + var(--safe-bottom));
  border-top: 1px solid var(--c-border);
  font-size: 12px;
  color: var(--c-warning);
}
</style>
