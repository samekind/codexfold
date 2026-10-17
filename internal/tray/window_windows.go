//go:build windows

package tray

import (
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/jchv/go-webview2/pkg/edge"
	"golang.org/x/sys/windows"
)

//go:embed dashboard.html
var dashboardHTML string

var (
	user32        = windows.NewLazySystemDLL("user32.dll")
	shell32       = windows.NewLazySystemDLL("shell32.dll")
	registerClass = user32.NewProc("RegisterClassExW")
	createWindow  = user32.NewProc("CreateWindowExW")
	defWindowProc = user32.NewProc("DefWindowProcW")
	showWindow    = user32.NewProc("ShowWindow")
	setForeground = user32.NewProc("SetForegroundWindow")
	postMessage   = user32.NewProc("PostMessageW")
	notifyIcon    = shell32.NewProc("Shell_NotifyIconW")
	moveMemory    = windows.NewLazySystemDLL("kernel32.dll").NewProc("RtlMoveMemory")
)

const (
	wmTray              = 0x8001
	wmShow              = 0x8002
	wmRefresh           = 0x8003
	idOpen              = 100
	idStore             = 101
	idLogs              = 102
	idRefresh           = 103
	idExit              = 104
	idWorkerLogs        = 105
	idCopyDiagnostics   = 106
	idExportDiagnostics = 107
	idWindowMinimize    = 108
	idWindowMaximize    = 109
	idWindowHide        = 110
	idWindowDrag        = 111
	// rsrc emits the manifest as 1, then each group followed by its nine images.
	appIconResource       = 2
	attentionIconResource = 12
)

type point struct{ X, Y int32 }
type rect struct{ Left, Top, Right, Bottom int32 }
type message struct {
	Window         uintptr
	ID             uint32
	WParam, LParam uintptr
	Time           uint32
	Point          point
	Private        uint32
}
type windowClass struct {
	Size, Style                        uint32
	Procedure                          uintptr
	ClassExtra, WindowExtra            int32
	Instance, Icon, Cursor, Background uintptr
	MenuName, ClassName                *uint16
	SmallIcon                          uintptr
}
type iconData struct {
	Size                uint32
	Window              uintptr
	ID, Flags, Callback uint32
	Icon                uintptr
	Tip                 [128]uint16
	State, StateMask    uint32
	Info                [256]uint16
	Timeout             uint32
	InfoTitle           [64]uint16
	InfoFlags           uint32
	GUID                windows.GUID
	BalloonIcon         uintptr
}

type application struct {
	window                            uintptr
	monitor                           *Monitor
	browser                           *edge.Chromium
	icon                              iconData
	largeIcon, smallIcon, warningIcon uintptr
	instance                          windows.Handle
	taskbarMessage                    uint32
	scale                             float64
	view                              View
	results                           chan View
	refresh                           chan struct{}
	stop                              chan struct{}
	pollDone                          chan struct{}
	actions                           chan PolicyChange
	notifiedIncidents                 map[string]bool
}

func utf16(value string) *uint16 {
	p, _ := windows.UTF16PtrFromString(strings.ReplaceAll(value, "\x00", ""))
	return p
}
func pointer(value *uint16) uintptr         { return uintptr(unsafe.Pointer(value)) }
func (a *application) px(value int) uintptr { return uintptr(int(float64(value) * a.scale)) }

func ShowError(err error) {
	user32.NewProc("MessageBoxW").Call(0, pointer(utf16(err.Error())), pointer(utf16("CodexFold")), 0x10)
}

func RequestExit(store string) error {
	key := fmt.Sprintf("CodexFold.Tray.%x", sha256.Sum256([]byte(strings.ToLower(filepath.Clean(store)))))
	window, _, _ := user32.NewProc("FindWindowW").Call(pointer(utf16(key)), 0)
	if window == 0 {
		return nil
	}
	if sent, _, err := postMessage.Call(window, 0x111, idExit, 0); sent == 0 {
		return fmt.Errorf("request tray exit: %w", err)
	}
	return nil
}

