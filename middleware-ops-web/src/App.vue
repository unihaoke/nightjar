<script setup lang="ts">
/** 应用根组件：注册全局视口监听并渲染路由。 */
import { RouterView } from 'vue-router'
import { useViewport } from '@/composables/useViewport'

useViewport()
</script>

<template>
  <RouterView />
</template>
