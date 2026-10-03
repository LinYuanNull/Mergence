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
			name:       "zcode 真实数据（venv python.exe + cli.py serve + /meta）",
			command:    `D:/AiWork/ZCode/zcode2api/.venv/Scripts/python.exe`,
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
			name:       "trae：node + server.js + /healthz，不能误判成 workbuddy",
			command:    "node",
			args:       []string{"server.js"},
			healthPath: "/healthz",
			// 题目给的最小信号未含端口；不填也应靠 args+command 压过 workbuddy。
			portEnvVar: "",
			want:       "trae",
		},
		{
			name:       "trae：带上端口信号更稳",
			command:    "node",
			args:       []string{"server.js"},
			healthPath: "/healthz",
			portEnvVar: "PORT",
			want:       "trae",
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
			// 判断依据：command 归一化后为空，只有 custom-managed 的 command 也是空，
			// 于是它额外拿到 command 命中的 +2；workbuddy 虽同样命中 health/args，
			// 但 command 为空对不上 wb2api，得分更低。空命令的托管进程本来就最像
			// 「自定义托管进程（空白）」这个模板，判成它是合理且对面板更有用的结果
			// （否则下拉框又会停在第一个选项 workbuddy 上，正是要修的 bug）。
			command:    "",
			args:       nil,
			healthPath: "/healthz",
			portEnvVar: "",
			want:       "custom-managed",
		},
		{
			name: "最高分并列 → 返回空（不猜）",
			// command 为空、args 命中 server.js：trae = health3 + args2 = 5；
			// custom-managed = health3 + command2 = 5，两者并列，无法区分。
			command:    "",
			args:       []string{"server.js"},
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
		`D:/AiWork/ZCode/zcode2api/.venv/Scripts/python.exe`: "python",
		`C:\tools\wb2api.EXE`:                                "wb2api",
		"python3":                                            "python",
		"python":                                             "python",
		"":                                                   "",
		"  ":                                                 "",
	}
	for in, want := range cases {
		if got := normalizeCommand(in); got != want {
			t.Errorf("normalizeCommand(%q) = %q, want %q", in, got, want)
		}
	}
}
