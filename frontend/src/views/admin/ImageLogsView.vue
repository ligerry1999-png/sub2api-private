<template>
  <AppLayout>
    <div class="space-y-5">
      <div class="card p-4">
        <div class="flex flex-col gap-3 lg:flex-row lg:items-center">
          <div class="min-w-[260px]">
            <SearchInput v-model="filters.q" placeholder="搜索提示词或请求 ID" @search="applyFilters" />
          </div>
          <DateRangePicker
            v-model:start-date="startDate"
            v-model:end-date="endDate"
            @change="applyFilters"
          />
          <div class="w-full sm:w-44">
            <Select v-model="filters.source" :options="sourceOptions" @change="applyFilters" />
          </div>
          <div class="w-full sm:w-44">
            <Input v-model="filters.model" placeholder="模型" @enter="applyFilters" />
          </div>
          <button class="btn btn-secondary lg:ml-auto" :disabled="loading" @click="resetFilters">
            重置
          </button>
          <button class="btn btn-primary" :disabled="loading" @click="loadLogs">刷新</button>
        </div>
      </div>

      <div class="card overflow-hidden">
        <div class="flex items-center justify-between border-b border-gray-200 px-4 py-3 dark:border-dark-700">
          <div>
            <h2 class="text-base font-semibold text-gray-900 dark:text-white">生图日志</h2>
            <p class="mt-1 text-sm text-gray-500 dark:text-dark-300">
              共 {{ pagination.total }} 条，列表显示缩略图，点开查看原图
            </p>
          </div>
          <LoadingSpinner v-if="loading" size="sm" />
        </div>

        <div v-if="!loading && logs.length === 0" class="py-12">
          <EmptyState title="暂无生图记录" description="新的生图请求完成后会出现在这里。" />
        </div>

        <div v-else class="divide-y divide-gray-100 dark:divide-dark-700">
          <article
            v-for="log in logs"
            :key="log.id"
            class="grid gap-4 p-4 transition-colors hover:bg-gray-50 dark:hover:bg-dark-800/60 lg:grid-cols-[180px_1fr_auto]"
          >
            <button
              type="button"
              class="group relative aspect-square w-full overflow-hidden rounded-lg bg-gray-100 text-left dark:bg-dark-700 lg:w-[180px]"
              :disabled="!log.images.length"
              @click="openPreview(log, log.images[0])"
            >
              <img
                v-if="thumbnailSrc(log, log.images[0])"
                :src="thumbnailSrc(log, log.images[0])"
                :alt="log.prompt"
                class="h-full w-full object-cover transition-transform duration-200 group-hover:scale-[1.02]"
              />
              <div v-else class="flex h-full w-full items-center justify-center text-sm text-gray-400">
                无缩略图
              </div>
              <span
                v-if="log.image_count > 1"
                class="absolute right-2 top-2 rounded-md bg-black/70 px-2 py-1 text-xs font-medium text-white"
              >
                {{ log.image_count }} 张
              </span>
            </button>

            <div class="min-w-0 space-y-3">
              <div class="flex flex-wrap items-center gap-2">
                <span class="rounded-md bg-primary-50 px-2 py-1 text-xs font-medium text-primary-700 dark:bg-primary-900/30 dark:text-primary-300">
                  {{ sourceLabel(log.source) }}
                </span>
                <span class="rounded-md bg-gray-100 px-2 py-1 text-xs font-medium text-gray-700 dark:bg-dark-700 dark:text-dark-200">
                  {{ log.model || '未知模型' }}
                </span>
                <span
                  v-if="log.image_size"
                  class="rounded-md bg-gray-100 px-2 py-1 text-xs text-gray-600 dark:bg-dark-700 dark:text-dark-300"
                >
                  {{ log.image_size }}
                </span>
                <span class="text-xs text-gray-500 dark:text-dark-300">
                  {{ formatDateTime(log.created_at) }}
                </span>
              </div>

              <p class="line-clamp-3 whitespace-pre-wrap text-sm leading-6 text-gray-900 dark:text-white">
                {{ log.prompt || '无提示词' }}
              </p>

              <div class="grid gap-2 text-sm text-gray-500 dark:text-dark-300 sm:grid-cols-2 xl:grid-cols-4">
                <div class="truncate">用户：{{ log.user?.email || `#${log.user_id}` }}</div>
                <div class="truncate">密钥：{{ log.api_key?.name || `#${log.api_key_id}` }}</div>
                <div class="truncate">账号：{{ log.account?.name || '未记录' }}</div>
                <div class="truncate">耗时：{{ formatDuration(log.duration_ms) }}</div>
                <div class="truncate">阶段：{{ routeDurationLabel(log) }}</div>
              </div>
            </div>

            <div class="flex items-start justify-end gap-2">
              <button
                class="btn btn-ghost btn-sm"
                :disabled="!log.images.length"
                @click="openPreview(log, log.images[0])"
              >
                查看
              </button>
            </div>
          </article>
        </div>

        <Pagination
          v-if="pagination.total > 0"
          :page="pagination.page"
          :total="pagination.total"
          :page-size="pagination.page_size"
          @update:page="handlePageChange"
          @update:pageSize="handlePageSizeChange"
        />
      </div>
    </div>
  </AppLayout>

  <BaseDialog
    :show="previewVisible"
    title="生图详情"
    width="full"
    :close-on-click-outside="true"
    @close="closePreview"
  >
    <div v-if="selectedLog" class="grid gap-5 lg:grid-cols-[minmax(0,1fr)_360px]">
      <div class="flex min-h-[360px] items-center justify-center rounded-lg bg-gray-100 p-3 dark:bg-dark-900">
        <LoadingSpinner v-if="previewLoading" size="lg" />
        <img
          v-else-if="previewDataURL"
          :src="previewDataURL"
          :alt="selectedLog.prompt"
          class="max-h-[72vh] max-w-full rounded-md object-contain"
        />
        <div v-else class="text-sm text-gray-500 dark:text-dark-300">原图加载失败</div>
      </div>
      <div class="space-y-4">
        <div>
          <div class="text-xs font-medium uppercase tracking-wide text-gray-400">提示词</div>
          <p
            class="mt-2 max-h-56 overflow-y-auto whitespace-pre-wrap rounded-lg bg-gray-50 p-3 text-sm leading-6 text-gray-900 dark:bg-dark-800 dark:text-white"
          >
            {{ selectedLog.prompt || '无提示词' }}
          </p>
        </div>
        <div class="space-y-2 text-sm text-gray-600 dark:text-dark-300">
          <div>用户：{{ selectedLog.user?.email || `#${selectedLog.user_id}` }}</div>
          <div>密钥：{{ selectedLog.api_key?.name || `#${selectedLog.api_key_id}` }}</div>
          <div>账号：{{ selectedLog.account?.name || '未记录' }}</div>
          <div>通道：{{ sourceLabel(selectedLog.source) }}</div>
          <div>模型：{{ selectedLog.model || '未知模型' }}</div>
          <div>时间：{{ formatDateTime(selectedLog.created_at) }}</div>
          <div>耗时：{{ formatDuration(selectedLog.duration_ms) }}</div>
          <div>阶段耗时：{{ routeDurationLabel(selectedLog) }}</div>
          <div>请求 ID：{{ selectedLog.request_id || '未记录' }}</div>
        </div>
        <div v-if="selectedLog.images.length > 1" class="grid grid-cols-4 gap-2">
          <button
            v-for="img in selectedLog.images"
            :key="img.index"
            class="aspect-square overflow-hidden rounded-md border border-gray-200 bg-gray-100 dark:border-dark-600 dark:bg-dark-700"
            :class="selectedImage?.index === img.index ? 'ring-2 ring-primary-500' : ''"
            @click="openPreview(selectedLog, img)"
          >
            <img
              v-if="thumbnailSrc(selectedLog, img)"
              :src="thumbnailSrc(selectedLog, img)"
              class="h-full w-full object-cover"
              alt=""
            />
          </button>
        </div>
      </div>
    </div>
  </BaseDialog>
