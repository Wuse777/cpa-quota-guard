# 测试与发布验证

## 日常检查

Linux、Go、GCC、Node.js、Python 3；Python 依赖见 `requirements.txt`。推荐在虚拟环境安装 Python 依赖：

```bash
python3 -m venv .venv
source .venv/bin/activate
python3 -m pip install -r requirements.txt
bash scripts/check.sh
```

`scripts/check.sh` 执行 Go race / vet、11 项 Node 会话测试和 Python 安装、桥接及打包单元测试。它不使用真实账号；`TestLiveQuotaCompatibility` 未设置环境变量时跳过。

GitHub CI 在 Linux 使用 Go 1.23.x 与当前已验证的 1.27.3 执行这些检查及共享库构建。CI 不包含真实账号验收，也不自动在外部服务器安装插件。

## 真实 CPA 隔离集成

从上游取得 CPA 8.0.35 二进制后，传入其绝对路径：

```bash
python3 tests/integration.py \
  --plugin dist/quota-guard.so \
  --cpa /absolute/path/to/cli-proxy-api
```

需要 Linux、PyYAML、OpenSSL 及空闲回环端口 18317–18319。测试使用虚构 OAuth 账号、本地额度 HTTP 服务和本地 TLS CONNECT 模型代理，不向真实上游发送模型请求。覆盖注册、认证、调度、窗口阈值、恢复、冲突、重启、临期自动兑换及持久化去重，共 26 项检查（包含真实 CPA 转发、嵌套错误持久化、成功列表查询及重启后失败保留）。

结果默认写入被 Git 忽略的 `test-results/integration-report.json`；可用 `--report` 指定路径。测试目录保留在系统临时目录，便于本机诊断，不要将运行结果直接加入仓库。

## CPAMC 浏览器验收

提供 [已验证构建](COMPATIBILITY.md) 的原始面板 HTML：

```bash
python3 tests/integration.py \
  --plugin dist/quota-guard.so \
  --cpa /absolute/path/to/cli-proxy-api \
  --panel /absolute/path/to/management.html \
  --serve
```

打开 `http://127.0.0.1:18317/management.html`，测试专用密钥为 `quota-guard-integration-only`。它是公开的本地夹具凭据，不能用于生产。未提供有效面板时 `--serve` 会报错。

核对：面板登录直接进入插件、全局策略保存、账号与独立窗口阈值、冲突提示、三种主题、标签键盘导航、窄屏溢出和 `prefers-reduced-motion`。截图只使用 `a.json`、`b.json`、`g.json` 等虚构账号。仓库图片来自这一隔离夹具。

## 真实 systemd 安装与回滚

该检查需要 root，启动独立临时 unit，占用回环端口 18417。不会重启正式 CPA unit：

```bash
sudo .venv/bin/python tests/install_integration.py \
  --plugin dist/quota-guard.so \
  --cpa /absolute/path/to/cli-proxy-api \
  --panel /absolute/path/to/management.html
```

覆盖正常基线、真实共享库安装与认证注册、回滚后配置逐字节恢复及策略保留，共 3 项检查。默认报告为 `test-results/install-integration-report.json`。

## 可选真实额度只读检查

仅由维护者显式选择：

```bash
QUOTA_GUARD_LIVE_AUTH_DIR=/private/path/to/auths \
QUOTA_GUARD_LIVE_PROXY=http://127.0.0.1:7890 \
go test -run TestLiveQuotaCompatibility -v -count=1
```

读取已有授权并查询服务商额度，不刷新 token、不发送模型生成请求、不兑换卡片。不要把真实授权或未经脱敏的日志放入仓库、CI、Issue 或公共附件。

## 发布

```bash
bash scripts/check.sh
python3 scripts/package_release.py
(cd release && sha256sum -c SHA256SUMS)
```

生成安装归档、外部 SHA256SUMS 和包内逐文件清单。`dist/` 与 `release/` 不提交到 Git。手工发布前核对安装、回滚、兼容性和文档；GitHub CI 只生成 Actions artifact，不自动公开 Release 或更改服务器。

0.2.2 本轮本地 Linux 构建使用 Go 1.23.12，执行 Go race / vet、15 项 Python 单元测试、11 项 Node 会话测试、26 项真实 CPA 隔离检查及 3 项 systemd 安装回滚检查。真实卡片未用于提前兑换测试。
