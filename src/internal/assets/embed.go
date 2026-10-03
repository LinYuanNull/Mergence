// embed.go 面板前端资源（HTML / JS / CSS）。
//
// 单独一个包、而不是留在 internal/web 里，理由两条：
//
//  1. 物理边界。internal/web 是 Go 源码目录，混进约 270 KB 前端资源后，
//     「改界面」和「改后端」在文件树上就分不开了。app.js / index.html /
//     app.css 是改动最密集的三个文件，把它们的归属落到目录上，比靠约定可靠。
//
//  2. go:embed 的硬约束。embed 模式**不允许 `..`**，也不能跨包引用文件；
//     资源一旦离开 internal/web，就必须由「资源所在的那个包」自己声明。
//
// 资源对外暴露的 URL 不变（/app.js、/app.css、/theme.js 等），
// 这次搬迁对前端与任何外部使用方都是透明的。
package assets

import _ "embed"

// IndexHTML 面板页面。
//
//go:embed index.html
var IndexHTML []byte

// ThemeJS 首帧主题脚本。必须由 index.html 在 <head> 里**同步**引入——
// 它跑得越晚，用户看到的主题闪烁越明显。
//
//go:embed theme.js
var ThemeJS []byte

// AppJS 面板框架：渠道管理、日志、设置、桌面桥。
//
//go:embed app.js
var AppJS []byte

// UpstreamJS 上游控制台六视图（账号池 / 任务 / 模型 / 用量 / 配置 / 日志）。
//
//go:embed upstream.js
var UpstreamJS []byte

// OverviewJS 概览指标视图。
//
//go:embed overview.js
var OverviewJS []byte

// ZcodeJS zcode2api 系列网关的原生控制台视图。
//
//go:embed zcode.js
var ZcodeJS []byte

// AppCSS 面板全部样式。
//
//go:embed app.css
var AppCSS []byte