</template>

<script setup lang="ts">
import { onBeforeUnmount, onMounted, reactive, ref } from 'vue'
import { AxiosError } from 'axios'
import AppLayout from '@/components/layout/AppLayout.vue'
import BaseDialog from '@/components/common/BaseDialog.vue'
import DateRangePicker from '@/components/common/DateRangePicker.vue'
import EmptyState from '@/components/common/EmptyState.vue'
import Input from '@/components/common/Input.vue'
import LoadingSpinner from '@/components/common/LoadingSpinner.vue'
import Pagination from '@/components/common/Pagination.vue'
import SearchInput from '@/components/common/SearchInput.vue'
import Select from '@/components/common/Select.vue'
import { adminAPI } from '@/api/admin'
import type { ImageLog, ImageLogImage } from '@/api/admin/imageLogs'
import { useAppStore } from '@/stores/app'
import { formatDateTime } from '@/utils/format'

const appStore = useAppStore()
const logs = ref<ImageLog[]>([])
const loading = ref(false)
const previewVisible = ref(false)
const previewLoading = ref(false)
const previewDataURL = ref('')
const selectedLog = ref<ImageLog | null>(null)
const selectedImage = ref<ImageLogImage | null>(null)
const thumbnailURLs = ref<Record<string, string>>({})
let abortController: AbortController | null = null
let thumbnailLoadSeq = 0

const pagination = reactive({
  page: 1,
  page_size: 20,
  total: 0
})

const filters = reactive({
  q: '',
  source: '',
  model: ''
})

const sourceOptions = [
  { label: '全部通道', value: '' },
  { label: 'ChatGPT2API', value: 'chatgpt2api_worker' },
  { label: 'Sub2API 兜底', value: 'sub2api_native' }
]

const formatLocalDate = (date: Date) => {
  const year = date.getFullYear()
  const month = String(date.getMonth() + 1).padStart(2, '0')
  const day = String(date.getDate()).padStart(2, '0')
  return `${year}-${month}-${day}`
}

