# 生图中转站第三方对接文档

> 最后核验日期：2026-07-22
>
> 适用对象：需要通过 Sub2API 调用生图模型的第三方开发者

## 1. 最重要的信息

Base URL（接口根地址）：就是所有 API 请求共同使用的开头地址。它的作用是告诉程序请求要发到哪台中转站。

```text
https://sub2api.tanzhongyu.asia/v1
```

推荐模型：

```text
gpt-image-2
```

API Key（访问密钥）：就是调用接口时使用的通行证。它的作用是识别调用者、检查权限并记录用量。

请登录中转站后，在 API Key 页面自行创建，不要向其他人共享自己的 Key。

```text
https://sub2api.tanzhongyu.asia/keys
```

新建 Key 时必须确认：

- Key 绑定的是 OpenAI 分组。
- 该分组已经开启生图权限。
- Key 有足够余额或可用额度。

如果 Key 能调用文字模型，但生图返回 `403`，通常是分组没有开启生图权限，需要联系管理员处理。

## 2. 鉴权方式

每次请求都要带下面这个 HTTP Header（请求头）：就是随请求一起发送的身份信息。它的作用是把 API Key 交给中转站验证。

```http
Authorization: Bearer sk-your-sub2api-key
```

下面的示例统一使用环境变量，避免把真实 Key 直接写进代码：

```bash
export SUB2API_BASE_URL="https://sub2api.tanzhongyu.asia/v1"
export SUB2API_API_KEY="sk-your-sub2api-key"
```

不要把 Key 放进网页前端、公开 Git 仓库、截图或聊天记录。推荐由自己的服务器调用中转站。

## 3. 推荐方式：异步生图

Async / 异步任务（先提交、后取结果）：就是先让服务器接收任务，再每隔几秒查询一次是否完成。它的作用是避免生图时间太长，导致一次 HTTP 连接中途超时。

### 3.1 提交任务

Endpoint（接口路径）：就是 Base URL 后面追加的具体功能地址。它的作用是告诉中转站这次要创建一张图片。

```text
POST /image-jobs/images/generations
```

完整地址：

```text
https://sub2api.tanzhongyu.asia/v1/image-jobs/images/generations
```

curl 示例：

```bash
curl -sS "$SUB2API_BASE_URL/image-jobs/images/generations" \
  -H "Authorization: Bearer $SUB2API_API_KEY" \
  -H "Content-Type: application/json" \
  -H "X-Request-Id: demo-image-001" \
  -d '{
    "model": "gpt-image-2",
    "prompt": "一只坐在书桌前工作的橘猫，柔和自然光，画面中不要出现文字",
    "n": 1,
    "size": "1024x1536",
    "quality": "high",
    "result_delivery": "file_url"
  }'
```

正常情况下会返回 HTTP `202`。这里的 `202` 只表示“任务已经收下”，不表示图片已经生成完成。

返回示例：

```json
{
  "job_id": "imgjob_0123456789abcdef",
  "status": "pending",
  "model": "gpt-image-2",
  "status_url": "https://sub2api.tanzhongyu.asia/v1/image-jobs/imgjob_0123456789abcdef",
  "result_url": "https://sub2api.tanzhongyu.asia/v1/image-jobs/imgjob_0123456789abcdef/result",
  "cancel_url": "https://sub2api.tanzhongyu.asia/v1/image-jobs/imgjob_0123456789abcdef/cancel"
}
```

请保存 `job_id`。后续查询状态、取结果和取消任务都要使用它，而且必须继续使用提交任务时的同一个 API Key。

### 3.2 查询任务状态

Polling / 轮询（定时查询）：就是每隔几秒询问一次任务状态。它的作用是在不保持长连接的情况下等待生图完成。

建议每 5 秒查询一次，不要每秒连续请求。

```bash
export JOB_ID="imgjob_0123456789abcdef"

curl -sS "$SUB2API_BASE_URL/image-jobs/$JOB_ID" \
  -H "Authorization: Bearer $SUB2API_API_KEY"
```

可能出现的状态：

| 状态 | 含义 | 应该怎么做 |
|---|---|---|
| `pending` | 正在等待执行 | 5 秒后继续查询 |
| `running` | 正在生成图片 | 5 秒后继续查询 |
| `success` | 已生成成功 | 请求 `result_url` 获取结果 |
| `failed` | 生成失败 | 查看 `error_type` 和 `retryable` |
| `canceled` | 任务已取消 | 不再查询 |

失败示例：

```json
{
  "job_id": "imgjob_0123456789abcdef",
  "status": "failed",
  "error": "upstream request failed",
  "error_type": "upstream_model_error",
  "retryable": true,
  "http_status": 502
}
```

只有 `retryable: true` 时才建议重试。重试前先等待 10 到 30 秒，避免连续请求让上游更加拥堵。

### 3.3 获取图片结果

状态变为 `success` 后，请求：

