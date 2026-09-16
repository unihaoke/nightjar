<script setup lang="ts">
/**
 * 知识库（4.5 质量闭环）。
 *
 * 要点：AI 诊断沉淀的条目为 draft 状态，人工确认（采纳/转正）后才参与检索；
 * 低采纳率条目在检索时降权。此处负责草稿审核与人工录入。
 */
import { computed, onMounted, reactive, ref } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import MarkdownIt from 'markdown-it'
import { knowledgeApi } from '@/api'
import { toastError } from '@/api/http'
import type { KnowledgeEntry } from '@/api/types'
import { formatTime, knowledgeStatusLabels, mwTypeLabels } from '@/utils/format'
import { useUserStore } from '@/stores/user'

const store = useUserStore()
const md = new MarkdownIt({ html: false, linkify: true, breaks: true })

const loading = ref(false)
const submitting = ref(false)
const dialogVisible = ref(false)
const detailVisible = ref(false)
const editing = ref<KnowledgeEntry | null>(null)
const current = ref<KnowledgeEntry | null>(null)
const items = ref<KnowledgeEntry[]>([])
const total = ref(0)
const stats = ref<{ total: number; by_status: Record<string, number>; adoption_rate: number } | null>(null)
const statusOptions = ref<{ value: string; label: string }[]>([])

const query = reactive({
  keyword: '',
  mw_type: '',
  status: '',
  source: '',
  page: 1,
  page_size: 20,
})

const canWrite = computed(() => store.can('knowledge:write'))

const form = reactive({
  title: '',
  content: '',
  mw_type: 'redis',
  tags: [] as string[],
  status: 'published',
})

/** Markdown 渲染。 */
function renderMarkdown(content: string): string {
  return md.render(content || '')
}

/** 加载列表与统计。 */
async function load(): Promise<void> {
  loading.value = true
  try {
    const [list, statistics, options] = await Promise.all([
      knowledgeApi.list({ ...query }),
      knowledgeApi.stats(),
      knowledgeApi.options(),
    ])
    items.value = list.list || []
    total.value = list.total || 0
    stats.value = statistics
    statusOptions.value = options.statuses || []
  } catch (error) {
    toastError(error)
  } finally {
    loading.value = false
  }
}

/** 打开编辑。 */
function openForm(entry?: KnowledgeEntry): void {
  editing.value = entry || null
  form.title = entry?.title || ''
  form.content = entry?.content || ''
  form.mw_type = entry?.mw_type || 'redis'
  form.tags = entry?.tags || []
  form.status = entry?.status || 'published'
  dialogVisible.value = true
}

/** 查看详情。 */
async function openDetail(entry: KnowledgeEntry): Promise<void> {
  detailVisible.value = true
  try {
    current.value = await knowledgeApi.detail(entry.id)
  } catch (error) {
    toastError(error)
  }
}

/** 提交。 */
async function submit(): Promise<void> {
  if (!form.title.trim() || !form.content.trim()) {
    ElMessage({ type: 'warning', message: '标题与内容不能为空' })
    return
  }
  submitting.value = true
  try {
    if (editing.value) {
      await knowledgeApi.update(editing.value.id, { ...form })
      ElMessage({ type: 'success', message: '条目已更新' })
    } else {
      await knowledgeApi.create({ ...form })
      ElMessage({ type: 'success', message: '条目已创建' })
    }
    dialogVisible.value = false
    await load()
  } catch (error) {
    toastError(error)
  } finally {
    submitting.value = false
  }
}

/** 采纳（草稿转正 / 提升权重）。 */
async function adopt(entry: KnowledgeEntry): Promise<void> {
  try {
    await knowledgeApi.adopt(entry.id)
    ElMessage({ type: 'success', message: '已采纳，该条目权重已提升' })
    await load()
  } catch (error) {
    toastError(error)
  }
}

/** 快速转正草稿。 */
async function publish(entry: KnowledgeEntry): Promise<void> {
  try {
    await knowledgeApi.update(entry.id, { status: 'published' })
    ElMessage({ type: 'success', message: '草稿已转为已发布，将参与检索' })
    await load()
  } catch (error) {
    toastError(error)
  }
}

/** 删除。 */
async function remove(entry: KnowledgeEntry): Promise<void> {
  try {
    await ElMessageBox.confirm(`确认删除条目「${entry.title}」？`, '删除确认', {
      confirmButtonText: '删除',
      cancelButtonText: '取消',
      type: 'warning',
    })
  } catch {
    return
  }
  try {
    await knowledgeApi.remove(entry.id)
    ElMessage({ type: 'success', message: '条目已删除' })
    await load()
  } catch (error) {
    toastError(error)
  }
}

onMounted(load)
</script>

