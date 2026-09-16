<script setup lang="ts">
/**
 * 应用外壳布局。
 *
 * 响应式策略：
 *   - 桌面：侧栏 + 顶栏 + 内容区；
 *   - 平板：侧栏默认折叠为图标栏；
 *   - 手机：侧栏收入抽屉，顶栏提供汉堡菜单入口。
 */
import { computed, onMounted } from 'vue'
import { RouterView, useRoute, useRouter } from 'vue-router'
import { ElMessageBox } from 'element-plus'
import { useAppStore } from '@/stores/app'
import { useUserStore } from '@/stores/user'
import SideNav from './SideNav.vue'

const app = useAppStore()
const store = useUserStore()
const route = useRoute()
const router = useRouter()

const pageTitle = computed(() => (route.meta.title as string) || '控制台')

const engineLabel = computed(() => {
  const engine = store.engine
  if (!engine) {
    return { text: '引擎未知', type: 'info' as const }
  }
  if (!engine.available) {
    return { text: '规则引擎兜底', type: 'warning' as const }
  }
  if (engine.degraded || engine.circuit_open) {
    return { text: '引擎降级', type: 'warning' as const }
  }
  return { text: `引擎正常 · ${engine.name}`, type: 'success' as const }
})

/** 退出登录。 */
async function handleLogout(): Promise<void> {
  try {
    await ElMessageBox.confirm('确认退出当前账号？', '退出登录', {
      confirmButtonText: '退出',
      cancelButtonText: '取消',
      type: 'warning',
    })
  } catch {
    return
  }
  await store.logout()
  void router.replace({ name: 'login' })
}

onMounted(async () => {
  // 外壳挂载时确保会话已加载（刷新页面后恢复权限与引擎状态）。
  if (!store.loaded) {
    try {
      await store.fetchProfile()
    } catch {
      // 401 由拦截器统一处理。
    }
  }
})
</script>

<template>
  <div class="shell">
    <!-- 桌面/平板：固定侧栏 -->
    <aside v-if="!app.isMobile" class="shell-side" :class="{ collapsed: app.sidebarCollapsed }">
      <SideNav />
    </aside>

    <!-- 移动端：抽屉导航 -->
    <el-drawer
      v-else
      v-model="app.mobileNavOpen"
      direction="ltr"
      :with-header="false"
      size="76%"
      class="nav-drawer"
    >
      <SideNav />
    </el-drawer>

    <div class="shell-main">
      <header class="topbar">
        <button
          v-if="app.isMobile"
          type="button"
          class="icon-btn"
          aria-label="打开导航"
          @click="app.setMobileNav(true)"
        >
          <el-icon><Fold /></el-icon>
        </button>
        <button
          v-else
          type="button"
          class="icon-btn"
          :aria-label="app.sidebarCollapsed ? '展开侧栏' : '收起侧栏'"
          @click="app.toggleCollapse()"
        >
          <el-icon><Expand v-if="app.sidebarCollapsed" /><Fold v-else /></el-icon>
        </button>

        <h1 class="topbar-title">{{ pageTitle }}</h1>

        <div class="spacer" />

        <el-tag v-if="!app.isMobile" :type="engineLabel.type" size="small" effect="light" round>
          {{ engineLabel.text }}
        </el-tag>

        <button type="button" class="icon-btn" aria-label="切换主题" @click="store.toggleTheme()">
          <el-icon><Moon v-if="store.theme === 'light'" /><Sunny v-else /></el-icon>
        </button>

        <el-dropdown trigger="click" @command="(cmd: string) => cmd === 'logout' ? handleLogout() : router.push({ name: cmd })">
          <button type="button" class="user-btn">
            <el-avatar :size="26" class="user-avatar">{{ (store.user?.nickname || store.user?.username || 'U').slice(0, 1) }}</el-avatar>
            <span v-if="!app.isMobile" class="user-name">{{ store.user?.nickname || store.user?.username }}</span>
            <el-icon v-if="!app.isMobile"><ArrowDown /></el-icon>
          </button>
          <template #dropdown>
            <el-dropdown-menu>
              <el-dropdown-item disabled>
                <span class="muted">{{ store.user?.role_code }}</span>
              </el-dropdown-item>
              <el-dropdown-item command="profile" divided>个人设置</el-dropdown-item>
              <el-dropdown-item command="logout">退出登录</el-dropdown-item>
            </el-dropdown-menu>
          </template>
        </el-dropdown>
      </header>

      <main class="shell-content">
        <RouterView v-slot="{ Component }">
          <component :is="Component" />
        </RouterView>
      </main>
    </div>
  </div>
</template>

<style scoped>
.shell {
  display: flex;
  height: 100%;
  min-height: 100dvh;
  background: var(--c-bg);
}

.shell-side {
  width: var(--sidebar-w);
  flex: 0 0 var(--sidebar-w);
  transition: width 160ms ease, flex-basis 160ms ease;
}

.shell-side.collapsed {
  width: 64px;
  flex-basis: 64px;
}

.shell-main {
  flex: 1;
  min-width: 0;
  display: flex;
  flex-direction: column;
}

.topbar {
  display: flex;
  align-items: center;
  gap: 8px;
  height: var(--header-h);
  flex: 0 0 var(--header-h);
  padding: 0 12px;
  padding-top: var(--safe-top);
  background: var(--c-surface);
  border-bottom: 1px solid var(--c-border);
  position: sticky;
  top: 0;
  z-index: 10;
}

.topbar-title {
  margin: 0;
  font-size: 14.5px;
  font-weight: 600;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}

.icon-btn {
  display: grid;
  place-items: center;
  width: 34px;
  height: 34px;
  border: 1px solid transparent;
  border-radius: var(--r-md);
  background: transparent;
  color: var(--c-text-2);
  cursor: pointer;
  font-size: 16px;
}

.icon-btn:hover {
  background: var(--c-surface-2);
  color: var(--c-text);
}

.icon-btn:active {
  transform: translateY(1px);
}

.user-btn {
  display: flex;
  align-items: center;
  gap: 6px;
  padding: 3px 8px 3px 3px;
  border: 1px solid var(--c-border);
  border-radius: var(--r-pill);
  background: var(--c-surface);
  color: var(--c-text);
  cursor: pointer;
  font-family: inherit;
  font-size: 13px;
}

.user-btn:hover {
  border-color: var(--c-border-strong);
}

.user-avatar {
  background: var(--c-accent);
  color: #fff;
  font-size: 12px;
}

.user-name {
  max-width: 96px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.shell-content {
  flex: 1;
  min-width: 0;
  overflow-x: hidden;
}

.nav-drawer :deep(.el-drawer__body) {
  padding: 0;
}
</style>