```bash
curl -sS "$SUB2API_BASE_URL/image-jobs/$JOB_ID/result" \
  -H "Authorization: Bearer $SUB2API_API_KEY"
```

如果提交任务时传了：

```json
"result_delivery": "file_url"
```

结果通常会包含图片 URL：

```json
{
  "created": 1780000000,
  "data": [
    {
      "url": "https://sub2api.tanzhongyu.asia/image-files/image_jobs/.../result.png"
    }
  ]
}
```

请及时把图片下载到自己的存储中，不要把中转站返回的 URL 当成永久网盘地址。

如果没有传 `result_delivery=file_url`，结果可能是 `b64_json`。Base64（把图片转成文本）：就是把二进制图片编码成一长串字符。它的作用是让图片可以直接放进 JSON 返回，但数据会比较大。

### 3.4 当前图片存储边界

截至 2026-07-27，私有版 `/v1/image-jobs/*` 的 `file_url` 结果仍保存在 Sub2API 服务器本地磁盘，并通过 `/image-files/...` 下载。它还没有接入腾讯云 COS、AWS S3 或其他对象存储。

公开版另有 `/v1/images/generations/async`、`/v1/images/edits/async` 和 `/v1/images/tasks/{task_id}` 三个异步接口。公开版会把这些接口产生的图片转存到 S3 兼容对象存储，再把短 URL 放入 Redis。这个公开能力不能直接替换私有 `/v1/image-jobs/*`，否则会破坏现有客户端的接口路径、失败分类、取消接口和默认 Base64 行为。

后续如果接入对象存储，应保持本章现有接口不变，只把任务成功后的图片文件从服务器本地目录改为“可选上传对象存储”，并继续保证：

- 未传 `result_delivery=file_url` 时仍兼容 Base64。
- 已传 `result_delivery=file_url` 时返回调用方可下载的 URL。
- 对象存储上传失败时有明确错误，不能返回一个实际不存在的成功地址。
- 图片日志、异步任务结果和 ChatGPT2API 图片副本分别设置保留策略，不能混成一个清理规则。

## 4. 完整 Python 示例

先安装依赖：

```bash
pip install requests
```

代码：

```python
import base64
import os
import time

import requests


BASE_URL = os.environ.get(
    "SUB2API_BASE_URL",
    "https://sub2api.tanzhongyu.asia/v1",
)
API_KEY = os.environ["SUB2API_API_KEY"]

headers = {
    "Authorization": f"Bearer {API_KEY}",
    "Content-Type": "application/json",
}

# 1. 提交任务
submit_response = requests.post(
    f"{BASE_URL}/image-jobs/images/generations",
    headers=headers,
    json={
        "model": "gpt-image-2",
        "prompt": "一只坐在书桌前工作的橘猫，柔和自然光，画面中不要出现文字",
        "n": 1,
        "size": "1024x1536",
        "quality": "high",
        "result_delivery": "file_url",
    },
    timeout=30,
)
submit_response.raise_for_status()
job = submit_response.json()
job_id = job["job_id"]

print("任务已提交：", job_id)

# 2. 每 5 秒查询一次，最多等待 10 分钟
deadline = time.time() + 600
while True:
    status_response = requests.get(
        f"{BASE_URL}/image-jobs/{job_id}",
        headers=headers,
        timeout=15,
    )
    status_response.raise_for_status()
    status_data = status_response.json()
    status = status_data["status"]

    print("当前状态：", status)

    if status == "success":
        break

    if status in {"failed", "canceled"}:
        raise RuntimeError(
            f"任务失败：error_type={status_data.get('error_type')}, "
            f"retryable={status_data.get('retryable')}, "
            f"error={status_data.get('error')}"
        )

    if time.time() >= deadline:
        raise TimeoutError(
            "等待已超过 10 分钟。请先查询或取消原任务，不要直接重复提交。"
        )

    time.sleep(5)

# 3. 获取结果
result_response = requests.get(
    f"{BASE_URL}/image-jobs/{job_id}/result",
    headers=headers,
    timeout=60,
)
result_response.raise_for_status()
result = result_response.json()
image = result["data"][0]

if image.get("url"):
    image_response = requests.get(image["url"], timeout=120)
    image_response.raise_for_status()
    with open("generated.png", "wb") as file:
        file.write(image_response.content)
elif image.get("b64_json"):
    with open("generated.png", "wb") as file:
        file.write(base64.b64decode(image["b64_json"]))
else:
    raise RuntimeError("返回结果里没有 url 或 b64_json")

print("图片已保存到 generated.png")
```

## 5. 兼容方式：同步生图

Sync / 同步请求（一次请求等待到底）：就是提交后保持连接，直到图片生成完成。它的作用是让接入代码更简单，但生成较慢时更容易遇到网络超时。

接口：

```text
POST /images/generations
```

curl 示例：

