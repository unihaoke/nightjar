<script setup lang="ts">
/** 系统信息：部署形态、AI 引擎、基础设施、护栏参数与安全配置。 */
import { computed, onMounted, ref } from 'vue'
import { systemApi } from '@/api'
import { toastError } from '@/api/http'
import type { SystemInfo } from '@/api/types'
import { formatDuration, prettyJSON } from '@/utils/format'
import { useAppStore } from '@/stores/app'

const app = useAppStore()

const loading = ref(false)
const info = ref<SystemInfo | null>(null)
const config = ref<Record<string, unknown> | null>(null)

/** 部署形态摘要。 */
const deployment = computed(() => {
  if (!info.value) {
    return []
  }
  const { infrastructure, app: appInfo } = info.value
  return [
    { key: '应用版本', value: `${appInfo.name} v${appInfo.version}` },
    { key: '运行模式', value: appInfo.mode },
    { key: '运行时长', value: formatDuration(Math.max(appInfo.now - appInfo.started_at, 0)) },
    { key: '平台数据库', value: `${infrastructure.database.host}/${infrastructure.database.name}` },
    { key: '缓存实现', value: infrastructure.redis.cache_kind },
    { key: '任务队列', value: infrastructure.redis.queue_kind },
    { key: '监控数据源', value: infrastructure.prometheus.source },
    { key: 'pgvector 原生向量', value: infrastructure.vector.native_pgvector ? '已启用' : '未启用（应用层余弦检索）' },
  ]
})

/** 护栏参数行。 */
const guardrailRows = computed(() => {
  const g = (info.value?.guardrail || {}) as Record<string, unknown>
  return [
    { key: '上下文预算', value: `输入 ${g.input_token_budget ?? '-'} / 输出 ${g.output_token_budget ?? '-'} tokens` },
    { key: '日志采集配额', value: `${(g.log_budget as Record<string, unknown>)?.context_lines ?? '-'} 行 × ${(g.log_budget as Record<string, unknown>)?.max_sources ?? '-'} 来源` },
    { key: '代码采集配额', value: `${(g.code_budget as Record<string, unknown>)?.max_files ?? '-'} 文件 × ${(g.code_budget as Record<string, unknown>)?.lines_per_file ?? '-'} 行` },
    { key: '防死循环', value: `最大 ${(g.loop_guard as Record<string, unknown>)?.max_steps ?? '-'} 步 · 重复 ${(g.loop_guard as Record<string, unknown>)?.repeat_threshold ?? '-'} 次中止` },
    { key: '并发上限', value: String(g.max_concurrency ?? '-') },
    { key: '确定性缓存 TTL', value: String(g.cache_ttl ?? '-') },
    { key: '成本预算（平台/用户）', value: `${g.daily_token_quota ?? '-'} / ${g.per_user_quota ?? '-'} tokens` },
    { key: 'SQL 限制（默认/上限）', value: `${(g.sql_limits as Record<string, unknown>)?.default ?? '-'} / ${(g.sql_limits as Record<string, unknown>)?.max ?? '-'}` },
  ]
})

/** 安全配置行。 */
const securityRows = computed(() => {
  const s = (info.value?.security || {}) as Record<string, unknown>
  const whitelist = (s.outbound_whitelist as string[]) || []
  return [
    { key: '出网白名单', value: whitelist.length > 0 ? whitelist.join('、') : '未配置（默认全部禁止第三方分析）' },
    { key: '代码片段上限', value: `${s.snippet_max_lines ?? '-'} 行/文件` },
    { key: '主密钥轮换周期', value: `${s.key_rotation_days ?? '-'} 天` },
    { key: '主密钥来源', value: s.master_key_configured ? '已配置（环境变量或密钥文件）' : '未配置' },
  ]
})

/** 加载。 */
async function load(): Promise<void> {
  loading.value = true
  try {
    info.value = await systemApi.info()
    try {
      config.value = await systemApi.config()
    } catch {
      config.value = null
    }
  } catch (error) {
    toastError(error)
  } finally {
    loading.value = false
  }
}

onMounted(load)
</script>

