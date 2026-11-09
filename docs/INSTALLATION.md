# 安装、升级与回滚

需要兼容的 CPA 8.0.35 / CPAMC 1.26.5、Linux amd64/glibc、systemd、Python 3 与 PyYAML。默认服务和用户为 `cliproxyapi`。先从 Releases 下载 0.3.0 安装包及外部 `SHA256SUMS`：

```bash
sha256sum -c SHA256SUMS
tar -xzf cpa-quota-guard_0.3.0_linux_amd64.tar.gz
cd cpa-quota-guard-0.3.0
sha256sum -c dist/SHA256SUMS
sudo apt-get install python3-yaml
python3 install.py --doctor
python3 install.py  # 只读计划
```

`--doctor` 独立运行，无需存在构建产物或管理密钥；显示宿主版本、平台、面板原始/安装指纹、桥接支持、其他插件能力、候选构建及可查询的运行构建。管理密钥不可用时相关项显示未查询。它不刷新额度、不兑换、不重启。

安装器在修改前校验 ELF64/amd64、`dist/BUILD.json` 的版本、ABI/schema、构建提交/时间与共享库 SHA256；Release 内的 `MANIFEST.json` 还校验逐文件完整性。不要为通过预检修改校验值。未知 CPAMC 构建始终拒绝；兼容性见 [COMPATIBILITY.md](COMPATIBILITY.md)。

## 两个独立开关与新账号

**首次安装默认：额度保护仅观察；自动使用重置卡关闭；新增账号不自动加入兑换。** `policy.safe-start.example.json` 与新的默认策略一致。安装计划和成功结果分别列出 `quota_mode`、`auto_reset_enabled`、`include_new_accounts`，观察模式本身不禁止已经启用的后台兑换。

通常先安全安装，再在 CPAMC 逐账号选择并启用。若要在第一次启动时明确启用指定账号：

```bash
sudo python3 install.py --apply --management-key-file /root/cpa-management.key \
  --auto-reset on --reset-account example-account.json
```

`--auto-reset on` 必须同时列出至少一个 `--reset-account`，可以重复该参数。安装器始终让 `include_new_accounts=false`。`--auto-reset off` 可显式选择默认关闭。它们只适用于没有 `policy.json` 的首次安装。

**升级保留现有策略和执行记录，不用新默认覆盖老设置。** 旧文件若没有 `include_new_accounts`，后端将其视为 `true`，维持原先缺省账号参与兑换的行为；没有 `auto_reset` 的更老文件也沿用旧版开启行为。升级后请在面板检查，并按需要关闭“自动加入新增账号”。观察/执行模式及既有账号规则保留。

## 密钥和正式安装

将现有 CPA 实际管理密钥保存在仅 root 可读的文件中；不要使用配置中的 bcrypt 哈希，不要把密钥写入 shell 历史：

```bash
sudo test -e /root/cpa-management.key || sudo install -m 600 /dev/null /root/cpa-management.key
sudoedit /root/cpa-management.key
sudo python3 install.py --apply --management-key-file /root/cpa-management.key
```

只有密钥文件不存在时才执行创建命令，避免覆盖已有内容。安装器不更换或打印密钥；它把现有密钥复制到状态目录 `cpa-management.key`，属主为服务用户、权限 0600。CPA 管理密钥轮换后同步该文件或重跑安装器。后台重置只通过 CPA 回环管理接口，忽略环境代理、不跟随重定向；不能只替换 `.so` 跳过配置。

安装器读取原服务/API 基线，创建并立即输出备份路径，再写入共享库、会话桥接及配置，重启 CPA，验证 active、插件版本及构建身份。它不调用 refresh 或 consume 进行验收，已有后台策略仍按原计划运行。安装失败时恢复旧文件，重新验证服务 active 和原版本 API；恢复失败会明确报告备份路径和未验证状态。

## 其他插件与面板扩展

纯管理界面或 SSO 插件可以共存；其他调度插件必须先解决调度冲突。安装器优先使用宿主明确返回的 `supports_scheduler` / `capabilities.scheduler`。CPA 8.0.35 的插件列表没有该字段，因此不能仅凭插件名称或菜单判断安全。

旧宿主可提供经维护者核实的能力声明，绑定当前已安装二进制 SHA256：

```json
{"schema_version":1,"plugins":{"example-menu":{"sha256":"填写已审阅共享库的64位SHA256","scheduler":false}}}
```

```bash
sudo python3 install.py --apply --management-key-file /root/cpa-management.key \
  --plugin-capabilities /root/plugin-capabilities.json
```

未知能力会在计划/doctor 中标为 unknown，`--apply` 在变更前阻止。二进制更新后原声明自动失效，需重新核实。声明只用于兼容性预检，不加载其他插件、不执行其注册逻辑。仓库测试使用合成的管理插件验证组合安装与回滚，不假定某个公开插件名永远不参与调度。

面板若还有已审核扩展，可用 `--panel-extensions` 保留其标记区块：

```json
{"extensions":[{"marker":"example-sso","sha256":"填写完整标记区块的64位SHA256"}]}
```

SHA256 覆盖完整 `\n/* example-sso:start */\n...\n/* example-sso:end */\n` 字节。仅当所有声明区块精确匹配、移除这些区块及本插件旧桥接后仍是已验证的原始 HTML，才允许安装。缺失/重复标记、修改过的区块或区块以外的更改均拒绝。不公开上传实际管理面板或私有声明；示例不包含生产数据。

## 自定义布局

```bash
sudo python3 install.py --apply \
  --config /etc/cliproxyapi/config.yaml \
  --panel /opt/cliproxyapi/static/management.html \
  --state-dir /var/lib/cliproxyapi/quota-guard \
  --service cliproxyapi --service-user cliproxyapi \
  --management-key-file /root/cpa-management.key
```

其他参数见 `python3 install.py --help`。插件目录与状态目录必须为绝对路径。`--proxy-url direct` 可禁用查询代理；默认继承 CPA `requests.proxy-url`，账号代理优先。安装器只使用回环 HTTP 管理端口，CPA 仅监听 HTTPS 的布局需手工部署和验证。配置合并会保留值，但 YAML 注释/排版会重新生成。

## 回滚

使用安装前已输出的备份路径，并提供当前实际管理密钥用于恢复后的只读 API 验证：

```bash
sudo python3 install.py --rollback /root/cliproxyapi-plugin-backups/quota-guard-实际目录 \
  --management-key-file /root/cpa-management.key
```

回滚恢复配置、共享库、面板及升级前插件密钥，保留 `policy.json` / `reset-journal.json`。原服务和 API 验证通过后才报告成功。旧版安装器创建的备份没有 API 基线，恢复后会明确要求人工验证，不虚报成功。回滚不是状态迁移工具；回滚旧代码前确认状态格式兼容。CPA 重启期间请求可能短暂不可用，关闭开关也不能撤回已经发出的兑换请求。