```bash
curl -sS --max-time 600 "$SUB2API_BASE_URL/images/generations" \
  -H "Authorization: Bearer $SUB2API_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-image-2",
    "prompt": "一只坐在书桌前工作的橘猫，柔和自然光，画面中不要出现文字",
    "n": 1,
    "size": "1024x1536",
    "quality": "high",
    "result_delivery": "file_url"
  }'
```

如果第三方程序使用 OpenAI SDK（OpenAI 官方客户端库），也可以把 `base_url` 改成中转站地址：

```python
import os

from openai import OpenAI


client = OpenAI(
    api_key=os.environ["SUB2API_API_KEY"],
    base_url="https://sub2api.tanzhongyu.asia/v1",
    timeout=600,
)

result = client.images.generate(
    model="gpt-image-2",
    prompt="一只坐在书桌前工作的橘猫，柔和自然光，画面中不要出现文字",
    n=1,
    size="1024x1536",
    quality="high",
)

print(result.data[0])
```

生产环境优先使用第 3 节的异步任务接口。同步接口更适合本地测试或预计很快完成的请求。

## 6. 图片编辑

图片编辑使用 multipart/form-data（同时上传文字和文件的表单格式）：就是一个请求里既能放提示词，也能放图片文件。它的作用是把参考图上传给生图模型进行修改。

异步编辑接口：

```text
POST /image-jobs/images/edits
```

示例：

```bash
curl -sS "$SUB2API_BASE_URL/image-jobs/images/edits" \
  -H "Authorization: Bearer $SUB2API_API_KEY" \
  -F "model=gpt-image-2" \
  -F "prompt=保留主体不变，把背景改成日落海边，画面中不要出现文字" \
  -F "image=@reference.png" \
  -F "size=1024x1536" \
  -F "quality=high" \
  -F "result_delivery=file_url"
```

建议使用 PNG、JPEG 或 WebP 图片，单个上传文件不要超过 20 MB。提交后仍然使用第 3 节的状态和结果接口查询。

## 7. 查看当前可用模型

模型可能随着上游账号和管理员配置变化。程序可以调用：

```bash
curl -sS "$SUB2API_BASE_URL/models" \
  -H "Authorization: Bearer $SUB2API_API_KEY"
```

2026-07-22 线上只读核验时，生图模型列表包含：

```text
gpt-image-1
gpt-image-1.5
gpt-image-2
```

新项目推荐使用 `gpt-image-2`。

## 8. 常用参数

| 参数 | 是否必填 | 示例 | 说明 |
|---|---:|---|---|
| `model` | 是 | `gpt-image-2` | 使用哪个生图模型 |
| `prompt` | 是 | `一只橘猫` | 希望模型生成什么内容 |
| `n` | 否 | `1` | 一次生成几张，建议先用 1 |
| `size` | 否 | `1024x1536` | 希望输出的尺寸或画幅 |
| `quality` | 否 | `high` | 可使用 `low`、`medium`、`high` 或 `auto` |
| `result_delivery` | 否 | `file_url` | 返回图片 URL；不传时保持 Base64 兼容行为 |

重要限制：`size` 是请求给上游的期望值，不代表当前订阅上游一定会严格返回相同像素。调用方必须在下载图片后读取真实宽高，不能只相信请求参数。

截至 2026-07-20 的线上实测中，请求 `2160x3840`，实际返回过 `941x1672`。因此当前链路不适合承诺“原生 4K”。普通内容建议先使用 `1024x1024`、`1536x1024` 或 `1024x1536`。

## 9. 常见错误

| HTTP 状态码 | 常见原因 | 处理方式 |
|---:|---|---|
| `400` | 参数缺失、格式错误、上传文件不符合要求 | 检查请求 JSON 或上传表单 |
| `401` | Key 缺失、错误、失效 | 重新创建或检查 Key |
| `403` | Key 所属分组没有生图权限 | 联系管理员开启分组生图权限 |
| `404` | 地址写错、任务不存在、任务不属于当前 Key | 检查 Base URL、路径、job_id 和 Key |
| `409` | 获取结果时任务已经失败或被取消 | 先查询状态接口读取失败原因 |
| `429` | 额度不足、请求过快或上游限流 | 等待后重试，检查余额和额度 |
| `502` | 上游生图服务失败 | 查看任务的 `retryable`，不要立即连续重试 |

## 10. 接入检查清单

正式接入前逐项确认：

- Base URL 精确写成 `https://sub2api.tanzhongyu.asia/v1`。
- 没有重复写成 `/v1/v1/...`。
- Key 通过环境变量或服务器密钥管理工具保存。
- Key 所属分组已经开启生图权限。
- 优先使用异步任务接口。
- 每 5 秒轮询一次，不要高频查询。
- 任务超时后先查询或取消，不要直接重复提交。
- 只在 `retryable: true` 时自动重试。
- 下载后检查图片真实宽高。
- 及时把结果保存到自己的存储中。
