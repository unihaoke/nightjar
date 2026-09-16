<script setup lang="ts">
/**
 * AI 诊断中心（核心差异化，4.3 + 第五章六道护栏）。
 *
 * 交互：
 *   - 选择实例或直接用自然语言提问（支持规则 + 实体匹配自动识别目标）；
 *   - SSE 流式返回，实时展示模型输出，结束后渲染结构化报告；
 *   - 展示「已截断 / 缺失」维度、权限范围、引擎状态与成本消耗。
 */
import { computed, onMounted, reactive, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import { aiApi, diagnoseStream, middlewareApi } from '@/api'
import { toastError } from '@/api/http'
import type { DiagnosisResponse, MiddlewareInstance } from '@/api/types'
import DiagnosisReport from '@/components/DiagnosisReport.vue'
import { envLabels, mwTypeLabels } from '@/utils/format'
import { useUserStore } from '@/stores/user'

const route = useRoute()
const router = useRouter()
const store = useUserStore()

const instances = ref<MiddlewareInstance[]>([])
const loadingInstances = ref(false)
const streaming = ref(false)
const rawOutput = ref('')
const result = ref<DiagnosisResponse | null>(null)
const cancelStream = ref<(() => void) | null>(null)

const form = reactive({
  instance_id: 0,
  question: '',
  skip_cache: false,
})

/** 常见问题快捷入口（降低上手成本）。 */
const presets = [
  '最近为什么变慢了？帮我分析根因',
  '内存使用率偏高，会有什么风险？',
  '连接数持续增长，是否需要扩容？',
  '有没有慢查询或阻塞操作？请给出排查步骤',
  '结合当前指标给出一份巡检结论',
]

const canSubmit = computed(() => form.question.trim().length >= 2 && !streaming.value)

/** 加载实例下拉。 */
async function loadInstances(): Promise<void> {
  loadingInstances.value = true
  try {
    const result = await middlewareApi.list({ page: 1, page_size: 100 })
    instances.value = result.list || []
    const preselect = Number(route.query.instance_id || 0)
    if (preselect) {
      form.instance_id = preselect
    } else if (instances.value.length === 1) {
      form.instance_id = instances.value[0].id
    }
  } catch (error) {
    toastError(error)
  } finally {
    loadingInstances.value = false
  }
}

/** 发起流式诊断。 */
function startDiagnose(): void {
  if (!canSubmit.value) {
    ElMessage({ type: 'warning', message: '请输入至少 2 个字符的问题描述' })
    return
  }
  result.value = null
  rawOutput.value = ''
  streaming.value = true

  cancelStream.value = diagnoseStream(
    {
      instance_id: form.instance_id || undefined,
      question: form.question.trim(),
      skip_cache: form.skip_cache,
    },
    {
      onMeta: (meta) => {
        // 元信息先到：可在此展示引擎与权限范围（结果区最终会完整渲染）。
        if (result.value) {
          result.value.meta = meta
        }
      },
      onDelta: (delta) => {
        rawOutput.value += delta
      },
      onDone: (done) => {
        streaming.value = false
        result.value = done
        cancelStream.value = null
        void router.replace({ name: 'ai-diagnose', query: { ...route.query, instance_id: String(done.meta.instance_id) } })
      },
      onError: (message) => {
        streaming.value = false
        cancelStream.value = null
        if (message !== '已取消诊断') {
          ElMessage({ type: 'error', message })
        } else {
          ElMessage({ type: 'info', message })
        }
      },
    },
  )
}

/** 取消诊断。 */
function cancelDiagnose(): void {
  cancelStream.value?.()
  streaming.value = false
}

/** 同步诊断（流式失败或用户偏好时使用）。 */
async function runSync(): Promise<void> {
  if (!canSubmit.value) {
    return
  }
  streaming.value = true
  try {
    result.value = await aiApi.diagnoseSync({
      instance_id: form.instance_id || undefined,
      question: form.question.trim(),
      skip_cache: form.skip_cache,
    })
    rawOutput.value = result.value.raw || ''
  } catch (error) {
    toastError(error)
  } finally {
    streaming.value = false
  }
}

/** 提交反馈。 */
async function handleFeedback(value: 'useful' | 'useless' | 'adopted'): Promise<void> {
  if (!result.value?.diagnosis_id) {
    return
  }
  try {
    await aiApi.feedback(result.value.diagnosis_id, value)
    ElMessage({ type: 'success', message: '反馈已记录，感谢参与质量基线建设' })
  } catch (error) {
    toastError(error)
  }
}

onMounted(loadInstances)
</script>

<template>
  <div class="page">
    <div class="page-header">
      <div>
        <h2 class="page-title">AI 诊断</h2>
        <p class="page-subtitle">
          单轮诊断 + 工具预采集：平台按固定管线采集上下文，一次 LLM 调用产出结构化结论，不迭代、不自主决策
        </p>
      </div>
      <div class="row">
        <el-tag size="small" effect="light" :type="store.engine?.available ? (store.engine?.degraded ? 'warning' : 'success') : 'danger'">
          引擎 {{ store.engine?.name || '-' }}
        </el-tag>
        <el-button size="small" @click="router.push({ name: 'ai-history' })">诊断历史</el-button>
      </div>
    </div>

    <div class="card">
      <el-form label-position="top">
        <el-row :gutter="12">
          <el-col :xs="24" :sm="12">
            <el-form-item label="诊断目标（可留空，由问题自动识别）">
              <el-select v-model="form.instance_id" filterable clearable placeholder="自动识别或选择实例" class="mobile-block" :loading="loadingInstances">
                <el-option
                  v-for="item in instances"
                  :key="item.id"
                  :label="`${item.name}（${mwTypeLabels[item.mw_type] || item.mw_type} · ${envLabels[item.environment]}）`"
                  :value="item.id"
                />
              </el-select>
            </el-form-item>
          </el-col>
          <el-col :xs="24" :sm="12">
            <el-form-item label="上下文缓存">
              <el-checkbox v-model="form.skip_cache">跳过确定性缓存，强制重新诊断（会消耗 token）</el-checkbox>
            </el-form-item>
          </el-col>
        </el-row>

        <el-form-item label="问题描述">
          <el-input
            v-model="form.question"
            type="textarea"
            :rows="3"
            maxlength="1000"
            show-word-limit
            placeholder="例如：Redis 最近为什么变慢了？请结合指标给出根因与处置建议"
            @keydown.ctrl.enter="startDiagnose"
          />
        </el-form-item>

        <div class="presets">
          <span class="muted">快捷提问：</span>
          <el-tag v-for="item in presets" :key="item" class="preset-tag" effect="plain" @click="form.question = item">
            {{ item }}
          </el-tag>
        </div>

        <div class="row submit-row">
          <el-button type="primary" :loading="streaming" :disabled="!canSubmit" :icon="'MagicStick'" @click="startDiagnose">
            开始诊断（流式）
          </el-button>
          <el-button v-if="streaming" @click="cancelDiagnose">取消</el-button>
          <el-button :disabled="streaming" @click="runSync">同步诊断</el-button>
          <span class="muted hint">提示：Ctrl + Enter 提交；流式输出走 SSE（meta/data/done/error 事件）</span>
        </div>
      </el-form>
    </div>

    <!-- 流式原始输出 -->
    <div v-if="streaming || (!result && rawOutput)" class="card">
      <h3 class="card-title">
        模型输出（实时）
        <el-tag v-if="streaming" size="small" type="warning" effect="light">推理中</el-tag>
      </h3>
      <pre class="raw-output">{{ rawOutput || '正在采集上下文并调用模型…' }}</pre>
    </div>

    <!-- 结构化报告 -->
    <div v-if="result" class="card">
      <h3 class="card-title">
        结构化诊断报告
        <div class="row">
          <el-tag v-if="result.meta?.cache_hit" size="small" type="success" effect="light">缓存命中</el-tag>
          <el-tag size="small" effect="plain">#{{ result.diagnosis_id }}</el-tag>
          <el-button text size="small" @click="router.push({ name: 'ai-history' })">查看历史</el-button>
        </div>
      </h3>
      <DiagnosisReport
        :report="result.report"
        :references="result.references"
        :warnings="result.warnings"
        :truncated="result.meta?.truncated"
        :missing="result.meta?.missing"
        :meta="{
          engine_name: result.meta?.engine_name,
          engine_status: result.meta?.engine_status,
          cache_hit: result.meta?.cache_hit,
          data_source: result.meta?.data_source,
          scope: result.meta?.scope,
          prompt_tokens: result.meta?.prompt_tokens,
          duration_ms: result.duration_ms,
          cost_tokens: result.cost_tokens,
        }"
        @feedback="handleFeedback"
      />
    </div>
  </div>
</template>

<style scoped>
.card + .card {
  margin-top: 12px;
}

.presets {
  display: flex;
  align-items: center;
  gap: 6px;
  flex-wrap: wrap;
  margin-bottom: 12px;
}

.preset-tag {
  cursor: pointer;
}

.preset-tag:active {
  transform: translateY(1px);
}

.submit-row {
  margin-top: 4px;
}

.hint {
  font-size: 11.5px;
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
  font-size: 12.5px;
  line-height: 1.65;
}
</style>
