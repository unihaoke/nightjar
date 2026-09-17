/**
 * 列表页组合式：统一「加载态 / 错误 / 分页 / 筛选同步地址栏 / 竞态防护」。
 *
 * 抽取动机（对应 UX-3 / UX-5 / UX-6，详见 POSTMORTEM 之外的体验评审记录）：
 *   1. 筛选与分页此前只存在于组件内的 `reactive()`，刷新、分享链接、后退全部丢失；
 *   2. 快速翻页或连点查询时，先发后到的响应会覆盖新数据（无竞态防护）；
 *   3. 每个页面各自写一份 loading / error / 空态，改一处要改十处。
 *
 * 用法：
 * ```ts
 * const list = useListPage<Alert>({
 *   fetch: (params, signal) => alertApi.list(params, signal),
 *   defaults: { level: '', status: '', page: 1, page_size: 20 },
 * })
 * ```
 *
 * 约定：
 *   - `defaults` 的键即「本页维护的查询参数」白名单，决定地址栏同步哪些键；
 *   - 与默认值相等的参数不写进 URL，保持地址栏干净；
 *   - 地址栏只做 `replace`（不做 `push`），避免连点「查询」把浏览历史塞满。
 */
import {
  computed,
  onBeforeUnmount,
  onMounted,
  reactive,
  ref,
  watch,
  type ComputedRef,
  type Ref,
} from 'vue'
import { useRoute, useRouter, type LocationQueryRaw } from 'vue-router'
import { toastError, type PageResult } from '@/api/http'
import { useAppStore } from '@/stores/app'

/** 分页字段：不属于业务筛选，单独处理。 */
const PAGE_KEYS = ['page', 'page_size'] as const

/** 取数函数：收到完整查询参数与可选的取消信号。 */
export type ListFetcher<T> = (
  params: Record<string, unknown>,
  signal?: AbortSignal,
) => Promise<PageResult<T>>

export interface ListPageOptions<T> {
  /** 取数实现。第二个参数是本次请求的取消信号，转发给 axios 即可真正中断请求。 */
  fetch: ListFetcher<T>
  /** 查询参数默认值，必须包含 page 与 page_size。 */
  defaults: Record<string, unknown> & { page: number; page_size: number }
  /** 需要按数字还原的查询键（URL 中一律是字符串）。 */
  numberKeys?: readonly string[]
  /** 需要按布尔还原的查询键（如「只看我提交的」）。 */
  booleanKeys?: readonly string[]
  /**
   * 地址栏查询键前缀。同一页面存在多个列表时（如「服务器 + 代码仓库」）必须区分，
   * 否则两个列表会互相覆盖 `page` / `keyword`。
   */
  keyPrefix?: string
  /** 是否把筛选与分页同步到地址栏，默认 true。 */
  syncUrl?: boolean
  /** 是否立即加载，默认 true。 */
  immediate?: boolean
  /**
   * 自动轮询间隔（毫秒），0 表示不支持轮询（页面也不该显示「自动刷新」开关）。
   *
   * 轮询是**静默**的：不弹遮罩、失败不弹 toast（否则后端抖一下就每分钟弹一次错误），
   * 失败只写入 `error` 由 `ResponsiveList` 的空态呈现。
   * 页面切到后台（`document.hidden`）自动暂停，回到前台立即刷新一次。
   */
  pollInterval?: number
  /** 是否静默（不弹 toast，由页面自行处理错误），默认 false。 */
  silent?: boolean
}

