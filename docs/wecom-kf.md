# 微信客服（WeChat Customer Service）接入 / `wecom_kf`

> 状态：P2 单客服 + P3 多客服 + P4 人工接管抑制 + 入站图片/语音/视频/文件 已实现代码与自动化测试，**尚待真实企业微信账号联调**。
> 总体规划见 [wecom-kf-codex-development-plan.md](./wecom-kf-codex-development-plan.md)。

`wecom_kf` 平台让个人微信用户通过企业微信「微信客服」向 cc-connect 项目（例如 Codex）提问，并把回答发回微信。
它与原有 `wecom`（企业微信自建应用 / 智能机器人）平台完全独立，不改变其行为。

## 工作方式

```text
微信客户 → 微信客服 → HTTPS 回调 (kf_msg_or_event)
        → 验签 / 解密 → 立即返回 "success"
        → 异步 kf/sync_msg 拉取（游标持久化、msgid 去重）
        → 按 open_kfid 找到项目，按 external_userid 生成会话
        → service_state 检查（人工接管时不分发）
        → cc-connect Core → Codex（只读）
        → 回复前再次检查 service_state → kf/send_msg（纯文本、按 2048 字节拆分）
```

- **会话键**：`wecom_kf:{corp_id}:{open_kfid}:{external_userid}`。同一客服下的客户共享项目目录但不共享对话；同一客户在不同客服下是不同会话。
- **多客服**：每个客服账号是一个 `[[projects]]`，各自配置 `open_kfid`。所有项目使用相同的 `listen_addr` / `callback_path` / `corp_id` / `callback_token` / `callback_aes_key` 时，只会开一个监听端口，回调按 `open_kfid` 分发。同一个 `open_kfid` 被映射到两个项目、或同一回调地址上的凭证不一致时，启动失败，绝不随机选项目。
- **去重与重启恢复**：拉取游标和最近处理过的 msgid 保存在 `<data_dir>/wecom_kf/<corp_id>_<open_kfid>.json`。重复回调、重复拉取不会重复触发 Codex；重启后从游标继续。首次启动（无游标）时，`sync_msg` 返回的 3 天历史中早于启动时间的消息会被忽略。
- **人工接管**：会话状态为「待接入池排队中(2)」「由人工接待(3)」或「已结束(4)」时，新问题不交给 AI；AI 已在生成时，回复前再次检查，若已转人工则丢弃迟到回答。人工结束后的恢复遵循微信客服平台自身的状态流转（客户再次发消息后状态变为「未处理」），本平台不强制改变状态。`service_state/get` 调用失败时会记录告警并继续（fail-open），可设 `takeover_check = false` 关闭检查。
- **发送限制**：微信客服规定每条客户消息后最多可回复 5 条（48 小时内）。平台为每个客户维护发送额度（`max_replies_per_message`，默认 5），超出时截断并标注 `…`。建议项目设置 `quiet = true`，避免思考/工具进度消息消耗额度。
- **格式**：微信只显示纯文本，回复前会去除 Markdown，并通过 `FormattingInstructions` 提示 Agent 使用纯文本、只读问答。
- **消息类型**：处理客户发送的文本、图片、语音、视频、文件。媒体通过 `media/get` 下载（单个上限 25 MB；微信客服本身限制图片/语音 2 MB、视频 10 MB、文件 20 MB），图片作为图片附件、语音作为 AMR 音频（配置了 `[speech]` 时由引擎转文字）、视频和文件作为文件附件交给 Agent，文件名取自下载响应的 `Content-Disposition`。附件由 cc-connect 按现有机制保存到项目工作目录下的 `.cc-connect/attachments/`（Codex 本身仍是只读）。位置、链接、小程序等其他类型以及发送失败的下载会记录日志后忽略。客服人员回复（origin=5）与系统事件（origin=4）不会再次触发机器人。可设 `inbound_media = false` 只接收文本。
- **消息合并**：与企业微信 WebSocket 私聊的聚合方式一致，同一客户在 `merge_window_ms`（默认 2000 ms）内连续发送的消息会合并成一次提问（例如先发文件再问“这个文件讲了什么”），媒体下载未完成时会等待下载结束再发出；设为 0 关闭合并。回复只发送文本，不发送媒体。

## 企业微信后台配置

1. 「微信客服」中创建客服账号，并在「通过 API 管理微信客服账号」里把这些账号交给一个自建应用管理；在「可调用接口的应用」中加入该应用。
2. 记录企业 `corp_id`、该应用的 `Secret`（填 `corp_secret`）、各客服账号的 `open_kfid`（`wk` 开头）。
3. 在微信客服 API 的回调配置中填写：URL `https://<你的域名>/wecom-kf/callback`、Token、EncodingAESKey（43 位）。**先启动 cc-connect 再保存**，以通过 URL 校验。
4. 服务器出口 IP 加入应用的企业可信 IP。
5. 公网 HTTPS 反向代理只把 `/wecom-kf/callback` 转发到 `127.0.0.1:8081`，不要直接暴露 cc-connect 或 Codex 的其它端口。