func Run(store string, background bool) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	// The executable manifest sets PerMonitorV2 before any COM windows exist.
	// Keep a fallback for hosts that call this package without that manifest.
	if proc := user32.NewProc("SetProcessDpiAwarenessContext"); proc.Find() == nil {
		proc.Call(^uintptr(3)) // DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2 (-4)
	} else {
		user32.NewProc("SetProcessDPIAware").Call()
	}
	key := fmt.Sprintf("CodexFold.Tray.%x", sha256.Sum256([]byte(strings.ToLower(filepath.Clean(store)))))
	mutex, err := windows.CreateMutex(nil, false, utf16("Local\\"+key))
	if mutex != 0 {
		defer windows.CloseHandle(mutex)
	}
	if err == windows.ERROR_ALREADY_EXISTS {
		window, _, _ := user32.NewProc("FindWindowW").Call(pointer(utf16(key)), 0)
		if window != 0 {
			postMessage.Call(window, wmShow, 0, 0)
		}
		return nil
	}
	if err != nil {
		return err
	}
	a := &application{monitor: NewMonitor(store), scale: 1, results: make(chan View, 1), refresh: make(chan struct{}, 1), stop: make(chan struct{}), pollDone: make(chan struct{}), actions: make(chan PolicyChange, 1), notifiedIncidents: make(map[string]bool)}
	if proc := user32.NewProc("GetDpiForSystem"); proc.Find() == nil {
		if dpi, _, _ := proc.Call(); dpi != 0 {
			a.scale = float64(dpi) / 96
		}
	}
	cursor, _, _ := user32.NewProc("LoadCursorW").Call(0, 32512)
	var instance windows.Handle
	err = windows.GetModuleHandleEx(0, nil, &instance)
	if err != nil {
		return err
	}
	a.instance = instance
	if err := a.loadIcons(); err != nil {
		return err
	}
	class := windowClass{Procedure: syscall.NewCallback(a.procedure), Instance: uintptr(instance), Icon: a.largeIcon, Cursor: cursor, Background: 16, ClassName: utf16(key), SmallIcon: a.smallIcon}
	class.Size = uint32(unsafe.Sizeof(class))
	if value, _, err := registerClass.Call(uintptr(unsafe.Pointer(&class))); value == 0 {
		return fmt.Errorf("register status window: %w", err)
	}
	// Clip child surfaces so repainting the native frame cannot cover WebView2.
	bounds := fitWindow(primaryWorkArea(), a.scale)
	window, _, err := createWindow.Call(0, pointer(class.ClassName), pointer(utf16("CodexFold")), 0x02cf0000, uintptr(bounds.Left), uintptr(bounds.Top), uintptr(bounds.Right-bounds.Left), uintptr(bounds.Bottom-bounds.Top), 0, 0, uintptr(instance), 0)
	if window == 0 {
		return fmt.Errorf("create status window: %w", err)
	}
	a.window = window
	defer user32.NewProc("DestroyWindow").Call(window)
	a.browser = edge.NewChromium()
	cache, err := os.UserCacheDir()
	if err != nil {
		return err
	}
	a.browser.DataPath = filepath.Join(cache, "CodexFold", "WebView2")
	a.browser.SetGlobalPermission(edge.CoreWebView2PermissionStateDeny)
	a.browser.MessageCallback = func(action string) {
		// Only fixed actions from the embedded page cross the native boundary.
		// No path, command line, or arbitrary code is accepted from JavaScript.
		switch action {
		case "ready":
			a.update(a.view)
		case "store":
			postMessage.Call(window, 0x111, idStore, 0)
		case "logs":
			postMessage.Call(window, 0x111, idLogs, 0)
		case "refresh":
			postMessage.Call(window, 0x111, idRefresh, 0)
		case "logs-worker":
			postMessage.Call(window, 0x111, idWorkerLogs, 0)
		case "copy-diagnostics":
			postMessage.Call(window, 0x111, idCopyDiagnostics, 0)
		case "export-diagnostics":
			postMessage.Call(window, 0x111, idExportDiagnostics, 0)
		case "exit":
			postMessage.Call(window, 0x111, idExit, 0)
		case "window-minimize":
			postMessage.Call(window, 0x111, idWindowMinimize, 0)
		case "window-maximize":
			postMessage.Call(window, 0x111, idWindowMaximize, 0)
		case "window-hide":
			postMessage.Call(window, 0x111, idWindowHide, 0)
		case "window-drag":
			postMessage.Call(window, 0x111, idWindowDrag, 0)
		default:
			if strings.HasPrefix(action, "{") {
				request, err := decodePolicyChange(action)
				if err != nil {
					a.reply(ActionResult{ID: request.ID, Message: err.Error()})
					return
				}
				select {
				case a.actions <- request:
				default:
					a.reply(ActionResult{ID: request.ID, Message: "正在保存上一项设置，请稍后重试。"})
				}
			}
		}
	}
	if !a.browser.Embed(window) {
		return fmt.Errorf("无法初始化 Microsoft Edge WebView2 Runtime，请安装后重新打开")
	}
	settings, err := a.browser.GetSettings()
	if err != nil {
		return err
	}
	if err := settings.PutAreDefaultContextMenusEnabled(false); err != nil {
		return err
	}
	if err := settings.PutAreDevToolsEnabled(false); err != nil {
		return err
	}
	a.browser.NavigateToString(dashboardHTML)
	// Use the Windows 11 rounded frame when available; older Windows ignores it.
	dwm := windows.NewLazySystemDLL("dwmapi.dll").NewProc("DwmSetWindowAttribute")
	corner, border := uint32(2), uint32(0xfffffffe)
	dwm.Call(window, 33, uintptr(unsafe.Pointer(&corner)), 4)
	dwm.Call(window, 34, uintptr(unsafe.Pointer(&border)), 4)
	// Recalculate the client area after the custom glass caption is ready.
	user32.NewProc("SetWindowPos").Call(window, 0, 0, 0, 0, 0, 0x37) // FRAMECHANGED | NOMOVE | NOSIZE | NOZORDER | NOACTIVATE
	registered, _, _ := user32.NewProc("RegisterWindowMessageW").Call(pointer(utf16("TaskbarCreated")))
	a.taskbarMessage = uint32(registered)
	a.icon = iconData{Window: window, ID: 1, Flags: 7, Callback: wmTray, Icon: a.smallIcon}
	a.icon.Size = uint32(unsafe.Sizeof(a.icon))
	a.setTip("CodexFold · 正在读取状态")
	if result, _, err := notifyIcon.Call(0, uintptr(unsafe.Pointer(&a.icon))); result == 0 {
		return fmt.Errorf("add notification icon: %w", err)
	}
	defer notifyIcon.Call(2, uintptr(unsafe.Pointer(&a.icon)))
	a.layout()
	a.update(View{Health: "正在读取状态", Storage: "空间统计：暂无数据", Activity: "读取 —    写入 —", Sessions: "托管会话：—", Detail: "正在读取本地后台状态…"})
	go a.poll()
	defer func() {
		close(a.stop)
		select {
		case <-a.pollDone:
		case <-time.After(3 * time.Second):
		}
	}()
	if !background {
		a.show()
	}
	var msg message
	for {
		result, _, err := user32.NewProc("GetMessageW").Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0)
		if int32(result) == -1 {
			return err
		}
		if result == 0 {
			return nil
		}
		if handled, _, _ := user32.NewProc("IsDialogMessageW").Call(window, uintptr(unsafe.Pointer(&msg))); handled != 0 {
			continue
		}
		user32.NewProc("TranslateMessage").Call(uintptr(unsafe.Pointer(&msg)))
		user32.NewProc("DispatchMessageW").Call(uintptr(unsafe.Pointer(&msg)))
	}
}