export interface ListPage<T> {
  /** 查询参数（可直接 v-model 到筛选控件）。 */
  query: Record<string, unknown> & { page: number; page_size: number }
  /** 当前页数据。 */
  items: Ref<T[]>
  /** 总条数。 */
  total: Ref<number>
  /** 是否加载中。 */
  loading: Ref<boolean>
  /** 最近一次失败的错误文案，成功时为空串。 */
  error: Ref<string>
  /** 已加载完成且无数据（用于渲染空态，避免加载中闪一下空状态）。 */
  isEmpty: ComputedRef<boolean>
  /** 是否移动端视口（供页面切换卡片/表格，也可直接用 ResponsiveList）。 */
  isMobile: ComputedRef<boolean>
  /** 最后一次加载成功的时刻（ISO 字符串），用于展示「更新于」。 */
  lastLoadedAt: Ref<string>
  /** 是否开启自动刷新（仅 pollInterval > 0 时有意义，可直接 v-model 到开关）。 */
  polling: Ref<boolean>
  /** 重新加载当前页（保留筛选与页码）。传 `{ silent: true }` 表示静默刷新。 */
  load: (options?: { silent?: boolean }) => Promise<void>
  /** 应用筛选：回到第 1 页并重新加载。 */
  search: () => void
  /** 重置为默认值并重新加载。 */
  reset: () => void
  /** 切换页码。 */
  setPage: (page: number) => void
  /** 切换每页条数（自动回到第 1 页）。 */
  setPageSize: (size: number) => void
}

/**
 * 创建列表页状态。
 *
 * 竞态防护采用「序号令牌 + AbortController」双保险：
 * 令牌保证过期响应不会覆盖新数据，AbortController 让过期请求尽早中断、少占带宽。
 * 即使 fetch 实现没有转发 signal，令牌依然能挡住覆盖问题。
 */
