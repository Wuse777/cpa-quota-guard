# 测试与发布验证

测试使用虚构账号和本地服务，不使用生产 OAuth、管理密钥、真实卡片或模型生成。Linux 工具：Go、GCC/cc、OpenSSL、Python 3/PyYAML、Node.js 22 与 Chromium。

## 日常检查

```bash
python3 -m venv .venv
source .venv/bin/activate
python3 -m pip install -r requirements.txt
bash scripts/check.sh
```

执行 Go race/vet、Node 会话与界面模型测试、Python 安装/桥接/打包单测。`TestLiveQuotaCompatibility` 未显式设置环境变量时跳过。POSIX 安装事务测试在 Linux 实际执行；Windows 只运行可移植预检，缺少符号链接权限会明确跳过相应测试，不能当成完整 Linux 验证。

```bash
bash build.sh
```

构建运行 Go race/vet，生成 `.so`、C 头文件、`BUILD.json` 和摘要。默认记录 Git HEAD 与 UTC 构建时间；受控发布可通过 `QUOTA_GUARD_BUILD_COMMIT` 和 `QUOTA_GUARD_BUILD_TIME` 指定。构建身份同时写入 Go 状态与 manifest，安装后必须一致。源码导出无 Git 时 commit 为 `source-export`，正式发布应提供真实提交。

## 固定上游夹具

```bash
python3 scripts/fetch_fixtures.py
```

下载官方 CPA **8.0.35** Linux amd64 与 CPAMC **1.26.5**，使用固定版本 URL，校验归档、二进制及面板 SHA256。源地址/摘要在脚本中，夹具只写入 Git 忽略的 `test-results/fixtures/`。不使用 latest；校验失败立即停止，不自动信任新摘要。也可使用自己已验证的绝对路径传入后续测试。

## 真实 CPA 隔离集成

```bash
python3 tests/integration.py --plugin dist/quota-guard.so \
  --cpa "$PWD/test-results/fixtures/cli-proxy-api" \
  --panel "$PWD/test-results/fixtures/management.html"
```

使用独立临时配置、虚构 OAuth、本地额度 HTTP 与 TLS CONNECT 模型代理。覆盖注册/认证/调度、窗口保护与恢复、冲突和重启、自动兑换去重/异常、旧策略兼容、新账号选择和后台健康。用例数由报告动态计算，新增用例后不依赖文档手工计数。默认报告为 `test-results/integration-report.json`，回环端口为 18317–18319。测试失败即阻止发布。

## 安装、组合与回滚

无 systemd PID 1 的环境也必须运行真实 CPA 进程验收：

```bash
python3 tests/install_integration.py --runtime process --plugin dist/quota-guard.so \
  --cpa "$PWD/test-results/fixtures/cli-proxy-api" \
  --panel "$PWD/test-results/fixtures/management.html"
```

process 控制器仅接受随机的测试服务名称，操作 PID 前验证 CPA 路径和临时配置，不调用正式 systemd。安装器实际复制共享库、重启 CPA、验证构建身份，与合成管理插件组合运行，然后回滚并验证原 API、逐字节配置、策略和 journal。它不是将安装结果全部 mock 的单测。

还必须运行真实 systemd 临时 unit 验收：

```bash
sudo .venv/bin/python tests/install_integration.py --runtime systemd \
  --report test-results/install-systemd-report.json --plugin dist/quota-guard.so \
  --cpa "$PWD/test-results/fixtures/cli-proxy-api" \
  --panel "$PWD/test-results/fixtures/management.html"
```

需要 root、空闲回环端口 18417 与工作的 systemd。独占创建随机 unit，结束后停止并移除，仅涉及独立临时服务。CI 同时执行 process 和 systemd 两种模式；任意失败都不跳过。正式服务不参与测试。

## Chromium 界面与真实会话桥接

```bash
npm ci
npx playwright install --with-deps chromium
node tests/browser-regression.mjs
```

合成父页面使用真实插件 HTML/CSS/JS，检查草稿与实时状态、保存冻结、冲突保留、搜索/筛选、聚合异常、明暗主题和手机布局。使用锁定版本的 Playwright，截图只含虚构账号。

另外启动真实隔离 CPA/CPAMC：

```bash
python3 tests/integration.py --plugin dist/quota-guard.so \
  --cpa "$PWD/test-results/fixtures/cli-proxy-api" \
  --panel "$PWD/test-results/fixtures/management.html" --serve
```

等待 `BROWSER_FIXTURE_READY`，在另一终端运行：

```bash
node tests/browser-regression.mjs --url http://127.0.0.1:18317/management.html \
  --key quota-guard-integration-only
```

该公开测试密钥只属于临时夹具。真实浏览器完成 CPAMC 登录、插件导航、iframe 桥接读/保存/恢复/刷新、主题同步和窄屏检查。`--serve` 必须在隔离测试环境使用，结束后 Ctrl+C 清理。仓库 `docs/images/` 来自虚构夹具，不上传生产截图。

## CI 与发布门禁

`.github/workflows/ci.yml` 对普通 PR、main、`v*` 标签及手工触发执行。Go 1.23.x/1.27.3 矩阵均运行单测和构建；1.27.3 额外执行固定上游夹具下载、真实 CPA 集成、process + systemd 组合安装回滚、合成 Chromium 回归和真实 CPAMC 桥接。没有秘密变量或真实账号依赖。标签版本必须与 `BUILD.json` 一致。

只有验证通过才生成可交付归档；CI 上传安装包及无真实账号的验收报告，不自动公开 GitHub Release 或修改外部服务器。发布者仍需确认该提交的所有门禁通过再上传资产。包内逐文件清单排除私有状态、授权、日志和测试报告：

```bash
python3 scripts/package_release.py
(cd release && sha256sum -c SHA256SUMS)
```

## 可选真实只读兼容性检查

维护者单独显式设置 `QUOTA_GUARD_LIVE_AUTH_DIR` / `QUOTA_GUARD_LIVE_PROXY` 后可运行 `go test -run TestLiveQuotaCompatibility -v -count=1`。该检查读取现有授权并查询额度，不刷新 token、不生成模型响应、不兑换卡片；不属于 PR/发布门禁，也不应把真实状态附到 Issue 或公开报告。
