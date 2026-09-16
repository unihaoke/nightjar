<script setup lang="ts">
/** 404 页面。 */
import { useRouter } from 'vue-router'

const router = useRouter()
</script>

<template>
  <div class="not-found">
    <div class="panel">
      <p class="code mono">404</p>
      <h1 class="title">页面不存在</h1>
      <p class="desc">链接可能已失效，或该功能在当前版本尚未开放。</p>
      <div class="row">
        <el-button type="primary" @click="router.replace({ name: 'dashboard' })">返回大盘</el-button>
        <el-button @click="router.back()">返回上一页</el-button>
      </div>
    </div>
  </div>
</template>

<style scoped>
.not-found {
  min-height: 100dvh;
  display: grid;
  place-items: center;
  padding: var(--sp-4);
}

.panel {
  max-width: 420px;
  text-align: center;
}

.code {
  margin: 0;
  font-size: 40px;
  font-weight: 700;
  color: var(--c-text-3);
  letter-spacing: 0.04em;
}

.title {
  margin: 8px 0 0;
  font-size: 18px;
  font-weight: 600;
}

.desc {
  margin: 8px 0 20px;
  color: var(--c-text-3);
  font-size: 13px;
}

.row {
  display: flex;
  gap: 8px;
  justify-content: center;
  flex-wrap: wrap;
}
</style>
