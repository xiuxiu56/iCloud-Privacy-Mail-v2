# iCloud Privacy Mail

本地运行的 Apple 隐私邮箱管理工具。后端使用 Go，前端使用 Vue 3，账号、邮箱、邮件、任务和系统设置统一保存在 SQLite。

## 环境要求

- Go 1.25 或更高版本
- Node.js 20.19+ 或 22.12+
- npm

## 快速启动

首次运行先复制配置文件：

```bash
cp config.example.json config.json
```

直接启动：

```bash
go run . -config config.json
```

打开：

```text
http://127.0.0.1:8788/
```

首次打开会要求创建本地管理员，项目没有默认账号或密码。

## 构建

一键构建前端和 Go 服务：

```bash
./scripts/build.sh
```

构建产物：

```text
bin/ipm-server
```

启动构建产物：

```bash
./bin/ipm-server -config config.json
```

只重新构建前端并同步到 Go 嵌入目录：

```bash
npm --prefix frontend ci
npm --prefix frontend run build
./scripts/sync-web.sh
```

## 开发模式

```bash
./scripts/dev.sh
```

- 前端：http://127.0.0.1:5174/
- 后端：http://127.0.0.1:8788/

## 测试

```bash
go test ./...
go vet ./...
npm --prefix frontend run build
```

## 常用配置

配置文件默认为 `config.json`，完整字段可参考 `config.example.json`。

| 配置 | 说明 |
| --- | --- |
| `host` / `port` | 服务监听地址和端口 |
| `data_path` | SQLite 数据库路径 |
| `secure_cookie` | HTTPS 部署时启用安全 Cookie |
| `api_key` | 公共取号 API Key |
| `database_backup_dir` | SQLite 备份目录 |
| `database_backup_retention_count` | 自动或手动备份后最多保留的份数，默认 3 |
| `server_chan_send_key` | Server 酱 SendKey |

SQLite 主数据库和密钥文件必须成对保留：

```text
data/app.db
data/app.db.key
```

服务启动 1 分钟后执行首轮自动备份，此后每 24 小时备份一次。系统设置页也可以点击“立即备份”。每次备份后默认只保留最新 3 份 `.db` 和 `.db.key` 文件。

## 项目地址

源码仓库：<https://github.com/xiuxiu56/iCloud-Privacy-Mail-v2>

## 版本与公告

系统设置的更新检查只读取 [`internal/updatecheck/announcements.json`](./internal/updatecheck/announcements.json)。发布新版本时更新 `latest`；项目消息放入 `announcements`。该方式使用 GitHub Raw 公开文件，不请求 GitHub REST API。

```json
{
  "schema_version": 1,
  "latest": {
    "version": "2.2.4",
    "name": "2.2.4 源码版",
    "notes": "自动创建单账号失败退出、总运行时间及邮箱池取码 API 链接已更新",
    "published_at": "2026-10-01T18:28:02+08:00",
    "url": "https://github.com/xiuxiu56/iCloud-Privacy-Mail-v2/archive/refs/heads/domain-mailbox-system.zip"
  },
  "announcements": []
}
```

## 主题模式

### 亮色模式

![亮色模式](./docs/screenshots/09-light-mode.jpg)

### 深色模式

![深色模式](./docs/screenshots/08-dark-mode.jpg)

## 外部邮箱 API

在系统设置中开启“公共取号 API”，设置全局 API Key 后调用领取接口。接口会把符合条件的可用邮箱建立租约，重复提交相同的 `project` 和 `request_id` 会返回原租约。

```http
POST /api/v1/mailboxes/claim
X-API-Key: YOUR_API_KEY
Content-Type: application/json
```

### 请求参数

| 参数 | 必填 | 说明 |
| --- | --- | --- |
| `project` | 是 | 项目标识，不能为空。 |
| `purpose` | 否 | 本次领取用途。 |
| `request_id` | 否 | 请求幂等标识；同一项目重复提交会返回原租约。 |
| `note` | 否 | 写入邮箱和租约的备注。 |
| `ttl_seconds` | 否 | 租约秒数；小于 60 秒会按 60 秒处理，超过系统上限会按上限处理。 |
| `mailbox_kind` | 否 | `icloud_hme`、`domain_forward`、`any` 或留空。 |
| `domain` | 否 | 只用于 `domain_forward`，按收件域名筛选。 |
| `apple_id` | 否 | 页面显示的 Apple ID 邮箱，例如 `name@example.com`；系统会自动解析对应账号。 |
| `account_id` | 否 | 系统内部 Apple 账号 ID，也可以传 Apple ID 邮箱。 |