## 配置示例

```toml
[[projects]]
name = "客服项目1"
quiet = true

[projects.agent]
type = "codex"

[projects.agent.options]
work_dir = 'D:\AIProjects\project1'
backend = "exec"
mode = "suggest"          # 只读沙箱

[[projects.platforms]]
type = "wecom_kf"

[projects.platforms.options]
corp_id = "YOUR_CORP_ID"
corp_secret = "YOUR_WECHAT_KF_SECRET"
open_kfid = "YOUR_OPEN_KFID_1"
callback_token = "YOUR_CALLBACK_TOKEN"
callback_aes_key = "YOUR_ENCODING_AES_KEY"
listen_addr = "127.0.0.1:8081"
callback_path = "/wecom-kf/callback"

# 第二个客服：复制整段项目配置，只改 name / work_dir / open_kfid
[[projects]]
name = "客服项目2"
quiet = true

[projects.agent]
type = "codex"

[projects.agent.options]
work_dir = 'D:\AIProjects\project2'
backend = "exec"
mode = "suggest"

[[projects.platforms]]
type = "wecom_kf"

[projects.platforms.options]
corp_id = "YOUR_CORP_ID"
corp_secret = "YOUR_WECHAT_KF_SECRET"
open_kfid = "YOUR_OPEN_KFID_2"
callback_token = "YOUR_CALLBACK_TOKEN"
callback_aes_key = "YOUR_ENCODING_AES_KEY"
listen_addr = "127.0.0.1:8081"
callback_path = "/wecom-kf/callback"
```

| 选项 | 必填 | 默认 | 说明 |
| --- | --- | --- | --- |
| `corp_id` | 是 | | 企业 ID，也用于校验回调密文的 receive ID |
| `corp_secret` | 是 | | 可调用微信客服接口的自建应用 Secret（不要与智能机器人凭证混用） |
| `open_kfid` | 是 | | 本项目对应的客服账号 ID |
| `callback_token` / `callback_aes_key` | 是 | | 回调 Token 与 43 位 EncodingAESKey |
| `listen_addr` | 否 | `127.0.0.1:8081` | 回调监听地址；多个项目相同即共享 |
| `callback_path` | 否 | `/wecom-kf/callback` | 回调路径 |
| `takeover_check` | 否 | `true` | 分发和回复前查询会话状态 |
| `max_replies_per_message` | 否 | `5` | 每条客户消息后最多发送条数 |
| `inbound_media` | 否 | `true` | 接收客户图片 / 语音 / 视频 / 文件 |
| `merge_window_ms` | 否 | `2000` | 合并同一客户连续消息的时间窗（毫秒），0 关闭 |
| `allow_from` | 否 | 全部 | 允许的客户 `external_userid`，逗号分隔 |
| `api_base_url` | 否 | `https://qyapi.weixin.qq.com` | API 地址覆盖 |
| `proxy` | 否 | | API 出站正向代理 |

权限：普通客户只能操作自己的会话；查看/切换他人会话、修改模式或工作目录等特权命令请通过项目的 `admin_from`（填 external_userid）限制。

## 尚待联调确认

- 回调验签/解密、`sync_msg` token 与频率、`send_msg` 的 48 小时窗口和 5 条限制在真实账号上的表现。
- 人工结束会话后 AI 恢复接待的实际状态流转。
- 是否需要在 AI 接待时主动把会话转为「由智能助手接待(1)」。
- 真实账号下 `media/get` 对微信客服媒体的返回头（文件名、类型）与大小限制；欢迎语（`enter_session` + `welcome_code`）。
- Windows 服务化部署与只读沙箱在 Windows 上的实际效果（规划 P1/P4）。

---

## English summary

`wecom_kf` connects WeCom **WeChat Customer Service** to cc-connect. Each customer-service account (`open_kfid`)
maps to one `[[projects]]` entry; all accounts share one callback listener (same `listen_addr` + `callback_path`)
and notifications are routed by `open_kfid`. Messages are pulled with `kf/sync_msg` (cursor + message IDs persisted
under `<data_dir>/wecom_kf/`), dispatched with session key `wecom_kf:{corp_id}:{open_kfid}:{external_userid}`, and
answered as plain text via `kf/send_msg` (split at 2048 bytes, at most `max_replies_per_message` sends per customer
message). When a session is queued for or handled by a human servicer, the AI neither receives the question nor sends
late answers. Use the Codex agent with `mode = "suggest"` for read-only Q&A. Customer text, images, voice (AMR),
video and files are passed to the agent (media via `media/get`, `inbound_media = false` for text only); messages a
customer sends within `merge_window_ms` (default 2 s) are merged into one turn. Replies are text only. Real-account
integration testing is still pending.
