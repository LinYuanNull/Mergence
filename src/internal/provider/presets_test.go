package provider

import "testing"

// TestInferManagedPreset 覆盖面板回显要用到的几种推断场景。
//
// 这些用例的数据都取自真实配置：
//   - zcode：命令是 venv 里的 python.exe，参数 cli.py serve，探活 /meta
//   - workbuddy：单 exe，无参数，探活 /healthz
//
// 重点之一是「health_path 都是 /healthz 时不能一律判成 workbuddy」——
// 这正是旧实现「第一个命中」会犯的错。
func TestInferManagedPreset(t *testing.T) {
	cases := []struct {
		name       string
		command    string
		args       []string
		healthPath string
		portEnvVar string
		want       string
	}{
		{
			name:       "zcode：venv 里的 python.exe 绝对路径 + cli.py serve + /meta",
			command:    `C:/venvs/zcode2api/Scripts/python.exe`,
			args:       []string{"cli.py", "serve"},
			healthPath: "/meta",
			portEnvVar: "ZCODE_PORT",
			want:       "zcode",
		},
		{
			name:       "zcode：python3 归一化后等同 python",
			command:    "python3",
			args:       []string{"cli.py", "serve"},
			healthPath: "/meta",
			portEnvVar: "ZCODE_PORT",
			want:       "zcode",
		},
		{
			name:       "workbuddy 真实数据（wb2api.exe + 空 args + /healthz）",
			command:    "wb2api.exe",
			args:       nil,
			healthPath: "/healthz",
			portEnvVar: "WB2A_LISTEN",
			want:       "workbuddy",
		},
		{
			name:       "trae 老配置（node + server.js）：判不出，返回空（不猜）",
			command:    "node",
			args:       []string{"server.js"},
			healthPath: "/healthz",
			portEnvVar: "",
			// trae 已改为内置原生（Mode=native，无 command）。老配置的 command 是
			// 解释器名 `node`，不具区分度，**刻意不登记为 LegacyCommands**——
			// 登记了会把任何用 node 启动的渠道（含用户自建网关）都误判成 trae，
			// 那比判不出更糟（用户一保存就把自定义渠道变成内置 trae）。
			// 所以这里的正确结果是「判不出」，让面板停在下拉框第一项由用户自己选。
			want: "",
		},
		{
			name:       "trae 老配置带端口信号：同样判不出（不猜）",
			command:    "node",
			args:       []string{"server.js"},
			healthPath: "/healthz",
			portEnvVar: "PORT",
			want:       "",
		},
		{
			name:       "health_path 不认识 → 判不出，返回空",
			command:    "whatever.exe",
			args:       nil,
			healthPath: "/nothing",
			portEnvVar: "",
			want:       "",
		},
		{
			name: "空 command + /healthz → custom-managed",
			// 判断依据：command 归一化后为空，能命中这条的只剩「子进程型、且从不需要
			// command」的模板，即 custom-managed，它额外拿到 command 命中的 +2。
			// 两个模板被排除：workbuddy 带 LegacyCommands（曾经有 command 的形态），
			// trae 是原生型（有明确身份，回显走 kind 而非推断）。空命令的托管进程本来
			// 就最像「自定义托管进程（空白）」，判成它是合理且对面板更有用的结果
			// （否则下拉框又会停在第一个选项 workbuddy 上，正是要修的 bug）。
			command:    "",
			args:       nil,
			healthPath: "/healthz",
			portEnvVar: "",
			want:       "custom-managed",
		},
		{
			name: "最高分并列 → 返回空（不猜）",
			// 未知 command + /healthz：三个 /healthz 候选（workbuddy / trae /
			// custom-managed）都只命中门槛的 +3，谁也没有额外信号，并列无法区分。
			// 这是「判不出」的另一条路径——不是门槛淘汰（health_path 认识），
			// 而是信号不足。此时宁可返回空让用户自己选，也不随机挑一个。
			command:    "foo.exe",
			args:       nil,
			healthPath: "/healthz",
			portEnvVar: "",
			want:       "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := InferManagedPreset(tc.command, tc.args, tc.healthPath, tc.portEnvVar)
			if got != tc.want {
				t.Fatalf("InferManagedPreset(%q, %v, %q, %q) = %q, want %q",
					tc.command, tc.args, tc.healthPath, tc.portEnvVar, got, tc.want)
			}
		})
	}
}

// TestNormalizeCommand 单独盯住命令归一化：面板里同一个解释器可能写成
// 绝对路径、带 .exe、或 python3，推断时必须视作同一种东西。
func TestNormalizeCommand(t *testing.T) {
	cases := map[string]string{
		`C:/venvs/zcode2api/Scripts/python.exe`: "python",
		`C:\tools\wb2api.EXE`:                   "wb2api",
		"python3":                               "python",
		"python":                                "python",
		"":                                      "",
		"  ":                                    "",
	}
	for in, want := range cases {
		if got := normalizeCommand(in); got != want {
			t.Errorf("normalizeCommand(%q) = %q, want %q", in, got, want)
		}
	}
}
