// settings_close_test.go 「关窗行为」设置的端到端断言。
//
// 这个设置的特殊之处在于它有**三份状态**必须同时一致：
// 内存配置、磁盘配置文件、窗口过程（OnCloseBehaviorChange 回调）。
// 只测其中一份会漏掉最典型的故障：接口返回成功、重启后行为却回弹。
package web

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"mergence/internal/config"
)

func postClose(t *testing.T, base, body string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Post(base+"/api/settings/close", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var d map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, d
}

func statusMinimizeToTray(t *testing.T, base string) bool {
	t.Helper()
	resp, err := http.Get(base + "/api/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var d struct {
		MinimizeToTray *bool `json:"minimize_to_tray"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		t.Fatal(err)
	}
	if d.MinimizeToTray == nil {
		t.Fatal("/api/status 未返回 minimize_to_tray 字段")
	}
	return *d.MinimizeToTray
}

func TestCloseBehaviorSave(t *testing.T) {
	s, base := newTestServer(t, nil)

	var hooked atomic.Value
	s.OnCloseBehaviorChange = func(on bool) { hooked.Store(on) }

	// 默认应为「最小化到托盘」
	if !statusMinimizeToTray(t, base) {
		t.Fatal("初始状态应为最小化到托盘")
	}

	code, d := postClose(t, base, `{"minimize_to_tray": false}`)
	if code != http.StatusOK {
		t.Fatalf("期望 200，得到 %d（%v）", code, d)
	}
	if v, _ := d["minimize_to_tray"].(bool); v {
		t.Errorf("响应应回显新值 false，得到 %v", d["minimize_to_tray"])
	}
	// 内存
	if s.currentConfig().Tray.MinimizeToTray {
		t.Error("内存配置未更新")
	}
	// 状态接口（前端 radio 的回显源）
	if statusMinimizeToTray(t, base) {
		t.Error("/api/status 未反映新值")
	}
	// 窗口过程
	if v, ok := hooked.Load().(bool); !ok || v {
		t.Errorf("未把新行为通知窗口过程（OnCloseBehaviorChange），得到 %v", hooked.Load())
	}

	// 磁盘：重启后必须还是 false，否则这次设置等于没做
	onDisk, warns, err := config.Load(filepath.Join(filepath.Dir(s.cfgPath), "mergence.json"))
	if err != nil {
		t.Fatalf("读回配置失败：%v", err)
	}
	if len(warns) > 0 {
		t.Logf("读回配置时的提示：%v", warns)
	}
	if onDisk.Tray.MinimizeToTray {
		t.Error("磁盘配置未落盘，重启后会回弹成「最小化到托盘」")
	}

	// 切回去，并确认「值没变时不重复通知窗口过程」
	hooked = atomic.Value{}
	code, d = postClose(t, base, `{"minimize_to_tray": true}`)
	if code != http.StatusOK {
		t.Fatalf("期望 200，得到 %d（%v）", code, d)
	}
	if v, _ := d["minimize_to_tray"].(bool); !v {
		t.Errorf("响应应回显新值 true，得到 %v", d["minimize_to_tray"])
	}
	if v, ok := hooked.Load().(bool); !ok || !v {
		t.Errorf("切回托盘时应通知窗口过程，得到 %v", hooked.Load())
	}
	hooked = atomic.Value{}
	postClose(t, base, `{"minimize_to_tray": true}`)
	if hooked.Load() != nil {
		t.Errorf("值未变化时不应再通知窗口过程，得到 %v", hooked.Load())
	}
}

// 字段漏传必须报错，不能当成 false（= 直接退出）——
// 这是指针字段存在的唯一理由。
func TestCloseBehaviorMissingField(t *testing.T) {
	_, base := newTestServer(t, nil)
	code, d := postClose(t, base, `{}`)
	if code != http.StatusBadRequest {
		t.Fatalf("缺少 minimize_to_tray 应返回 400，得到 %d（%v）", code, d)
	}
	if msg, _ := d["error"].(string); !strings.Contains(msg, "minimize_to_tray") {
		t.Errorf("错误信息应点名缺失字段，得到 %q", msg)
	}
}

// 关掉托盘驻留后，托盘生命周期不受影响：接口不该去动托盘线程
// （那需要重构 Tray 的一次性 channel，刻意不做）。
// 这里钉住「接口不会因为 minimize_to_tray=false 而报错或崩」。
func TestCloseBehaviorNoopWithoutDesktop(t *testing.T) {
	_, base := newTestServer(t, nil) // 未注入 OnCloseBehaviorChange，等价 headless
	if code, d := postClose(t, base, `{"minimize_to_tray": false}`); code != http.StatusOK {
		t.Fatalf("无窗口层时也应保存成功，得到 %d（%v）", code, d)
	}
	if statusMinimizeToTray(t, base) {
		t.Error("无窗口层时状态接口仍应反映新值")
	}
}
