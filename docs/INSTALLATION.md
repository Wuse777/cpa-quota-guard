# 安装、升级与回滚

## 前提

- 已运行兼容的 CPA / CPAMC，详见 [COMPATIBILITY.md](COMPATIBILITY.md)。
- Linux 主机、systemd、Python 3 与 PyYAML，拥有 root 权限。
- 默认目录为 `/opt/cliproxyapi`，服务及用户为 `cliproxyapi`。
- 预编译安装包从本仓库 Releases 获取；源码构建使用 `bash build.sh`。

Debian / Ubuntu 的 Python 依赖可使用 `sudo apt-get install python3-yaml`。开发虚拟环境也可用 `python3 -m pip install -r requirements.txt`；用 sudo 运行安装器时，需确保所选 Python 能导入 `yaml`。

## 首次自动兑换设置

默认策略的额度保护为观察模式，但**自动使用重置卡默认开启、提前 6 小时**，保存策略或关闭浏览器不会改变后台独立运行的性质。

若希望在第一次启动前关闭自动兑换，仅在新的策略目录中预置安全启动策略：

```bash
# 下列路径应与 --state-dir 保持一致；升级时不要覆盖原策略。
sudo install -d -o cliproxyapi -g cliproxyapi -m 700 /var/lib/cliproxyapi/quota-guard
if sudo test -e /var/lib/cliproxyapi/quota-guard/policy.json; then
  echo '已有策略，保留原文件；请在 CPAMC 中检查自动使用开关。'
else
  sudo install -o cliproxyapi -g cliproxyapi -m 600 policy.safe-start.example.json \
    /var/lib/cliproxyapi/quota-guard/policy.json
fi
```

该示例关闭自动兑换、保持观察模式且没有账号规则。安装后可在面板中再启用。正常运行和升级时不要手动替换策略或删除执行记录。

## 校验及安装

在解压前校验外部 `SHA256SUMS`，进入安装目录后执行 `sha256sum -c dist/SHA256SUMS`。先查看计划：

```bash
python3 install.py
```

将现有 CPA 管理密钥保存在仅 root 可读的文件中，例如通过编辑器填写，而不是写入 shell 历史：

```bash
sudo test -e /root/cpa-management.key || sudo install -m 600 /dev/null /root/cpa-management.key
sudoedit /root/cpa-management.key
sudo python3 install.py --apply --management-key-file /root/cpa-management.key
```

仅在密钥文件不存在时使用创建命令，避免覆盖已保存内容。文件保存实际密钥，不是配置中的 bcrypt 哈希。安装器不会更换或打印密钥。

实际安装会备份配置、共享库及面板，补充会话桥接，创建私有状态目录，然后重启 CPA 并校验插件注册。失败会恢复旧文件并重启。成功后记录输出的备份路径。

默认从 CPA 的 `requests.proxy-url` 继承查询代理；`--proxy-url direct` 可直连，或指定你自己的 HTTP / HTTPS / SOCKS5 代理。账号自身的代理优先。

自 0.2.2 起，后台复用 CPA 管理接口执行重置。安装器会将实际管理密钥写入状态目录的 `cpa-management.key`，仅服务用户可读（0600），并保存该文件的升级前版本以供失败恢复或回滚。密钥不会写入 YAML、策略或状态响应。轮换 CPA 管理密钥后需同步更新此文件，或重新运行安装器。

**从 0.2.1 升级不能只替换 `.so` 文件**：请运行新版安装器配置管理地址和私有密钥文件；否则自动查询和兑换会显示配置错误，额度保护继续运行。策略和去重 journal 保留。

## 自定义布局

```bash
sudo python3 install.py --apply \
  --config /etc/cliproxyapi/config.yaml \
  --panel /opt/cliproxyapi/static/management.html \
  --state-dir /var/lib/cliproxyapi/quota-guard \
  --service cliproxyapi --service-user cliproxyapi \
  --management-key-file /root/cpa-management.key
```

还支持 `--binary`、`--backup-dir` 和 `--proxy-url`；完整参数见 `python3 install.py --help`。插件目录与状态目录必须为绝对路径。安装器通过本机 HTTP 管理接口验证；CPA 本身只监听 HTTPS 的布局需要手工部署并验证。已有其他启用插件时，安装器会停止并要求先核对调度兼容性。

配置合并示例为 `config.example.yaml`，不是可替换整份 CPA 配置的文件。安装器保留配置值，但 YAML 注释和排版会重新生成。

## 在 CPAMC 中使用

1. 刷新 CPAMC 并正常登录，打开侧栏“账号额度保护”。
2. 查看账号、额度窗口和卡片。首次查询可能需要等待代理及上游响应。
3. 在账号卡片中启用额度保护，配置通用比例或独立窗口比例。
4. 在“策略设置”检查自动重置总开关、提前时间、额度保护模式和未知额度策略。
5. 保存后检查生效状态；在“执行记录”查看自动兑换结果。

页面直接沿用面板会话，无二次密钥输入。存在编辑草稿时自动刷新暂停；旧修订号保存冲突时重新载入再编辑。断开网页不会停止服务器工作。

## 升级与回滚

升级前保留 `policy.json` 和 `reset-journal.json`，不要在服务运行中删除或手动覆盖。运行新版本安装器时会使用现有策略，保留原保护模式及开关。

使用安装时输出的真实备份目录回滚：

```bash
sudo python3 install.py --rollback /root/cliproxyapi-plugin-backups/quota-guard-替换为实际备份目录
```

回滚恢复配置、共享库、CPAMC HTML 和升级前的插件管理密钥文件，保留策略及 journal。宿主重启期间请求可能短暂不可用。回滚旧代码前确认其能够读取当前状态格式；本安装器的回滚并非状态格式迁移工具。
