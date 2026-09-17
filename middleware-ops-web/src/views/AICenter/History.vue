<script setup lang="ts">
/** 诊断历史：检索、查看详情、复用结论。 */
import { ref } from 'vue'
import { useRouter } from 'vue-router'
import { aiApi } from '@/api'
import { toastError } from '@/api/http'
import type { DiagnosisRecord, DiagnosisResponse } from '@/api/types'
import { useListPage } from '@/composables/useListPage'
import ResponsiveList from '@/components/ResponsiveList.vue'
import DiagnosisReport from '@/components/DiagnosisReport.vue'
import { feedbackLabels, formatTime, mwTypeLabels } from '@/utils/format'

const router = useRouter()

const detailVisible = ref(false)
const detailLoading = ref(false)
const current = ref<DiagnosisRecord | null>(null)

const list = useListPage<DiagnosisRecord>({
  fetch: (params, signal) => aiApi.history(params, signal),
  defaults: { keyword: '', mw_type: '', feedback: '', page: 1, page_size: 20 },
})
const { query, items, total, loading, error } = list

/** 查看详情。 */
async function openDetail(row: DiagnosisRecord): Promise<void> {
  detailVisible.value = true
  detailLoading.value = true
  try {
    const detail = await aiApi.detail(row.id)
    current.value = detail.diagnosis
  } catch (error) {
    toastError(error)
  } finally {
    detailLoading.value = false
  }
}

/** 反馈。 */
async function handleFeedback(value: 'useful' | 'useless' | 'adopted'): Promise<void> {
  if (!current.value) {
    return
  }
  try {
    await aiApi.feedback(current.value.id, value)
    current.value.feedback = value
    await list.load()
  } catch (error) {
    toastError(error)
  }
}

/** 以相同问题重新诊断。 */
function rediagnose(row: DiagnosisRecord): void {
  void router.push({ name: 'ai-diagnose', query: { instance_id: String(row.instance_id) } })
}

/** 当前详情对应的结构化响应（复用报告组件）。 */
function asResponse(row: DiagnosisRecord): DiagnosisResponse | null {
  if (!row.report) {
    return null
  }
  return {
    diagnosis_id: row.id,
    meta: {
      instance_id: row.instance_id,
      instance_name: '',
      mw_type: row.mw_type,
      engine_name: row.engine_used,
      engine_status: row.engine_status,
      cache_hit: row.cache_hit,
      truncated: row.truncated,
      missing: null,
      prompt_tokens: 0,
      scope: '',
      data_source: '',
    },
    report: row.report,
    references: null,
    warnings: null,
    engine_used: row.engine_used,
    cost_tokens: row.cost_tokens,
    duration_ms: row.duration_ms,
  }
}
</script>