func (a *application) poll() {
	defer close(a.pollDone)
	defer a.monitor.FlushHistory()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	var operation *ActionResult
	for {
		view := a.monitor.Refresh(time.Now())
		view.Operation = operation
		select {
		case a.results <- view:
		default:
			select {
			case <-a.results:
			default:
			}
			select {
			case a.results <- view:
			default:
			}
		}
		postMessage.Call(a.window, wmRefresh, 0, 0)
		select {
		case <-a.stop:
			return
		case <-ticker.C:
		case <-a.refresh:
		case request := <-a.actions:
			result := a.monitor.ApplyPolicy(request)
			operation = &result
		}
	}
}

func (a *application) update(view View) {
	a.view = view
	if a.browser != nil {
		payload, err := json.Marshal(view)
		if err == nil {
			a.browser.Eval("window.updateStatus && window.updateStatus(" + string(payload) + ")")
		}
	}
	a.icon.Icon = a.smallIcon
	if view.Health == "需要关注" {
		a.icon.Icon = a.warningIcon
	}
	a.setTip("CodexFold · " + view.Health + "\n" + view.Activity)
	notifyIcon.Call(1, uintptr(unsafe.Pointer(&a.icon)))
	for _, incident := range view.Incidents {
		if incident.RecoveredAt == nil && !a.notifiedIncidents[incident.ID] {
			a.notifiedIncidents[incident.ID] = true
			a.icon.Flags = 0x10 // NIF_INFO: request one native notification per incident.
			a.icon.InfoTitle, a.icon.Info = [64]uint16{}, [256]uint16{}
			copy(a.icon.InfoTitle[:], windows.StringToUTF16("CodexFold · 需要关注"))
			copy(a.icon.Info[:], windows.StringToUTF16(incident.Reason+"。打开状态窗口查看详情。"))
			a.icon.InfoFlags = 2
			notifyIcon.Call(1, uintptr(unsafe.Pointer(&a.icon)))
			a.icon.Flags = 7
			break
		}
	}
}

