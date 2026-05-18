import { apiClient } from '../client'
import type { PaginatedResponse } from '@/types'

export interface ImageLogPerson {
  id: number
  email?: string
  username?: string
  name?: string
}

export interface ImageLogImage {
  index: number
  mime_type: string
  thumbnail_data_url?: string
  size_bytes: number
  width?: number
  height?: number
}

export interface ImageLog {
  id: number
  user_id: number
  api_key_id: number
  account_id?: number
  group_id?: number
  request_id: string
  source: string
  endpoint: string
  model: string
  prompt: string
  status: string
  error_message?: string | null
  image_count: number
  image_size?: string | null
  duration_ms?: number | null
  created_at: string
  user?: ImageLogPerson
  api_key?: ImageLogPerson
  account?: ImageLogPerson
  group?: ImageLogPerson
  images: ImageLogImage[]
}

export interface ImageLogQueryParams {
  page?: number
  page_size?: number
  start_date?: string
  end_date?: string
  user_id?: number
  api_key_id?: number
  account_id?: number
  group_id?: number
  model?: string
  source?: string
  status?: string
  q?: string
}

export async function list(
  params: ImageLogQueryParams,
  options?: { signal?: AbortSignal }
): Promise<PaginatedResponse<ImageLog>> {
  const { data } = await apiClient.get<PaginatedResponse<ImageLog>>('/admin/image-logs', {
    params,
    signal: options?.signal
  })
  return data
}

export async function getImageDataURL(id: number, index: number): Promise<string> {
  const { data } = await apiClient.get<Blob>(`/admin/image-logs/${id}/images/${index}`, {
    responseType: 'blob'
  })
  return new Promise((resolve, reject) => {
    const reader = new FileReader()
    reader.onload = () => resolve(String(reader.result || ''))
    reader.onerror = () => reject(reader.error || new Error('Failed to read image'))
    reader.readAsDataURL(data)
  })
}

const imageLogsAPI = {
  list,
  getImageDataURL
}

export default imageLogsAPI
