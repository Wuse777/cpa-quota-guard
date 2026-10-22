# 参与开发

欢迎提交问题和 Pull Request。提交前请阅读 README 的兼容性和行为限制。

## 本地检查

在 Linux 环境安装 Go、GCC、Python 3、Node.js 和 Python 依赖后运行：

```bash
python3 -m pip install -r requirements.txt
bash scripts/check.sh
```

检查会执行 Go race 测试、vet、Node 会话测试和 Python 安装/桥接测试，不使用真实账号。宿主集成与 systemd 安装验收见 [测试文档](docs/TESTING.md)。

Go 文件使用 `gofmt`；保留 Linux 换行。修改调度、持久化或兑换逻辑时，补充能够验证外部行为和失败场景的测试。UI 调整需要核对 CPAMC 主题、键盘操作、窄屏布局和减少动态效果设置。

## 提交内容

- 描述问题、最终行为和实际运行的检查。
- 使用虚构账号、`example.invalid` 邮箱和本地 HTTP 服务复现。
- 不提交生成二进制、运行配置、账号授权、策略状态、私有截图、服务器地址或个人部署日志。
- 保留第三方许可；贡献代码按本仓库 MIT 许可证发布。

不要将修改认证、会话桥接或未知面板兼容性校验作为无关修复混入提交。提交安全问题前阅读 [SECURITY.md](SECURITY.md)。