func (a *application) setTip(value string) {
	a.icon.Tip = [128]uint16{}
	encoded, _ := windows.UTF16FromString(value)
	if len(encoded) > 127 {
		encoded = encoded[:127]
		if encoded[126] >= 0xd800 && encoded[126] <= 0xdbff {
			encoded = encoded[:126]
		}
	}
	copy(a.icon.Tip[:], encoded)
}

func (a *application) layout() {
	if a.browser != nil && a.browser.GetController() != nil {
		a.browser.Resize()
	}
}

func primaryWorkArea() rect {
	var work rect
	if ok, _, _ := user32.NewProc("SystemParametersInfoW").Call(0x30, 0, uintptr(unsafe.Pointer(&work)), 0); ok != 0 {
		return work
	}
	width, _, _ := user32.NewProc("GetSystemMetrics").Call(0)
	height, _, _ := user32.NewProc("GetSystemMetrics").Call(1)
	return rect{Right: int32(width), Bottom: int32(height)}
}

func windowWorkArea(window uintptr) rect {
	var info struct {
		Size          uint32
		Monitor, Work rect
		Flags         uint32
	}
	info.Size = uint32(unsafe.Sizeof(info))
	monitor, _, _ := user32.NewProc("MonitorFromWindow").Call(window, 2)
	if ok, _, _ := user32.NewProc("GetMonitorInfoW").Call(monitor, uintptr(unsafe.Pointer(&info))); ok != 0 {
		return info.Work
	}
	return primaryWorkArea()
}

func fitWindow(work rect, scale float64) rect {
	margin := int32(16 * scale)
	width := min(int32(1000*scale), max(1, work.Right-work.Left-2*margin))
	height := min(int32(780*scale), max(1, work.Bottom-work.Top-2*margin))
	left := work.Left + (work.Right-work.Left-width)/2
	top := work.Top + (work.Bottom-work.Top-height)/2
	return rect{left, top, left + width, top + height}
}

func frameResizeHit(bounds rect, cursor point, edge int32) uintptr {
	left, right := cursor.X < bounds.Left+edge, cursor.X >= bounds.Right-edge
	top, bottom := cursor.Y < bounds.Top+edge, cursor.Y >= bounds.Bottom-edge
	switch {
	case top && left:
		return 13
	case top && right:
		return 14
	case bottom && left:
		return 16
	case bottom && right:
		return 17
	case left:
		return 10
	case right:
		return 11
	case top:
		return 12
	case bottom:
		return 15
	default:
		return 0
	}
}