`apple_id` 和 `account_id` 同时填写时，以 `apple_id` 为准。

### 领取 iCloud 隐私邮箱

#### 从所有 Apple 账号领取

`apple_id` 为空或省略时，从所有符合条件的 iCloud 隐私邮箱中按 `created_at` 从早到晚领取第一个：

```json
{
  "project": "注册任务",
  "purpose": "创建账号",
  "request_id": "icloud-all-001",
  "mailbox_kind": "icloud_hme",
  "apple_id": "",
  "ttl_seconds": 1800
}
```

#### 指定页面显示的 Apple ID

可以直接填写 Apple 账号页面显示的邮箱。系统会先定位账号，再在该账号的隐私邮箱中按 `created_at` 从早到晚领取第一个：

```json
{
  "project": "注册任务",
  "purpose": "创建账号",
  "request_id": "icloud-account-001",
  "mailbox_kind": "icloud_hme",
  "apple_id": "name@example.com",
  "ttl_seconds": 1800
}
```

也可以使用另一个页面显示的 Apple ID：

```json
{
  "project": "注册任务",
  "purpose": "创建账号",
  "request_id": "icloud-account-002",
  "mailbox_kind": "icloud_hme",
  "apple_id": "another-account@example.com"
}
```

### 领取域名邮箱

#### 从所有域名邮箱领取

`domain` 为空或省略时，从所有已启用的域名邮箱中按 `created_at` 从早到晚领取第一个：

```json
{
  "project": "注册任务",
  "purpose": "创建账号",
  "request_id": "domain-all-001",
  "mailbox_kind": "domain_forward",
  "domain": "",
  "ttl_seconds": 1800
}
```

#### 指定接收域名

```json
{
  "project": "注册任务",
  "purpose": "创建账号",
  "request_id": "domain-xiummm-001",
  "mailbox_kind": "domain_forward",
  "domain": "xiummm.com",
  "ttl_seconds": 1800
}
```

域名邮箱已绑定 Apple 账号时，也可以同时填写 `apple_id`，在指定域名内继续按 Apple 账号筛选。

### 任意来源邮箱

`mailbox_kind` 填写 `any` 或省略时，会在 iCloud 隐私邮箱和已启用的域名邮箱中统一选择。需要固定来源时，请明确填写 `icloud_hme` 或 `domain_forward`。

### 领取响应

成功响应的 `data` 包含 `mailbox` 和 `lease`：

```json
{
  "success": true,
  "data": {
    "mailbox": {
      "email": "example@icloud.com",
      "account_id": "acc_xxxxxxxxx",
      "mailbox_kind": "icloud_hme",
      "status": "reserved"
    },
    "lease": {
      "id": "lease_xxxxxxxxx",
      "email": "example@icloud.com",
      "state": "claimed",
      "expires_at": "2026-09-28T12:00:00Z"
    },
    "created": true,
    "idempotent": false
  }
}
```

没有匹配的可用邮箱时，响应中的 `code` 为 `no_available_mailbox`。Apple ID 不存在时，响应中的 `code` 为 `apple_account_not_found`。

取码、邮件列表和完整正文对两类邮箱使用相同接口，后端会根据邮箱记录选择对应收件链路：

```text
GET /api/v1/mailboxes/{email}/code
GET /api/v1/mailboxes/{email}/messages
GET /api/v1/mailboxes/{email}/messages/{message_id}
```

邮箱独立 API Token、全局 API Key、`X-API-Key` 和 `Authorization: Bearer` 的鉴权方式保持一致。响应中的 `mailbox_kind` 用于确认邮箱来源。

## iCloud Web 邮件同步接口

下面这些是后端内部调用的 iCloud 邮件网关接口，不能直接当作本项目的公共 API 使用。登录态 Cookie、`dsid` 和 `clientId` 由已保存的 iCloud Web 会话提供，文档不保存真实账号或 Cookie。