<template>
  <div class="page" v-loading="loading">
    <div class="page-header">
      <div>
        <h2 class="page-title">系统信息</h2>
        <p class="page-subtitle">部署形态、AI 引擎策略、基础设施与六道护栏的生效参数</p>
      </div>
      <el-button :icon="'Refresh'" size="small" @click="load">刷新</el-button>
    </div>

    <el-row :gutter="12">
      <el-col :xs="24" :md="12">
        <div class="card">
          <h3 class="card-title">部署形态</h3>
          <div class="kv-list">
            <div v-for="row in deployment" :key="row.key" class="kv">
              <span>{{ row.key }}</span>
              <span class="mono">{{ row.value }}</span>
            </div>
          </div>
        </div>
      </el-col>
      <el-col :xs="24" :md="12">
        <div class="card">
          <h3 class="card-title">
            AI 引擎
            <el-tag size="small" :type="info?.ai_engine.status.available ? (info?.ai_engine.status.degraded ? 'warning' : 'success') : 'danger'">
              {{ info?.ai_engine.strategy }}
            </el-tag>
          </h3>
          <div class="kv-list">
            <div class="kv">
              <span>提供方</span>
              <span class="mono">{{ (info?.ai_engine.providers || []).join(' → ') || '-' }}</span>
            </div>
            <div class="kv">
              <span>当前生效</span>
              <span class="mono">{{ info?.ai_engine.status.name }}</span>
            </div>
            <div class="kv">
              <span>连续失败 / 熔断</span>
              <span class="mono">
                {{ info?.ai_engine.status.consecutive_fails ?? 0 }} / {{ info?.ai_engine.status.circuit_open ? '已打开' : '关闭' }}
              </span>
            </div>
            <div class="kv">
              <span>评测集规模</span>
              <span class="mono">{{ info?.ai_engine.eval_set_size ?? 0 }} 条</span>
            </div>
            <div class="kv">
              <span>只读工具</span>
              <span class="mono">{{ (info?.ai_engine.tools || []).join('、') }}</span>
            </div>
          </div>
          <el-alert
            v-for="(note, index) in info?.ai_engine.notes || []"
            :key="index"
            type="info"
            :closable="false"
            :title="note"
            class="mt-sm"
          />
        </div>
      </el-col>
    </el-row>

    <el-row :gutter="12" class="mt">
      <el-col :xs="24" :md="12">
        <div class="card">
          <h3 class="card-title">护栏参数（引擎层统一生效）</h3>
          <div class="kv-list">
            <div v-for="row in guardrailRows" :key="row.key" class="kv">
              <span>{{ row.key }}</span>
              <span class="mono">{{ row.value }}</span>
            </div>
          </div>
        </div>
      </el-col>
      <el-col :xs="24" :md="12">
        <div class="card">
          <h3 class="card-title">安全与合规</h3>
          <div class="kv-list">
            <div v-for="row in securityRows" :key="row.key" class="kv">
              <span>{{ row.key }}</span>
              <span class="mono">{{ row.value }}</span>
            </div>
            <div class="kv">
              <span>通知渠道</span>
              <span class="row">
                <el-tag
                  v-for="item in info?.notify || []"
                  :key="item.channel"
                  size="small"
                  :type="item.enabled ? 'success' : 'info'"
                  effect="plain"
                >
                  {{ item.channel }}
                </el-tag>
              </span>
            </div>
          </div>
        </div>
      </el-col>
    </el-row>

    <div v-if="config" class="card mt">
      <h3 class="card-title">脱敏后的运行配置</h3>
      <pre class="code-block">{{ prettyJSON(config) }}</pre>
      <p class="muted note">
        密钥类字段一律不出现在接口响应中（仅暴露「是否已配置」）；完整配置请查看部署侧的 configs/config.yaml。
      </p>
    </div>

    <div class="card mt">
      <h3 class="card-title">能力矩阵（声明与实现一致）</h3>
      <div class="table-scroll">
        <el-table :data="info?.capability_matrix || []" size="small">
          <el-table-column prop="mw_type" label="中间件" width="140" />
          <el-table-column label="纳管" width="70">
            <template #default="{ row }">{{ row.manage ? '支持' : '—' }}</template>
          </el-table-column>
          <el-table-column label="监控指标" width="90">
            <template #default="{ row }">{{ row.metrics ? '支持' : '—' }}</template>
          </el-table-column>
          <el-table-column label="阈值告警" width="90">
            <template #default="{ row }">{{ row.alerts ? '支持' : '—' }}</template>
          </el-table-column>
          <el-table-column label="AI 诊断" width="90">
            <template #default="{ row }">{{ row.ai_diagnose ? '支持' : '—' }}</template>
          </el-table-column>
          <el-table-column prop="note" label="备注" min-width="220" />
        </el-table>
      </div>
    </div>

    <p class="muted foot">
      界面密度：{{ app.isMobile ? '移动端紧凑布局' : '桌面控制台布局' }} · 可通过顶栏图标切换深浅主题
    </p>
  </div>
</template>

<style scoped>
.mt {
  margin-top: 12px;
}

.kv-list {
  display: flex;
  flex-direction: column;
  gap: 10px;
}

.kv {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
  font-size: 12.5px;
  color: var(--c-text-2);
}

.mt-sm {
  margin-top: 8px;
}

.code-block {
  margin: 0;
  padding: 12px;
  background: var(--c-surface-2);
  border-radius: var(--r-md);
  font-size: 12px;
  line-height: 1.6;
  max-height: 320px;
  overflow: auto;
  white-space: pre-wrap;
  word-break: break-word;
}

.note {
  margin: 8px 0 0;
  font-size: 11.5px;
}

.foot {
  margin: 12px 0 0;
  font-size: 11.5px;
  text-align: center;
}
</style>