export function useListPage<T>(options: ListPageOptions<T>): ListPage<T> {
  const route = useRoute()
  const router = useRouter()
  const app = useAppStore()

  const {
    fetch,
    defaults,
    numberKeys = [],
    booleanKeys = [],
    keyPrefix = '',
    syncUrl = true,
    immediate = true,
    pollInterval = 0,
    silent = false,
  } = options

  const query = reactive({ ...defaults }) as unknown as Record<string, unknown> & {
    page: number
    page_size: number
  }

  const items = ref<T[]>([]) as Ref<T[]>
  const total = ref(0)
  const loading = ref(false)
  const error = ref('')
  const lastLoadedAt = ref('')
  /** 自动刷新开关：只有配置了 pollInterval 的页面才可能是 true。 */
  const polling = ref(pollInterval > 0)

  /** 本页维护的全部查询键。 */
  const knownKeys = Object.keys(defaults)

  /** 查询键 → 地址栏键（多列表共用一页时靠前缀隔离）。 */
  function urlKey(key: string): string {
    return keyPrefix ? `${keyPrefix}${key}` : key
  }

  /** 请求序号：只有最后一次请求的结果允许写入。 */
  let seq = 0
  let controller: AbortController | null = null

  /** 把 URL 字符串还原为查询参数的实际类型。 */
  function coerce(key: string, raw: string): unknown {
    if (booleanKeys.includes(key)) {
      return raw === 'true'
    }
    const isNumeric = key === 'page' || key === 'page_size' || numberKeys.includes(key)
    if (!isNumeric) {
      return raw
    }
    const parsed = Number(raw)
    return Number.isFinite(parsed) ? parsed : defaults[key]
  }

  /** 从地址栏读取查询参数，返回是否发生了变化。 */
  function hydrateFromUrl(): boolean {
    if (!syncUrl) {
      return false
    }
    let changed = false
    for (const key of knownKeys) {
      const raw = route.query[urlKey(key)]
      const value = typeof raw === 'string' && raw !== '' ? coerce(key, raw) : defaults[key]
      if (query[key] !== value) {
        query[key] = value
        changed = true
      }
    }
    return changed
  }

  /** 生成写入地址栏的查询串（省略等于默认值的项）。 */
  function buildUrlQuery(): Record<string, string> {
    const next: Record<string, string> = {}
    for (const key of knownKeys) {
      const value = query[key]
      if (value === undefined || value === null || value === '') {
        continue
      }
      if (value === defaults[key]) {
        continue
      }
      next[urlKey(key)] = String(value)
    }
    return next
  }

  /** 把当前查询写回地址栏（只 replace，避免污染浏览历史）。 */
  function syncToUrl(): void {
    if (!syncUrl) {
      return
    }
    // 保留本页不认识的查询键（如从其它页面带过来的 instance_id）。
    const merged: LocationQueryRaw = { ...route.query }
    for (const key of knownKeys) {
      delete merged[urlKey(key)]
    }
    Object.assign(merged, buildUrlQuery())
    void router.replace({ query: merged })
  }

  /** 是否有请求在途（轮询需要据此跳过，避免叠加）。 */
  let inFlight = false

  /**
   * 加载当前页。
   *
   * `silent` 用于自动轮询：不置 loading（避免遮罩每分钟闪一次）、失败不弹 toast
   * （否则后端抖动会变成每分钟一条错误提示），失败原因只体现在 `error` 上。
   */
  async function load(options?: { silent?: boolean }): Promise<void> {
    const quiet = options?.silent === true
    const token = ++seq
    // 中断上一次未完成的请求：用户连点「查询」时不必等旧请求回来。
    controller?.abort()
    controller = new AbortController()

    if (!quiet) {
      loading.value = true
    }
    error.value = ''
    inFlight = true
    try {
      const result = await fetch({ ...query }, controller.signal)
      if (token !== seq) {
        return
      }
      items.value = result?.list ?? []
      total.value = result?.total ?? 0
      lastLoadedAt.value = new Date().toISOString()
    } catch (err) {
      if (token !== seq || (err as Error)?.name === 'AbortError') {
        return
      }
      error.value = err instanceof Error ? err.message : String(err)
      if (!silent && !quiet) {
        toastError(err)
      }
    } finally {
      inFlight = false
      if (token === seq && !quiet) {
        loading.value = false
      }
    }
  }

  function search(): void {
    query.page = 1
    syncToUrl()
    void load()
  }

  function reset(): void {
    for (const key of knownKeys) {
      query[key] = defaults[key]
    }
    syncToUrl()
    void load()
  }

  function setPage(page: number): void {
    if (query.page === page) {
      return
    }
    query.page = page
    syncToUrl()
    void load()
  }

  function setPageSize(size: number): void {
    query.page_size = size
    query.page = 1
    syncToUrl()
    void load()
  }

  // 首次进入：地址栏优先（分享链接 / 刷新恢复）。
  hydrateFromUrl()
  if (immediate) {
    void load()
  }

  // 地址栏被外部改写时（浏览器后退、其它页面带参数跳转过来）同步回查询条件。
  // 自身 syncToUrl 引发的变更会被 hydrateFromUrl 判定为"无变化"，不会重复加载。
  if (syncUrl) {
    watch(
      () => route.query,
      () => {
        if (hydrateFromUrl()) {
          void load()
        }
      },
    )
  }

  // -------------------------------------------------------------------------
  // 自动刷新
  // -------------------------------------------------------------------------
  let timer: number | undefined

  function stopPolling(): void {
    if (timer) {
      window.clearInterval(timer)
      timer = undefined
    }
  }

  function startPolling(): void {
    if (pollInterval <= 0) {
      return
    }
    stopPolling()
    timer = window.setInterval(() => {
      // 后台标签页不刷：既省资源，也避免用户回来时看到一堆"已变化"的数据。
      // 上一次请求还没回来也跳过，避免请求叠加。
      if (document.hidden || inFlight) {
        return
      }
      void load({ silent: true })
    }, pollInterval)
  }

  /** 切回前台：立刻补刷一次，不用等下一个周期。 */
  function onVisibilityChange(): void {
    if (document.hidden) {
      return
    }
    if (polling.value && !inFlight) {
      void load({ silent: true })
    }
  }

  if (polling.value) {
    startPolling()
  }
  watch(polling, (on) => {
    if (on) {
      startPolling()
    } else {
      stopPolling()
    }
  })

  onMounted(() => {
    document.addEventListener('visibilitychange', onVisibilityChange)
  })

  onBeforeUnmount(() => {
    stopPolling()
    document.removeEventListener('visibilitychange', onVisibilityChange)
    // 组件卸载后结果已无意义，中断在途请求。
    seq++
    controller?.abort()
  })

  return {
    query,
    items,
    total,
    loading,
    error,
    isEmpty: computed(() => !loading.value && items.value.length === 0),
    isMobile: computed(() => app.isMobile),
    lastLoadedAt,
    polling,
    load,
    search,
    reset,
    setPage,
    setPageSize,
  }
}

export { PAGE_KEYS }