| 用途 | 接口 |
| --- | --- |
| 邮箱夹列表 | `POST /mailws2/v1/geqs/query?clientIntent=fetchMailboxCountQuery` |
| 增量线程同步 | `POST /mailws2/v1/thread/search`，请求体使用 `THREAD_DIGEST` |
| 全量线程同步 | `POST /mailws2/v1/thread/search`，请求体使用 `THREAD_ID_AND_DATE` 和 `includeFolderStatus: true` |
| 线程邮件元数据 | `POST /mailws2/v1/thread/get` |
| 邮件正文 | `POST /mailws2/v1/message/get` |

邮箱池内容同步的全量和增量流程都使用抓包中的 `thread/search`，再调用 `thread/get` 做收件地址匹配；`fetchCategoryView` 属于网页分类初始化查询，不作为邮箱池内容同步入口。邮件网关请求使用抓包中的 `2634Hotfix39`，门户接口使用 `2634Build50`。Apple 更新网页版本后，应重新抓取并核对构建号、请求体和响应字段。

抓包中还出现了 `fetchMailboxQuery`、`fetchRemindMeQuery`、`fetchAccountPref`、`fetchMostRecentMessageTimestamp` 和 `fetchMessageMetadataByThreadIds`。这些是网页初始化、提醒或批量元数据查询，当前收件同步不依赖它们；如果要完全复刻网页行为，还需要对应的响应体来核对字段。

## 页面

### 登录

![登录页面](./docs/screenshots/00-login.jpg)

### 控制台

![控制台](./docs/screenshots/01-dashboard.jpg)

### Apple 账号

![Apple 账号](./docs/screenshots/02-apple-accounts.jpg)

Apple 账号表格操作列的电源图标用于停用或开启账号。停用后不会参与自动创建、后台邮件监听、邮箱池内容同步或公共取号；登录态和已关联邮箱会保留，重新点击电源图标即可恢复。

账号详情中三个登录通道各自带有检测图标，可以单独检测：

```http
POST /api/apple-accounts/{id}/check/apple_account
POST /api/apple-accounts/{id}/check/icloud_web
POST /api/apple-accounts/{id}/check/icloud_imap
```

仍可使用 `POST /api/apple-accounts/{id}/check` 一次检测全部已保存通道。账号启停接口为：

```http
POST /api/apple-accounts/{id}/status
Content-Type: application/json

{"enabled": false}
```

### 邮箱池

![邮箱池](./docs/screenshots/03-mailboxes.jpg)

### 域名邮箱

域名邮箱页面用于生成和登记地址、同步收件、获取验证码、查看完整邮件以及清理本地邮件。

### 创建隐私邮箱

![创建隐私邮箱](./docs/screenshots/04-tasks.jpg)

### 系统设置

![系统设置](./docs/screenshots/06-settings.jpg)

### 公共邮箱取码

![公共邮箱取码](./docs/screenshots/07-email-code.jpg)

`/email-code` 的邮件列表、邮件详情和验证码请求只查询当前输入邮箱。域名邮箱按邮件原始收件人识别，页面底部提供开源项目地址。

## 页面路由

| 路由 | 页面 |
| --- | --- |
| `/login` | 管理员登录 |
| `/` | 控制台 |
| `/apple-accounts` | Apple 账号与登录态 |
| `/mailboxes` | 邮箱池与邮件取码 |
| `/domain-mailboxes` | 域名邮箱与邮件取码 |
| `/tasks` | 隐私邮箱创建任务 |
| `/settings` | 系统设置、数据库维护与消息推送 |
| `/email-code` | 公共邮箱取码页面 |

## 项目结构

```text
├── main.go                 Go 服务入口
├── config.example.json     配置示例
├── internal/               后端业务、协议、SQLite 和 HTTP API
├── frontend/               Vue 3 前端
├── scripts/                开发、构建和前端同步脚本
├── docs/screenshots/       页面截图
└── bin/                    本地构建产物
```

## 更新代码后重新运行

如果只修改 Go 代码：

```bash
go run . -config config.json
```

如果修改了 `frontend/src`：

```bash
npm --prefix frontend run build
./scripts/sync-web.sh
go run . -config config.json
```