<template>
  <div class="page">
    <div class="page-header">
      <div>
        <h2 class="page-title">诊断历史</h2>
        <p class="page-subtitle">诊断结果自动存档并可检索复用；同实例同问题 24 小时内命中确定性缓存</p>
      </div>
      <el-button type="primary" :icon="'MagicStick'" @click="router.push({ name: 'ai-diagnose' })">新建诊断</el-button>
    </div>

    <ResponsiveList
      :items="items"
      :loading="loading"
      :error="error"
      :total="total"
      :page="query.page"
      :page-size="query.page_size"
      empty-text="暂无诊断记录"
      @update:page="list.setPage"
      @update:page-size="list.setPageSize"
      @retry="list.load"
    >
      <template #filters>
        <el-input v-model="query.keyword" placeholder="按问题关键词搜索" clearable class="filter-item" @keyup.enter="list.search" />
        <el-select v-model="query.mw_type" placeholder="全部类型" clearable class="filter-item">
          <el-option label="Redis" value="redis" />
          <el-option label="Kafka" value="kafka" />
          <el-option label="MySQL" value="mysql" />
          <el-option label="PostgreSQL" value="pg" />
          <el-option label="Elasticsearch" value="es" />
          <el-option label="Nginx" value="nginx" />
        </el-select>
        <el-select v-model="query.feedback" placeholder="全部反馈" clearable class="filter-item">
          <el-option label="有用" value="useful" />
          <el-option label="没用" value="useless" />
          <el-option label="已采纳" value="adopted" />
        </el-select>
        <el-button type="primary" :icon="'Search'" @click="list.search">查询</el-button>
        <el-button :icon="'RefreshLeft'" @click="list.reset">重置</el-button>
      </template>

      <!-- 移动端：卡片 -->
      <template #card="{ row }">
        <div class="history-title">
          <el-tag size="small" effect="plain">{{ mwTypeLabels[row.mw_type] || row.mw_type }}</el-tag>
          <span class="question">{{ row.user_query }}</span>
        </div>
        <p class="history-summary">{{ row.report?.root_cause || '（无结构化结论）' }}</p>
        <div class="history-meta">
          <span class="muted">{{ formatTime(row.created_at) }}</span>
          <span class="muted">置信度 {{ Math.round((row.report?.confidence || 0) * 100) }}%</span>
          <el-tag v-if="row.feedback" size="small" effect="plain">
            {{ feedbackLabels[row.feedback] || row.feedback }}
          </el-tag>
        </div>
        <div class="history-actions is-card">
          <el-button size="small" @click="openDetail(row)">详情</el-button>
          <el-button size="small" @click="rediagnose(row)">重新诊断</el-button>
        </div>
      </template>

      <!-- 桌面端：列表 -->
      <template #table>
        <ul class="history-list">
        <li v-for="item in items" :key="item.id">
          <div class="history-main">
            <div class="history-title">
              <el-tag size="small" effect="plain">{{ mwTypeLabels[item.mw_type] || item.mw_type }}</el-tag>
              <span class="question">{{ item.user_query }}</span>
            </div>
            <p class="history-summary">{{ item.report?.root_cause || '（无结构化结论）' }}</p>
            <div class="history-meta">
              <span class="muted">{{ formatTime(item.created_at) }}</span>
              <span class="muted">置信度 {{ Math.round((item.report?.confidence || 0) * 100) }}%</span>
              <span class="muted">tokens {{ item.cost_tokens }}</span>
              <span class="muted">{{ item.duration_ms }} ms</span>
              <el-tag v-if="item.engine_status !== 'ok'" size="small" type="warning" effect="light">
                {{ item.engine_status }}
              </el-tag>
              <el-tag v-if="item.feedback" size="small" effect="plain">
                {{ feedbackLabels[item.feedback] || item.feedback }}
              </el-tag>
            </div>
          </div>
          <div class="history-actions">
            <el-button text size="small" @click="openDetail(item)">详情</el-button>
            <el-button text size="small" @click="rediagnose(item)">重新诊断</el-button>
          </div>
        </li>
        </ul>
      </template>
    </ResponsiveList>

    <el-drawer v-model="detailVisible" title="诊断详情" :size="'720px'" direction="rtl">
      <div v-loading="detailLoading">
        <template v-if="current">
          <div class="detail-meta">
            <span>{{ current.user_query }}</span>
            <span class="muted">{{ formatTime(current.created_at) }}</span>
          </div>
          <DiagnosisReport
            v-if="asResponse(current)"
            :report="asResponse(current)!.report"
            :truncated="current.truncated"
            :meta="asResponse(current)!.meta"
            @feedback="handleFeedback"
          />
          <el-divider />
          <h4 class="raw-title">原始模型输出</h4>
          <pre class="raw-output">{{ current.diagnosis_result || '（未保留原始输出）' }}</pre>
        </template>
      </div>
    </el-drawer>
  </div>
</template>

<style scoped>
.filter-item {
  width: 200px;
}

.history-list {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
}

.history-list li {
  display: flex;
  gap: 12px;
  justify-content: space-between;
  align-items: flex-start;
  padding: 12px 0;
  border-bottom: 1px solid var(--c-border);
}

.history-list li:last-child {
  border-bottom: 0;
}

.history-main {
  min-width: 0;
  flex: 1;
}

.history-title {
  display: flex;
  align-items: center;
  gap: 8px;
  min-width: 0;
}

.question {
  font-size: 13.5px;
  font-weight: 500;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.history-summary {
  margin: 6px 0 0;
  font-size: 12.5px;
  color: var(--c-text-2);
  display: -webkit-box;
  -webkit-line-clamp: 2;
  -webkit-box-orient: vertical;
  overflow: hidden;
}

.history-meta {
  display: flex;
  gap: 12px;
  flex-wrap: wrap;
  margin-top: 6px;
  font-size: 11.5px;
}

.history-actions {
  flex: 0 0 auto;
}

/* 卡片模式下横向排布 */
.history-actions.is-card {
  display: flex;
  gap: 8px;
  margin-top: 8px;
}

.detail-meta {
  display: flex;
  flex-direction: column;
  gap: 4px;
  font-size: 13.5px;
  margin-bottom: 12px;
}

.raw-title {
  margin: 0 0 8px;
  font-size: 13px;
}

.raw-output {
  margin: 0;
  padding: 12px;
  background: var(--c-surface-2);
  border-radius: var(--r-md);
  max-height: 320px;
  overflow: auto;
  white-space: pre-wrap;
  word-break: break-word;
  font-size: 12px;
}


</style>