func (a *application) loadIcons() error {
	load := func(resource uintptr, size int) (uintptr, error) {
		icon, _, err := user32.NewProc("LoadImageW").Call(uintptr(a.instance), resource, 1, a.px(size), a.px(size), 0x8000) // IMAGE_ICON, LR_SHARED
		if icon == 0 {
			return 0, fmt.Errorf("load application icon %d: %w", resource, err)
		}
		return icon, nil
	}
	var err error
	if a.largeIcon, err = load(appIconResource, 32); err != nil {
		return err
	}
	if a.smallIcon, err = load(appIconResource, 16); err != nil {
		return err
	}
	a.warningIcon, err = load(attentionIconResource, 16)
	return err
}

func (a *application) show() {
	showWindow.Call(a.window, 9) // restore a minimized window
	// STARTF_USESHOWWINDOW can override the first ShowWindow call with SW_HIDE.
	// SetWindowPos applies our explicit open request even for a hidden launcher.
	user32.NewProc("SetWindowPos").Call(a.window, 0, 0, 0, 0, 0, 0x47) // NOSIZE | NOMOVE | NOZORDER | SHOWWINDOW
	a.syncBrowserVisibility(true)
	a.layout()
	if a.browser != nil {
		_ = a.browser.NotifyParentWindowPositionChanged()
		a.browser.Focus()
	}
	user32.NewProc("UpdateWindow").Call(a.window)
	setForeground.Call(a.window)
}

func (a *application) syncBrowserVisibility(visible bool) {
	if a.browser == nil || a.browser.GetController() == nil {
		return
	}
	if visible {
		_ = a.browser.Show()
	} else {
		_ = a.browser.Hide()
	}
}

func (a *application) openFolder(path string) {
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		user32.NewProc("MessageBoxW").Call(a.window, pointer(utf16("该目录尚未创建：\n"+path)), pointer(utf16("CodexFold")), 0x40)
		return
	}
	result, _, _ := shell32.NewProc("ShellExecuteW").Call(a.window, pointer(utf16("open")), pointer(utf16(path)), 0, 0, 1)
	if result <= 32 {
		ShowError(fmt.Errorf("无法打开目录 (%d): %s", result, path))
	}
}

func (a *application) command(id uintptr) {
	switch id {
	case idOpen:
		a.show()
	case idStore:
		a.openFolder(a.monitor.Store)
	case idLogs:
		path := a.view.LogDirectories["filesystem"]
		if path == "" {
			path = filepath.Join(a.monitor.Store, "fs")
		}
		a.openFolder(path)
	case idWorkerLogs:
		a.openFolder(filepath.Join(a.monitor.Store, "enrollment"))
	case idCopyDiagnostics:
		a.copyDiagnostics()
	case idExportDiagnostics:
		a.exportDiagnostics()
	case idRefresh:
		a.monitor.RequestAccountingRefresh()
		select {
		case a.refresh <- struct{}{}:
		default:
		}
	case idExit:
		user32.NewProc("PostQuitMessage").Call(0)
	case idWindowMinimize:
		showWindow.Call(a.window, 6)
	case idWindowMaximize:
		if maximized, _, _ := user32.NewProc("IsZoomed").Call(a.window); maximized != 0 {
			showWindow.Call(a.window, 9)
		} else {
			showWindow.Call(a.window, 3)
		}
	case idWindowHide:
		postMessage.Call(a.window, 0x10, 0, 0)
	case idWindowDrag:
		user32.NewProc("ReleaseCapture").Call()
		postMessage.Call(a.window, 0xa1, 2, 0) // WM_NCLBUTTONDOWN / HTCAPTION
	}
}

func (a *application) popup() {
	a.show()
	if a.browser != nil {
		a.browser.Eval("window.openGlassMenu && window.openGlassMenu()")
	}
}

