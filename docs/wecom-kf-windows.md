# 微信客服（`wecom_kf`）Windows 服务器部署

> 对应规划 [§10 Windows 部署](./wecom-kf-codex-development-plan.md#10-windows-部署) 与 P1/P4。平台用法见 [wecom-kf.md](./wecom-kf.md)。
> 本文的脚本和步骤已做语法检查，**尚未在真实 Windows Server + 企业微信账号上验证**，首次部署请按文末清单逐项确认。

目标：一台 Windows 服务器上，一个 cc-connect 进程以固定的非管理员账户开机自启，Codex 只读回答，公网只暴露 HTTPS 客服回调。

```text
微信客服 ──HTTPS──> 反向代理 :443 (/wecom-kf/callback)
                        └──> 127.0.0.1:8081  cc-connect.exe（服务账户 ccservice，计划任务开机启动）
                                 └──> codex exec --sandbox read-only（同一账户，工作目录只读）
```

## 1. 固定版本并构建

在 fork 上记录所用提交（`git rev-parse HEAD`），升级前先在测试环境回归。构建（在 Linux/macOS 交叉编译或在 Windows 上直接构建均可，需要 Go 与 pnpm）：

```bash
cd web && pnpm install --frozen-lockfile && pnpm build && cd ..
GOOS=windows GOARCH=amd64 go build -o cc-connect.exe ./cmd/cc-connect
# 只要微信客服 + Codex 时可裁剪：make build PLATFORMS_INCLUDE=wecom_kf AGENTS=codex（Windows 目标需设置 GOOS/GOARCH）
```

建议目录：`D:\cc-connect\cc-connect.exe`、`D:\cc-connect\config.toml`、`D:\cc-connect\data`（`data_dir`）、`D:\cc-connect\logs`。

## 2. 服务账户

1. 新建本地普通用户（例如 `ccservice`），**不要**加入 Administrators。
2. 本地安全策略 `secpol.msc` → 本地策略 → 用户权限分配 →「作为批处理作业登录」加入该用户（计划任务在未登录时运行需要此权限）。
3. 用该账户登录一次（或 `runas /user:ccservice cmd`），以生成用户目录。

## 3. Codex CLI 与第三方中转

以下均在 **服务账户** 下完成，凭证和配置只放在该账户目录里，不要写进仓库或 cc-connect 配置的版本库：

1. 安装 Node.js 与 Codex CLI：`npm install -g @openai/codex`（记下 `codex.cmd` 所在目录，例如 `C:\Users\ccservice\AppData\Roaming\npm`，安装服务时用 `-ExtraPath` 传入；或在 cc-connect 项目中设置 `[projects.agent.options] cmd = '...\codex.cmd'`）。
2. 在 `C:\Users\ccservice\.codex\config.toml` 中保留现有中转站配置（`model_provider` / `base_url` / `env_key` 等），API Key 用该账户的用户环境变量或 `[projects.agent.options.env]` 提供。
3. 手动验证：`runas /user:ccservice "cmd /k codex exec --sandbox read-only \"总结 README\""`，确认能通过中转正常回答。

## 4. cc-connect 配置要点

参考 `config.example.toml` 中的 wecom_kf 段与 [wecom-kf.md](./wecom-kf.md#配置示例)：

- 顶层设置 `data_dir = 'D:\cc-connect\data'`（不设则在服务账户的 `%USERPROFILE%\.cc-connect`）。
- 每个客服账号一个 `[[projects]]`：`quiet = true`；`admin_from` 只填管理员的 `external_userid`（**不要**用 `"*"`）。
- Agent：`type = "codex"`，`backend = "exec"`，`mode = "suggest"`（只读沙箱、不弹审批）。
- 平台：`listen_addr = "127.0.0.1:8081"`（仅本机监听）。

启动时 cc-connect 会对微信客服项目做部署检查，以下情况会在日志中输出 `wecom_kf deployment check` 告警：Codex 使用可写模式（`auto-edit` / `full-auto` / `yolo`）、`backend = "app_server"`（会把工具审批提示发给客户）、Agent 不是 Codex、`admin_from = "*"`；`listen_addr` 不是回环地址时也会告警。正式上线前日志中应没有这些告警。

## 5. 安装为开机自启服务

以管理员身份打开 PowerShell：

```powershell
cd D:\src\cc-connect\deploy\windows
.\install-cc-connect-service.ps1 -ExePath D:\cc-connect\cc-connect.exe `
    -ConfigPath D:\cc-connect\config.toml `
    -ExtraPath 'C:\Users\ccservice\AppData\Roaming\npm','C:\Program Files\nodejs'
# 按提示输入服务账户（如 .\ccservice）及密码；密码只交给任务计划程序，不落盘
```

脚本会：

- 在配置目录生成 `cc-connect-service.ps1`：设置 `CC_LOG_FILE`（默认 `<配置目录>\logs\cc-connect.log`）、`CC_LOG_MAX_SIZE`（默认 10MB）、`CC_LOG_MAX_BACKUPS`（默认 10）以启用 cc-connect 自带日志轮转，运行 `cc-connect.exe -config <配置>`，非正常退出 10 秒后重启。
- 注册计划任务 `cc-connect`：**开机触发**、以服务账户「不管用户是否登录都运行」、普通权限、无运行时限、任务失败每分钟重试；并立即启动。
- 给服务账户授予日志目录的修改权限。

常用操作：`Get-ScheduledTask cc-connect`、`Stop-ScheduledTask cc-connect`、`Start-ScheduledTask cc-connect`；卸载 `.\install-cc-connect-service.ps1 -Uninstall`。可用 `-TaskName` 同机安装多套，`-LogFile` / `-LogMaxSizeMB` / `-LogMaxBackups` 调整日志。

> 与 `cc-connect daemon install` 的区别：后者在用户**登录时**以交互方式启动，适合个人电脑；服务器无人登录时请用本脚本，二者不要同时安装。

## 6. 文件权限（只读的系统级保证）

规划要求不能只靠提示词保证“只回答”。除 Codex `--sandbox read-only` 外，用 NTFS 权限让服务账户对项目资料只有读取权限；cc-connect 只需要写客户附件目录 `.cc-connect`：

```powershell
$u = 'ccservice'; $w = 'D:\AIProjects\project1'
icacls $w /grant "${u}:(OI)(CI)RX"
New-Item -ItemType Directory -Force "$w\.cc-connect" | Out-Null
icacls "$w\.cc-connect" /grant "${u}:(OI)(CI)M"
icacls D:\cc-connect\data /grant "${u}:(OI)(CI)M"
# config.toml 含企业 Secret：仅管理员与服务账户可读
icacls D:\cc-connect\config.toml /inheritance:r /grant:r "Administrators:F" "SYSTEM:F" "${u}:R"
```

确认服务账户不在对项目目录有写权限的组（如对该盘有修改权限的 `Users`）中，必要时去掉继承的写权限。

## 7. HTTPS 反向代理与防火墙

只把客服回调路径转发到本机监听，其它路径一律拒绝。Caddy 示例（自动申请证书）：

```caddyfile
kf.example.com {
    handle /wecom-kf/callback {
        reverse_proxy 127.0.0.1:8081
    }
    respond 404
}
```

nginx 示例：

```nginx
server {
    listen 443 ssl;
    server_name kf.example.com;
    # ssl_certificate / ssl_certificate_key ...
    location = /wecom-kf/callback { proxy_pass http://127.0.0.1:8081; }
    location / { return 404; }
}
```

Windows 防火墙只放行 443（以及证书签发需要的 80），不要放行 8081、cc-connect 管理 / Web UI 端口或 Codex 相关端口。服务器出口 IP 加入企业微信应用的「企业可信 IP」。

## 8. 备份

定期备份 `config.toml`（含密钥，注意加密存放）与 `data_dir`（会话、`wecom_kf\` 下的拉取游标与去重记录）。示例（每日计划任务）：

```powershell
robocopy D:\cc-connect\data E:\backup\cc-connect\data /MIR /R:1 /W:1
Copy-Item D:\cc-connect\config.toml E:\backup\cc-connect\ -Force
```

恢复时先停任务、还原文件再启动；游标还原后不会重复回复已处理消息。

## 9. 上线检查清单

- [ ] 重启服务器且无人登录，`Get-ScheduledTask cc-connect` 为 Running，日志有平台启动信息。
- [ ] 日志中没有 `wecom_kf deployment check` 告警。
- [ ] 在企业微信后台保存回调 URL 时 URL 校验通过。
- [ ] 用两个微信号同时向同一客服提问，回答正确且上下文互不串。
- [ ] 让客服“在项目里新建/修改/删除一个文件”“执行某个命令”：被拒绝，且项目目录无变化（验证 Windows 下只读沙箱和 NTFS 权限真实生效）。
- [ ] 人工接管后 AI 不再回复；结束后客户再次发消息 AI 恢复。
- [ ] 手动结束 `cc-connect.exe` 进程，10 秒内自动重启；重启后不会重复回复旧消息。
- [ ] 日志超过上限后出现轮转文件。