<template>
  <div class="page">
    <div class="page-header">
      <div>
        <h2 class="page-title">知识库</h2>
        <p class="page-subtitle">
          AI 诊断结果自动沉淀为草稿，人工确认后转正并参与检索；低采纳率条目自动降权
          <span v-if="stats"> · 采纳率 {{ Math.round((stats.adoption_rate || 0) * 100) }}%</span>
        </p>
      </div>
      <el-button v-if="canWrite" type="primary" :icon="'Plus'" @click="openForm()">录入条目</el-button>
    </div>

    <el-row :gutter="12">
      <el-col :xs="8" :sm="8">
        <div class="mini-stat">
          <span class="label">已发布</span>
          <span class="value">{{ stats?.by_status?.published ?? 0 }}</span>
        </div>
      </el-col>
      <el-col :xs="8" :sm="8">
        <div class="mini-stat">
          <span class="label">待确认草稿</span>
          <span class="value text-warning">{{ stats?.by_status?.draft ?? 0 }}</span>
        </div>
      </el-col>
      <el-col :xs="8" :sm="8">
        <div class="mini-stat">
          <span class="label">已废弃</span>
          <span class="value muted">{{ stats?.by_status?.deprecated ?? 0 }}</span>
        </div>
      </el-col>
    </el-row>

    <div class="card filters mt">
      <el-input v-model="query.keyword" placeholder="搜索标题或内容" clearable class="filter-item" @keyup.enter="load" />
      <el-select v-model="query.mw_type" placeholder="全部类型" clearable class="filter-item">
        <el-option label="Redis" value="redis" />
        <el-option label="Kafka" value="kafka" />
        <el-option label="MySQL" value="mysql" />
        <el-option label="PostgreSQL" value="pg" />
        <el-option label="Elasticsearch" value="es" />
        <el-option label="Nginx" value="nginx" />
      </el-select>
      <el-select v-model="query.status" placeholder="全部状态" clearable class="filter-item">
        <el-option v-for="item in statusOptions" :key="item.value" :label="item.label" :value="item.value" />
      </el-select>
      <el-select v-model="query.source" placeholder="全部来源" clearable class="filter-item">
        <el-option label="人工录入" value="manual" />
        <el-option label="AI 诊断沉淀" value="auto" />
      </el-select>
      <el-button type="primary" :icon="'Search'" @click="load">查询</el-button>
    </div>

    <div class="card" v-loading="loading">
      <div v-if="items.length === 0">
        <el-empty description="知识库暂无条目" />
      </div>
      <ul v-else class="kb-list">
        <li v-for="item in items" :key="item.id">
          <div class="kb-main">
            <div class="kb-head">
              <span class="kb-title" @click="openDetail(item)">{{ item.title }}</span>
              <el-tag size="small" effect="plain">{{ mwTypeLabels[item.mw_type] || item.mw_type || '通用' }}</el-tag>
              <el-tag
                size="small"
                :type="item.status === 'published' ? 'success' : item.status === 'draft' ? 'warning' : 'info'"
                effect="light"
              >
                {{ knowledgeStatusLabels[item.status] || item.status }}
              </el-tag>
              <el-tag size="small" effect="plain" :type="item.source === 'auto' ? 'info' : 'primary'">
                {{ item.source === 'auto' ? 'AI 沉淀' : '人工录入' }}
              </el-tag>
            </div>
            <p class="kb-excerpt">{{ item.content.replace(/[#*`>-]/g, '').slice(0, 160) }}</p>
            <div class="kb-meta">
              <span class="muted">{{ formatTime(item.created_at) }}</span>
              <span class="muted">采纳 {{ item.adopt_count }} 次 · 引用 {{ item.use_count }} 次</span>
              <el-tag v-for="tag in item.tags || []" :key="tag" size="small" effect="plain">{{ tag }}</el-tag>
            </div>
          </div>
          <div class="kb-actions">
            <el-button text size="small" @click="openDetail(item)">查看</el-button>
            <el-button v-if="canWrite" text size="small" type="primary" @click="adopt(item)">采纳</el-button>
            <el-button v-if="canWrite && item.status === 'draft'" text size="small" @click="publish(item)">转正</el-button>
            <el-button v-if="canWrite" text size="small" @click="openForm(item)">编辑</el-button>
            <el-button v-if="canWrite" text size="small" type="danger" @click="remove(item)">删除</el-button>
          </div>
        </li>
      </ul>

      <el-pagination
        v-model:current-page="query.page"
        v-model:page-size="query.page_size"
        :total="total"
        :page-sizes="[20, 50, 100]"
        layout="total, sizes, prev, pager, next"
        class="pager"
        @current-change="load"
        @size-change="load"
      />
    </div>

    <!-- 编辑对话框 -->
    <el-dialog v-model="dialogVisible" :title="editing ? '编辑知识条目' : '录入知识条目'" width="680px">
      <el-form label-position="top">
        <el-form-item label="标题">
          <el-input v-model="form.title" placeholder="如 Redis 内存告警处置手册" />
        </el-form-item>
        <el-row :gutter="12">
          <el-col :xs="24" :sm="12">
            <el-form-item label="关联中间件">
              <el-select v-model="form.mw_type" class="mobile-block">
                <el-option label="Redis" value="redis" />
                <el-option label="Kafka" value="kafka" />
                <el-option label="MySQL" value="mysql" />
                <el-option label="PostgreSQL" value="pg" />
                <el-option label="Elasticsearch" value="es" />
                <el-option label="Nginx" value="nginx" />
              </el-select>
            </el-form-item>
          </el-col>
          <el-col :xs="24" :sm="12">
            <el-form-item label="状态">
              <el-select v-model="form.status" class="mobile-block">
                <el-option v-for="item in statusOptions" :key="item.value" :label="item.label" :value="item.value" />
              </el-select>
            </el-form-item>
          </el-col>
          <el-col :span="24">
            <el-form-item label="标签">
              <el-select v-model="form.tags" multiple filterable allow-create default-first-option class="mobile-block" />
            </el-form-item>
          </el-col>
          <el-col :span="24">
            <el-form-item label="内容（支持 Markdown）">
              <el-input v-model="form.content" type="textarea" :rows="12" placeholder="## 现象&#10;## 根因&#10;## 处置步骤" />
            </el-form-item>
          </el-col>
        </el-row>
      </el-form>
      <template #footer>
        <el-button @click="dialogVisible = false">取消</el-button>
        <el-button type="primary" :loading="submitting" @click="submit">保存</el-button>
      </template>
    </el-dialog>

    <!-- 详情抽屉 -->
    <el-drawer v-model="detailVisible" :title="current?.title || '知识条目'" size="640px" direction="rtl">
      <div v-if="current" class="kb-detail">
        <div class="kb-detail-meta">
          <el-tag size="small" effect="plain">{{ mwTypeLabels[current.mw_type] || current.mw_type || '通用' }}</el-tag>
          <el-tag size="small" effect="light">{{ knowledgeStatusLabels[current.status] || current.status }}</el-tag>
          <span class="muted">采纳 {{ current.adopt_count }} 次 · 引用 {{ current.use_count }} 次</span>
          <span class="muted">{{ formatTime(current.created_at) }}</span>
        </div>
        <div class="markdown-body" v-html="renderMarkdown(current.content)" />
      </div>
    </el-drawer>
  </div>
</template>

<style scoped>
.mt {
  margin-top: 12px;
}

.mini-stat {
  background: var(--c-surface);
  border: 1px solid var(--c-border);
  border-radius: var(--r-lg);
  padding: 10px 14px;
  display: flex;
  flex-direction: column;
  gap: 2px;
}

.mini-stat .label {
  font-size: 12px;
  color: var(--c-text-3);
}

.mini-stat .value {
  font-size: 20px;
  font-weight: 600;
  font-variant-numeric: tabular-nums;
}

.filters {
  display: flex;
  gap: 8px;
  flex-wrap: wrap;
  margin-bottom: 12px;
}

.filter-item {
  width: 180px;
}

.kb-list {
  list-style: none;
  margin: 0;
  padding: 0;
}

.kb-list li {
  display: flex;
  gap: 12px;
  justify-content: space-between;
  align-items: flex-start;
  padding: 12px 0;
  border-bottom: 1px solid var(--c-border);
}

.kb-list li:last-child {
  border-bottom: 0;
}

.kb-main {
  flex: 1;
  min-width: 0;
}

.kb-head {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}

.kb-title {
  font-size: 13.5px;
  font-weight: 600;
  cursor: pointer;
}

.kb-title:hover {
  color: var(--c-accent);
}

.kb-excerpt {
  margin: 6px 0 0;
  font-size: 12.5px;
  color: var(--c-text-2);
  display: -webkit-box;
  -webkit-line-clamp: 2;
  -webkit-box-orient: vertical;
  overflow: hidden;
}

.kb-meta {
  display: flex;
  gap: 10px;
  flex-wrap: wrap;
  align-items: center;
  margin-top: 6px;
  font-size: 11.5px;
}

.kb-actions {
  flex: 0 0 auto;
  display: flex;
  flex-direction: column;
  align-items: flex-end;
  gap: 2px;
}

.pager {
  margin-top: 12px;
  justify-content: flex-end;
}

.kb-detail-meta {
  display: flex;
  gap: 10px;
  flex-wrap: wrap;
  align-items: center;
  font-size: 12px;
  padding-bottom: 12px;
  border-bottom: 1px solid var(--c-border);
  margin-bottom: 12px;
}

.markdown-body {
  font-size: 13.5px;
  line-height: 1.75;
  color: var(--c-text-2);
  word-break: break-word;
}

.markdown-body :deep(h2),
.markdown-body :deep(h3) {
  font-size: 14px;
  color: var(--c-text);
  margin: 16px 0 8px;
}

.markdown-body :deep(code) {
  background: var(--c-surface-2);
  padding: 1px 5px;
  border-radius: var(--r-sm);
}

.markdown-body :deep(pre) {
  background: var(--c-surface-2);
  padding: 10px 12px;
  border-radius: var(--r-md);
  overflow-x: auto;
}

@media (max-width: 767px) {
  .filter-item {
    width: 100%;
  }

  .kb-list li {
    flex-direction: column;
  }

  .kb-actions {
    flex-direction: row;
    align-items: center;
    flex-wrap: wrap;
  }
}
</style>