const startDate = ref(formatLocalDate(new Date(Date.now() - 7 * 24 * 60 * 60 * 1000)))
const endDate = ref(formatLocalDate(new Date()))

const sourceLabel = (source: string) => {
  if (source === 'chatgpt2api_worker') return 'ChatGPT2API'
  if (source === 'sub2api_native') return 'Sub2API 兜底'
  return source || '未知通道'
}

const formatDuration = (ms?: number | null) => {
  if (ms == null) return '未记录'
  if (ms < 1000) return `${ms} ms`
  return `${(ms / 1000).toFixed(1)} s`
}

const metadataNumber = (log: ImageLog, key: string) => {
  const value = log.metadata?.[key]
  if (typeof value === 'number' && Number.isFinite(value)) return value
  if (typeof value === 'string') {
    const parsed = Number(value)
    if (Number.isFinite(parsed)) return parsed
  }
  return null
}

const routeDurationLabel = (log: ImageLog) => {
  const worker = metadataNumber(log, 'worker_duration_ms')
  if (worker != null) return `网关 ${formatDuration(worker)}`
  const native = metadataNumber(log, 'native_duration_ms')
  if (native != null) return `原生 ${formatDuration(native)}`
  return '未记录'
}

const imageKey = (log: ImageLog, image?: ImageLogImage) => {
  if (!image) return ''
  return `${log.id}:${image.index}`
}

const thumbnailSrc = (log: ImageLog | null, image?: ImageLogImage) => {
  if (!log || !image) return ''
  const key = imageKey(log, image)
  return thumbnailURLs.value[key] || image.thumbnail_data_url || ''
}

const revokeThumbnailURLs = (keepKeys = new Set<string>()) => {
  const next: Record<string, string> = {}
  for (const [key, url] of Object.entries(thumbnailURLs.value)) {
    if (keepKeys.has(key)) {
      next[key] = url
      continue
    }
    URL.revokeObjectURL(url)
  }
  thumbnailURLs.value = next
}

const loadThumbnails = async (items: ImageLog[]) => {
  const seq = ++thumbnailLoadSeq
  const nextKeys = new Set<string>()
  const targets: Array<{ log: ImageLog; image: ImageLogImage; key: string }> = []
  for (const log of items) {
    for (const image of log.images) {
      const key = imageKey(log, image)
      if (!key) continue
      nextKeys.add(key)
      if (!thumbnailURLs.value[key]) {
        targets.push({ log, image, key })
      }
    }
  }
  revokeThumbnailURLs(nextKeys)
  await Promise.allSettled(
    targets.map(async ({ log, image, key }) => {
      const url = await adminAPI.imageLogs.getThumbnailObjectURL(log.id, image.index)
      if (seq !== thumbnailLoadSeq || !nextKeys.has(key)) {
        URL.revokeObjectURL(url)
        return
      }
      thumbnailURLs.value = { ...thumbnailURLs.value, [key]: url }
    })
  )
}

const loadLogs = async () => {
  abortController?.abort()
  abortController = new AbortController()
  loading.value = true
  try {
    const res = await adminAPI.imageLogs.list(
      {
        page: pagination.page,
        page_size: pagination.page_size,
        start_date: startDate.value,
        end_date: endDate.value,
        q: filters.q || undefined,
        source: filters.source || undefined,
        model: filters.model || undefined
      },
      { signal: abortController.signal }
    )
    logs.value = res.items
    pagination.total = res.total
    pagination.page = res.page
    pagination.page_size = res.page_size
    void loadThumbnails(res.items)
  } catch (error) {
    if (error instanceof AxiosError && error.code === 'ERR_CANCELED') return
    appStore.showError('加载生图日志失败')
  } finally {
    loading.value = false
  }
}

const applyFilters = () => {
  pagination.page = 1
  void loadLogs()
}

const resetFilters = () => {
  filters.q = ''
  filters.source = ''
  filters.model = ''
  pagination.page = 1
  void loadLogs()
}

const handlePageChange = (page: number) => {
  pagination.page = page
  void loadLogs()
}

const handlePageSizeChange = (pageSize: number) => {
  pagination.page_size = pageSize
  pagination.page = 1
  void loadLogs()
}

const openPreview = async (log: ImageLog, image?: ImageLogImage) => {
  if (!image) return
  selectedLog.value = log
  selectedImage.value = image
  previewVisible.value = true
  previewLoading.value = true
  previewDataURL.value = ''
  try {
    previewDataURL.value = await adminAPI.imageLogs.getImageDataURL(log.id, image.index)
  } catch {
    appStore.showError('加载原图失败')
  } finally {
    previewLoading.value = false
  }
}

const closePreview = () => {
  previewVisible.value = false
  previewDataURL.value = ''
  selectedLog.value = null
  selectedImage.value = null
}

onMounted(() => {
  void loadLogs()
})

onBeforeUnmount(() => {
  abortController?.abort()
  thumbnailLoadSeq++
  revokeThumbnailURLs()
})
</script>
