# 兼容性与运行诊断

当前验证目标：CLIProxyAPI **8.0.35**、C ABI **1**、JSON schema **6**；CPAMC **1.26.5**。安装前要求宿主响应中的版本为 8.0.35，其他或无法识别的宿主版本在变更前拒绝。服务端预编译目标为 Linux amd64 / glibc，Ubuntu 24.04。Go 1.23.x 与 1.27.3 均由 CI 构建；ARM64、musl/Alpine、其他系统及 CPA/CPAMC 版本尚未验证。

CPAMC 认证桥接仍绑定已核实的原始 HTML 构建：

```text
57558cd023475674634a114824b014d3e5ac161f6f3791e3d86b6adab962a700
```

安装器幂等替换本插件桥接，不接受未知原始构建。对其他扩展，只有 `--panel-extensions` 中精确绑定完整标记区块 SHA256 的内容才允许保留；剥离声明区块后仍须匹配上述原始哈希。标记本身不构成信任。不要简单扩大哈希白名单或跳过检查。

安装器关闭 CPAMC 自动更新，避免上游升级覆盖桥接。升级面板前需重新核实认证存储、API 客户端、iframe、主题变量和会话隔离，并完成真实浏览器验收。中期仍应使用宿主提供的稳定插件会话接口替代压缩模块依赖；当前版本没有声称已解除这一绑定。

```bash
python3 install.py --doctor --management-key-file /root/cpa-management.key
```

doctor 输出宿主/插件版本、构建提交、时间、面板指纹、桥接支持及其他插件能力，不产生刷新或兑换请求。未提供密钥或未安装构建也可运行，只将相应项标为不可用。

其他插件兼容性按调度能力判定。新宿主能力字段优先；8.0.35 未公开 scheduler 标志，需为已审阅插件提供 SHA256 绑定声明。管理界面/SSO 插件不再被一律禁止；未知能力与确有调度冲突会在变更前阻止。说明见 [INSTALLATION.md](INSTALLATION.md)。

`dist/BUILD.json` 记录版本、commit、built_at、ABI/schema、目标与 `.so` SHA256，安装前校验并与安装后状态构建身份比对。发布包还有逐文件 `MANIFEST.json` 和外部 `SHA256SUMS`。这些校验用于完整性和追溯，不代替从受信任 Releases 获取文件。

仓库不包含上游 CPA 二进制或 CPAMC HTML。测试下载器固定官方版本、URL 和 SHA256，未使用 latest；校验失败必须停止。第三方许可见 `NOTICE`。