func (a *application) procedure(window uintptr, msg uint32, wparam, lparam uintptr) uintptr {
	if a.taskbarMessage != 0 && msg == a.taskbarMessage {
		notifyIcon.Call(0, uintptr(unsafe.Pointer(&a.icon)))
		return 0
	}
	switch msg {
	case 0x83: // WM_NCCALCSIZE: use the web caption while retaining the native sizing frame.
		if wparam != 0 {
			var bounds rect
			// LPARAM belongs to Windows. Copy through the native API rather
			// than retaining an unmanaged address as a Go pointer.
			moveMemory.Call(uintptr(unsafe.Pointer(&bounds)), lparam, unsafe.Sizeof(bounds))
			if maximized, _, _ := user32.NewProc("IsZoomed").Call(window); maximized != 0 {
				bounds = windowWorkArea(window)
			} else {
				// Keep a native sizing rim outside the WebView child so the
				// child cannot intercept pointer hit tests at the window edges.
				rim := int32(a.px(6))
				bounds.Left += rim
				bounds.Top += rim
				bounds.Right -= rim
				bounds.Bottom -= rim
			}
			moveMemory.Call(lparam, uintptr(unsafe.Pointer(&bounds)), unsafe.Sizeof(bounds))
			return 0
		}
	case 0x84: // WM_NCHITTEST: signed coordinates also work on left-hand monitors.
		if maximized, _, _ := user32.NewProc("IsZoomed").Call(window); maximized == 0 {
			var bounds rect
			user32.NewProc("GetWindowRect").Call(window, uintptr(unsafe.Pointer(&bounds)))
			cursor := point{int32(int16(lparam & 0xffff)), int32(int16((lparam >> 16) & 0xffff))}
			if hit := frameResizeHit(bounds, cursor, int32(a.px(6))); hit != 0 {
				return hit
			}
		}
		return 1 // HTCLIENT: the caption drag regions are sent explicitly by the page.
	case wmShow:
		a.show()
		return 0
	case wmRefresh:
		select {
		case view := <-a.results:
			a.update(view)
		default:
		}
		return 0
	case wmTray:
		switch uint32(lparam) {
		case 0x202, 0x203:
			a.show()
		case 0x205:
			a.popup()
		}
		return 0
	case 0x111:
		a.command(wparam & 0xffff)
		return 0
	case 0x10:
		showWindow.Call(window, 0)
		return 0
	case 0x18: // WM_SHOWWINDOW: keep the controller in sync with the tray window
		a.syncBrowserVisibility(wparam != 0)
		return 0
	case 0x5:
		visible, _, _ := user32.NewProc("IsWindowVisible").Call(window)
		a.syncBrowserVisibility(wparam != 1 && visible != 0) // SIZE_MINIMIZED
		a.layout()
		return 0
	case 0x3, 0x216: // WM_MOVE, WM_MOVING
		if a.browser != nil {
			_ = a.browser.NotifyParentWindowPositionChanged()
		}
	case 0x2e0: // WM_DPICHANGED
		a.scale = float64(wparam&0xffff) / 96
		if a.instance != 0 {
			if err := a.loadIcons(); err == nil {
				user32.NewProc("SendMessageW").Call(window, 0x80, 1, a.largeIcon)
				user32.NewProc("SendMessageW").Call(window, 0x80, 0, a.smallIcon)
				if a.icon.Size != 0 {
					a.update(a.view)
				}
			}
		}
		var bounds rect
		moveMemory.Call(uintptr(unsafe.Pointer(&bounds)), lparam, unsafe.Sizeof(bounds))
		user32.NewProc("SetWindowPos").Call(window, 0, uintptr(bounds.Left), uintptr(bounds.Top), uintptr(bounds.Right-bounds.Left), uintptr(bounds.Bottom-bounds.Top), 0x14) // NOZORDER | NOACTIVATE
		a.layout()
		return 0
	case 0x24: // minimum size, in physical pixels
		var limits [5]point
		moveMemory.Call(uintptr(unsafe.Pointer(&limits)), lparam, unsafe.Sizeof(limits))
		work := windowWorkArea(window)
		limits[3] = point{min(int32(a.px(540)), work.Right-work.Left), min(int32(a.px(420)), work.Bottom-work.Top)}
		moveMemory.Call(lparam, uintptr(unsafe.Pointer(&limits)), unsafe.Sizeof(limits))
		return 0
	case 0x2:
		user32.NewProc("PostQuitMessage").Call(0)
		return 0
	}
	result, _, _ := defWindowProc.Call(window, uintptr(msg), wparam, lparam)
	return result
}
