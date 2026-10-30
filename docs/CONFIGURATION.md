# 配置与行为

优先在 CPAMC 页面修改，保存时会携带最新 `revision` 并执行校验。`policy.example.json` 仅演示字段；`policy.safe-start.example.json` 是首次启动前关闭自动兑换的示例。它们不包含真实账号。

## 额度保护

| 字段 | 默认 | 含义 |
| --- | --- | --- |
| `mode` | `observe` | `observe` 只观察；`enforce` 执行避让 |
| `poll_seconds` | 60 | 查询间隔，10–3600 秒 |
| `max_age_seconds` | 180 | 成功额度快照有效期，10–86400 秒，不能短于查询间隔 |
| `timeout_seconds` | 15 | 单账号查询超时，1–60 秒 |
| `safety_percent` | 3 | 提前停止余量，0–100 个百分点 |
| `resume_gap_percent` | 5 | 恢复间隔，0–100 个百分点 |
| `unknown_action` | `block` | 未知、过期或缺失窗口时 `block` / `allow` |
| `strategy` | `round-robin` | `round-robin` 轮询或 `fill-first` 依次使用 |
| `rules` | `[]` | 每账号开关、通用阈值及可选窗口阈值 |

规则的 `account` 可匹配 CPA 账号名、ID 或认证索引。`min_remaining_percent` 为最低剩余比例；可选 `windows` 使用查询到的窗口 ID 映射独立阈值。未自定义窗口时保护普通窗口；附加额度按需要手动选择。不要从界面显示名称猜测窗口 ID。

```text
停止线 = min(100, 最低保留比例 + 提前停止余量)
恢复线 = min(100, 停止线 + 恢复间隔)
```

任一受保护窗口剩余 ≤停止线后暂不分配新请求；所有受保护窗口成功查询到 ≥恢复线后才恢复。重置时间用于显示，不代表额度已经恢复。查询失败保留最近成功结果至有效期结束；未知数据按策略处理。

启用规则命中的请求优先选择最高优先级的可用账号，再轮询或依次选择；全部受保护时返回 `quota_guard_protected`。未命中启用规则或处于观察模式时交给 CPA 原生调度。匹配规则时原生加权及亲和性不会与插件选择叠加。

## 自动使用重置卡

| 字段 | 默认 | 含义 |
| --- | --- | --- |
| `auto_reset.enabled` | `true` | 服务器后台自动兑换总开关 |
| `auto_reset.before_hours` | 6 | 到期前多少小时进入窗口，0.1–168 |
| `auto_reset.accounts` | `{}` | 账号独立开关；缺省开启 |

自动兑换独立于 `mode`；即使只观察额度也会运行。启用时轮询间隔不超过 60 秒，实际检查受网络耗时影响。升级旧策略时若没有 `auto_reset` 字段，会补默认开启和提前 6 小时。

仅处理可用、未过期的 `codex_rate_limits` 卡片，优先考虑最早到期者。请求编号先持久化再提交；超时或不明确响应先核实并沿用原编号重试。明确未执行的拒绝延后约 5 分钟。上游确认后只核实卡片同步，不持续重新提交。

上游不能指定消耗的卡片。非目标卡变为不可用时，账号兑换暂停至目标卡到期。执行记录最近 200 条，保存在 `reset-journal.json`；写入失败或文件损坏时停止自动兑换，额度保护继续运行。关闭开关不会撤回已经发出的请求。

自 0.2.2 起，查询与兑换均通过本机 CPA `/v8/management/requests/api-call`，复用 CPAMC 手动重置的转发接口、账号索引和 `$TOKEN$` 解析；插件不再自行向上游发送兑换 HTTP 请求。`host.routing.reset_cooldown` 仍仅用于成功后的 CPA 调度冷却清理。

每次兑换返回的 CPA HTTP 状态、上游 HTTP 状态、结构化错误码和脱敏摘要分别保存在 operation 的 `last_result` 和事件的 `result` 中。CPA 自身失败时，上游状态为 0/缺省，避免将转发失败误报为上游拒绝。无效 JSON、未知响应码和嵌套错误均显示明确失败原因；原始响应和请求头不写入日志。

成功查询列表不会清除最近兑换错误；退避、到期、重启后仍显示错误，直到后续兑换被确认成功或完成卡片核实。旧 journal 保持版本 1 可读，旧失败没有 HTTP 详情时会显示“旧版本未保存”提示，不虚构历史原因。事件页显示具体错误，顶部显示“兑换异常”。

后台调用需要 `management_url`（默认 `http://127.0.0.1:8317`）和 `management_key_file`（默认状态目录下的 `cpa-management.key`）。升级时重新运行安装器：它按 CPA 端口配置回环地址，将实际管理密钥写入服务用户持有的 0600 文件，不把密钥写进 YAML、策略、状态接口或执行记录。仅允许无路径的回环 HTTP 地址，忽略环境代理且不跟随重定向。管理密钥轮换后，重新运行安装器或更新该私有文件；缺失或无效时停止兑换并显示错误，不回退直连上游。

## 数据与接口

| 方法 | 路径 | 作用 |
| --- | --- | --- |
| GET | `/v0/management/plugins/quota-guard/status` | 当前策略、账号、额度、卡片和执行记录 |
| PUT | `/v0/management/plugins/quota-guard/policy` | 保存完整策略，携带最新修订号 |
| POST | `/v0/management/plugins/quota-guard/refresh` | 排队刷新，返回 202 |
| GET | `/v0/resource/plugins/quota-guard/index` | 不含账号及密钥的静态页面 |

管理接口沿用 CPA 管理认证；策略格式错误返回 400，编辑冲突返回 409。状态中包含账号名称等管理信息，不返回 OAuth token。静态资源公开访问不等于匿名访问管理数据。

状态目录为 0700，策略与执行记录为 0600；额度快照保存在内存，重启后重新查询。插件读取 CPA 管理的当前 OAuth 授权及代理，不修改账号 disabled 标志或 OAuth 文件。

运行配置 `test_probe_base` 仅允许本地回环 HTTP，专用于隔离测试，生产不要配置。运行配置样例见根目录 `config.example.yaml`。
