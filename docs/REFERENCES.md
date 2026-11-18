# 参考项目与开源组件

软件界面不再展示参考项目清单，统一在本文件维护。以下为当前 Go 版本实际使用或直接参考的开源项目/组件；完整的许可义务（含 Frida LGPL 对应源码要求）见 `docs/THIRD_PARTY_NOTICES.md`。

| 项目 | 用途 | 许可 |
|---|---|---|
| [Wails v2](https://github.com/wailsapp/wails) | 桌面窗口与 WebView2 绑定 | MIT |
| [gorilla/websocket](https://github.com/gorilla/websocket) | CDP 与私有调试桥 WebSocket | BSD-3-Clause |
| [modelcontextprotocol/go-sdk](https://github.com/modelcontextprotocol/go-sdk) | MCP 服务端与 stdio/HTTP 传输 | MIT |
| [modernc.org/sqlite](https://gitlab.com/cznic/sqlite) | 纯 Go SQLite（登录服务与索引） | BSD-3-Clause |
| [google/uuid](https://github.com/google/uuid) | 标识生成 | BSD-3-Clause |
| [golang.org/x/sys](https://pkg.go.dev/golang.org/x/sys) | Windows API | BSD-3-Clause |
| [go-ole](https://github.com/go-ole/go-ole) / [go-winloader](https://github.com/jchv/go-winloader) | COM 与动态加载 | MIT |
| [Frida](https://frida.re/) | 原生观测 worker 的注入与跟踪 | LGPL-2.1-or-later |
| [KillWxapkg (Ackites)](https://github.com/Ackites/KillWxapkg) | V1MMWX 包离线解密与还原 | MIT |
| WebView2 Runtime | 系统 WebView2 运行时（不随精简包分发） | Microsoft 条款 |

说明：

- 登录服务源码来自原始项目 `services/login`（`yyb_go` 模块），已复制到 `native/services/login` 并在同进程内运行，不再分发独立 exe。
- 私有调试桥的 wire format 由 `native/internal/bridge/codec.go` 用 Go 标准库独立实现。
- 微信运行时（WeChatAppEx、flue.dll 等）不随包分发，由用户本机提供，实际来源为应用宝 PC 的 `WmpfRuntime\5.10.7300.414\` 目录。
