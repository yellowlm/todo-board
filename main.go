//go:build windows

package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	webview2 "github.com/jchv/go-webview2"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const (
	appTitle    = "待办看板"
	appName     = "AgentTodoBoard"
	defaultPort = 17321
	windowW     = 820
	windowH     = 640

	minClientW = 560
	minClientH = 140

	editorW0 = 480
	editorH0 = 560

	editorMinW = 360
	editorMinH = 420

	trayTip = "待办看板"
	trayUID = 1
)

// 窗口样式
const (
	gwlStyle    = ^uintptr(15) // -16
	gwlExStyle  = ^uintptr(19) // -20
	gwlpWndProc = ^uintptr(3)  // -4

	wsCaption      = 0x00C00000
	wsMaximizeBox  = 0x00010000
	wsMinimizeBox  = 0x00020000
	wsThickFrame   = 0x00040000
	wsSysMenu      = 0x00080000
	wsExToolWindow = 0x00000080
	wsExAppWindow  = 0x00040000

	swHide    = 0
	swShow    = 5
	swRestore = 9

	swpNoSize       = 0x0001
	swpNoMove       = 0x0002
	swpNoZOrder     = 0x0004
	swpNoActivate   = 0x0010
	swpFrameChanged = 0x0020
	swpNoRedraw     = 0x0008
)

// 消息
const (
	wmNull          = 0x0000
	wmDestroy       = 0x0002
	wmSize          = 0x0005
	wmClose         = 0x0010
	wmCommand       = 0x0111
	wmLButtonUp     = 0x0202
	wmLButtonDblClk = 0x0203
	wmRButtonUp     = 0x0205
	wmContextMenu   = 0x007B
	wmNCLButtonDown = 0x00A1
	wmMove          = 0x0003
	wmNCCalcSize    = 0x0083
	wmNCHitTest     = 0x0084
	wmCancelMode    = 0x001F
	wmSetRedraw     = 0x000B
	vkLeftButton    = 0x01
	wmSysCommand    = 0x0112
	scSize          = 0xF000
	wmExitSizeMove  = 0x0232

	htTransparent = ^uintptr(0)
	htClient      = 1
	htLeft        = 10
	htRight       = 11
	htTop         = 12
	htTopLeft     = 13
	htTopRight    = 14
	htBottom      = 15
	htBottomLeft  = 16
	htBottomRight = 17

	htCaption = 2

	// 自定义托盘回调消息，避开库内部使用的 WM_APP(0x8000)
	wmTray = 0x8001
)

// 托盘
const (
	nimAdd    = 0
	nimModify = 1
	nimDelete = 2

	nifMessage = 0x1
	nifIcon    = 0x2
	nifTip     = 0x4

	imageIcon      = 1
	lrLoadFromFile = 0x0010
	lrDefaultSize  = 0x0040
	idiApplication = 32512

	mfString    = 0x00000000
	mfSeparator = 0x00000800
	mfChecked   = 0x00000008

	tpmRightButton = 0x0002
	tpmBottomAlign = 0x0020
	tpmReturnCmd   = 0x0100

	menuOpen      = 1
	menuAutostart = 2
	menuQuit      = 3
)

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	shell32  = windows.NewLazySystemDLL("shell32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procRegisterWindowMessageW = user32.NewProc("RegisterWindowMessageW")
	procCallWindowProcW        = user32.NewProc("CallWindowProcW")
	procDefWindowProcW         = user32.NewProc("DefWindowProcW")
	procSendMessageW           = user32.NewProc("SendMessageW")
	procPostMessageW           = user32.NewProc("PostMessageW")
	procReleaseCapture         = user32.NewProc("ReleaseCapture")
	procSetCapture             = user32.NewProc("SetCapture")
	procInvalidateRect         = user32.NewProc("InvalidateRect")
	procUpdateWindow           = user32.NewProc("UpdateWindow")
	procGetAsyncKeyState       = user32.NewProc("GetAsyncKeyState")
	procSetWindowPos           = user32.NewProc("SetWindowPos")
	procGetWindowLongPtrW      = user32.NewProc("GetWindowLongPtrW")
	procSetWindowLongPtrW      = user32.NewProc("SetWindowLongPtrW")
	procGetClientRect          = user32.NewProc("GetClientRect")
	procGetWindowRect          = user32.NewProc("GetWindowRect")
	procGetSystemMetrics       = user32.NewProc("GetSystemMetrics")
	procShowWindow             = user32.NewProc("ShowWindow")
	procSetForegroundWindow    = user32.NewProc("SetForegroundWindow")
	procFindWindowW            = user32.NewProc("FindWindowW")
	procFindWindowExW          = user32.NewProc("FindWindowExW")
	procCreatePopupMenu        = user32.NewProc("CreatePopupMenu")
	procAppendMenuW            = user32.NewProc("AppendMenuW")
	procDestroyMenu            = user32.NewProc("DestroyMenu")
	procGetCursorPos           = user32.NewProc("GetCursorPos")
	procTrackPopupMenu         = user32.NewProc("TrackPopupMenu")
	procLoadImageW             = user32.NewProc("LoadImageW")
	procLoadIconW              = user32.NewProc("LoadIconW")
	procDestroyWindow          = user32.NewProc("DestroyWindow")
	procSetDpiAwarenessContext = user32.NewProc("SetProcessDpiAwarenessContext")

	procShellNotifyIconW = shell32.NewProc("Shell_NotifyIconW")
	procCreateMutexW     = kernel32.NewProc("CreateMutexW")

	hwndMain      uintptr
	wv            webview2.WebView
	prevWndProc   uintptr
	taskbarMsg    uintptr
	trayIcon      uintptr
	instanceMutex windows.Handle
	quitting      atomic.Bool
	isHeadless    bool
	baseURL       string

	stateMu     sync.Mutex
	stateTimer  *time.Timer
	saveEnabled atomic.Bool

	editorMu       sync.Mutex
	editorW        webview2.WebView
	editorHwnd     uintptr
	editorProcPrev uintptr
	editorSpawning bool
)

type point struct {
	X int32
	Y int32
}

type winRect struct {
	Left   int32
	Top    int32
	Right  int32
	Bottom int32
}

type notifyIconData struct {
	CbSize           uint32
	Wnd              windows.HWND
	UID              uint32
	UFlags           uint32
	UCallbackMessage uint32
	HIcon            windows.Handle
	SzTip            [128]uint16
	DwState          uint32
	DwStateMask      uint32
	SzInfo           [256]uint16
	UVersion         uint32
	SzInfoTitle      [64]uint16
	DwInfoFlags      uint32
	GuidItem         windows.GUID
	HBalloonIcon     windows.Handle
}

func main() {
	headless := flag.Bool("headless", false, "仅启动本地 HTTP 服务，不创建桌面窗口")
	port := flag.Int("port", defaultPort, "本地 HTTP 服务端口")
	dbFlag := flag.String("db", "", "数据库文件路径")
	flag.Parse()

	isHeadless = *headless
	setupLogging()

	log.Printf("启动 %s %s (headless=%v, pid=%d)", appTitle, Version, isHeadless, os.Getpid())

	if !isHeadless {
		if !claimSingleInstance(`Local\AgentTodoBoard`) {
			log.Printf("检测到已有实例在运行，正在激活其窗口")
			activateExistingWindow()
			return
		}
		procSetDpiAwarenessContext.Call(^uintptr(3)) // PER_MONITOR_AWARE_V2
	}

	dbPath := *dbFlag
	if dbPath == "" {
		exe, err := os.Executable()
		dir := "."
		if err == nil {
			dir = filepath.Dir(exe)
		}
		dbPath = filepath.Join(dir, sanitizeFileName(computerFQDN())+".db")
	}
	log.Printf("数据库: %s", dbPath)

	store, err := OpenStore(dbPath)
	if err != nil {
		fatal("打开数据库失败", err)
	}
	defer store.Close()

	srv := NewServer(store, dbPath)
	kernel := StartMonitorKernel(store)
	defer kernel.Stop()
	binds := loadBinds()
	failures, err := srv.Start(*port, binds)
	if err != nil {
		fatal("启动本地服务失败", err)
	}
	for addr, msg := range failures {
		log.Printf("监听 %s 失败: %s", addr, msg)
	}
	defer srv.Stop()
	log.Printf("本地服务已启动: %s（监听 %s，端口 %d）", srv.BaseURL(), strings.Join(srv.Listening(), "、"), srv.Port())
	if len(failures) > 0 {
		log.Printf("有 %d 个监听地址启动失败，已跳过", len(failures))
	}
	baseURL = srv.BaseURL()

	if isHeadless {
		fmt.Printf("READY pid=%d url=%s\n", os.Getpid(), srv.BaseURL())
		waitForSignal()
		log.Printf("服务已停止")
		return
	}

	runGUI(srv)
	log.Printf("已退出")
}

func setupLogging() {
	dir := filepath.Join(appDataDir(), "logs")
	path := filepath.Join(dir, "app.log")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.SetOutput(os.Stderr)
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		log.SetOutput(os.Stderr)
		return
	}
	if isHeadless {
		log.SetOutput(io.MultiWriter(os.Stderr, f))
	} else {
		log.SetOutput(f)
	}
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
}

func appDataDir() string {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		base = os.TempDir()
	}
	return filepath.Join(base, appName)
}

func runGUI(srv *Server) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	w := webview2.NewWithOptions(webview2.WebViewOptions{
		Debug:     false,
		AutoFocus: true,
		DataPath:  filepath.Join(appDataDir(), "WebView2"),
		WindowOptions: webview2.WindowOptions{
			Title:  appTitle,
			Width:  windowW,
			Height: windowH,
			Center: true,
		},
	})
	if w == nil {
		fatal("创建窗口失败", errors.New("请确认已安装 Microsoft Edge WebView2 Runtime"))
	}
	wv = w
	hwndMain = uintptr(w.Window())
	log.Printf("主窗口句柄: 0x%x", hwndMain)

	procShowWindow.Call(hwndMain, swHide)

	// 先挂钩 wndproc，再改样式：WM_NCCALCSIZE 由我们处理，客户端才能等于窗口
	if !setupWindowProc() {
		log.Printf("警告: 窗口消息挂钩失败，托盘菜单与关闭到托盘可能不可用")
	}
	applyFrameless(windowW, windowH)
	measureAndSetMinSize(windowW, windowH)
	stripMaximizeBox()
	targetW, targetH := restoreWindow()
	logClient("初始")
	addTrayIcon()
	watchChildSubclass()
	watchEdgeGrab()

	var shown atomic.Bool
	showWindowFn := func() {
		if !shown.CompareAndSwap(false, true) {
			return
		}
		procShowWindow.Call(hwndMain, swShow)
		forceWindowSize(targetW, targetH)
		procSetForegroundWindow.Call(hwndMain)
		saveEnabled.Store(true)
	}

	srv.OnReady = func() { w.Dispatch(showWindowFn) }
	srv.OnHide = func() {
		w.Dispatch(func() {
			if hwndMain != 0 && !quitting.Load() {
				procShowWindow.Call(hwndMain, swHide)
			}
		})
	}
	srv.OnDrag = func() {
		w.Dispatch(func() {
			if hwndMain != 0 {
				procReleaseCapture.Call()
				procSendMessageW.Call(hwndMain, wmNCLButtonDown, htCaption, 0)
			}
		})
	}
	srv.AutostartStatus = autostartEnabled
	srv.SetAutostart = setAutostart
	srv.OnOpenEditor = openEditorWindow
	srv.OnEditorReady = showEditorWindow
	srv.OnHideEditor = hideEditorWindow
	srv.OnDragEditor = dragEditorWindow
	srv.OnResize = resizeWindow
	srv.OnResizeEnd = endResize
	srv.OnClose = quitApp

	time.AfterFunc(3*time.Second, func() { w.Dispatch(showWindowFn) })

	w.Navigate(srv.BaseURL() + "/app")
	installChildSubclass(hwndMain)
	w.Run()

	log.Printf("消息循环结束")
	if wv != nil {
		wv = nil
	}
	removeTrayIcon()
}

// stripFrame 去掉标题栏（保留 WS_THICKFRAME，否则系统不会进入缩放），
// 非客户区由 WM_NCCALCSIZE 返回 0 吃掉，故 thickframe 不会显示出来。
func stripFrame(hwnd uintptr) {
	style, _, _ := procGetWindowLongPtrW.Call(hwnd, gwlStyle)
	style &^= wsCaption | wsSysMenu | wsMaximizeBox | wsMinimizeBox
	style |= wsThickFrame
	procSetWindowLongPtrW.Call(hwnd, gwlStyle, style)
}

func applyFramelessTo(hwnd uintptr, width, height uint) {
	stripFrame(hwnd)

	ex, _, _ := procGetWindowLongPtrW.Call(hwnd, gwlExStyle)
	ex |= wsExToolWindow
	ex &^= wsExAppWindow
	procSetWindowLongPtrW.Call(hwnd, gwlExStyle, ex)

	procSetWindowPos.Call(hwnd, 0, 0, 0,
		uintptr(width), uintptr(height),
		swpNoMove|swpNoZOrder|swpNoActivate|swpFrameChanged)
	procSendMessageW.Call(hwnd, wmSize, 0, 0)
}

func applyFrameless(width, height uint) { applyFramelessTo(hwndMain, width, height) }

// setMinClientSize 计算出非客户区尺寸，使客户端最小尺寸恰为 minW x minH。
func setMinClientSize(w webview2.WebView, hwnd uintptr, totalW, totalH, minW, minH int) {
	var r winRect
	procGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&r)))
	cw := int(r.Right - r.Left)
	ch := int(r.Bottom - r.Top)
	if cw <= 0 || ch <= 0 {
		cw, ch = totalW, totalH
	}
	w.SetSize(minW+(totalW-cw), minH+(totalH-ch), webview2.HintMin)
	// SetSize(HintMin) 会重新加上 WS_THICKFRAME/WS_MAXIMIZEBOX，这里再抹掉
	stripFrame(hwnd)
	procSetWindowPos.Call(hwnd, 0, 0, 0, 0, 0,
		swpNoMove|swpNoSize|swpNoZOrder|swpNoActivate|swpFrameChanged)
	log.Printf("客户区 %dx%d，最小尺寸设为 %dx%d", cw, ch, minW+(totalW-cw), minH+(totalH-ch))
}

func measureAndSetMinSize(width, height uint) {
	setMinClientSize(wv, hwndMain, int(width), int(height), minClientW, minClientH)
}

func stripMaximizeBoxTo(hwnd uintptr) {
	style, _, _ := procGetWindowLongPtrW.Call(hwnd, gwlStyle)
	procSetWindowLongPtrW.Call(hwnd, gwlStyle, style&^wsMaximizeBox)
}

func stripMaximizeBox() { stripMaximizeBoxTo(hwndMain) }

// forceWindowSize 显示后校正尺寸（样式变更可能让系统重排窗口），但不覆盖恢复出来的尺寸。
func forceWindowSize(w, h int) {
	stripMaximizeBox()
	var r winRect
	procGetWindowRect.Call(hwndMain, uintptr(unsafe.Pointer(&r)))
	curW, curH := int(r.Right-r.Left), int(r.Bottom-r.Top)
	if curW != w || curH != h {
		procSetWindowPos.Call(hwndMain, 0, 0, 0, uintptr(w), uintptr(h),
			swpNoMove|swpNoZOrder|swpNoActivate|swpFrameChanged)
		log.Printf("窗口尺寸校正为 %dx%d（原 %dx%d）", w, h, curW, curH)
	}
	procSendMessageW.Call(hwndMain, wmSize, 0, 0)
	logClient("已显示")
}

func logClient(tag string) {
	var r winRect
	procGetClientRect.Call(hwndMain, uintptr(unsafe.Pointer(&r)))
	log.Printf("%s：客户区 %dx%d", tag, r.Right-r.Left, r.Bottom-r.Top)
}

func setupWindowProc() bool {
	p, err := windows.UTF16PtrFromString("TaskbarCreated")
	if err == nil {
		taskbarMsg, _, _ = procRegisterWindowMessageW.Call(uintptr(unsafe.Pointer(p)))
		runtime.KeepAlive(p)
	}
	cb := windows.NewCallback(windowProc)
	if cb == 0 {
		return false
	}
	prev, _, _ := procSetWindowLongPtrW.Call(hwndMain, gwlpWndProc, cb)
	if prev == 0 {
		return false
	}
	prevWndProc = prev
	return true
}

func windowProc(hwnd, msg, wp, lp uintptr) uintptr {
	if taskbarMsg != 0 && msg == taskbarMsg {
		addTrayIcon()
		return 0
	}
	if r, handled := framelessMessage(hwnd, msg, wp, lp); handled {
		return r
	}
	switch msg {
	case wmMove, wmSize:
		scheduleSaveState()
	case wmExitSizeMove:
		saveWindowState()
	}
	switch msg {
	case wmClose:
		// 关闭即退出（保存状态 → 清托盘图标 → 销毁窗口）
		quitApp()
		return 0
	case wmCommand:
		handleMenuCommand(int32(wp & 0xFFFF))
		return 0
	case wmTray:
		switch uint32(lp & 0xFFFF) {
		case wmLButtonUp, wmLButtonDblClk:
			showMainWindow()
		case wmRButtonUp, wmContextMenu:
			showTrayMenu()
		}
		return 0
	}
	if prevWndProc != 0 {
		r, _, _ := procCallWindowProcW.Call(prevWndProc, hwnd, msg, wp, lp)
		return r
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, msg, wp, lp)
	return r
}

func showMainWindow() {
	if hwndMain == 0 {
		return
	}
	procShowWindow.Call(hwndMain, swRestore)
	procShowWindow.Call(hwndMain, swShow)
	procSetForegroundWindow.Call(hwndMain)
}

func showTrayMenu() {
	var pt point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))

	hmenu, _, _ := procCreatePopupMenu.Call()
	if hmenu == 0 {
		return
	}
	defer procDestroyMenu.Call(hmenu)

	appendMenu(hmenu, mfString, menuOpen, "🖥️ 打开主界面")
	appendMenu(hmenu, mfSeparator, 0, "")
	flags := uintptr(mfString)
	label := "⚡ 开机自启动"
	if autostartEnabled() {
		flags |= mfChecked
		label += "\t已启用"
	}
	appendMenu(hmenu, flags, menuAutostart, label)
	appendMenu(hmenu, mfSeparator, 0, "")
	appendMenu(hmenu, mfString, menuQuit, "🚪 退出程序")

	procSetForegroundWindow.Call(hwndMain)
	r, _, _ := procTrackPopupMenu.Call(hmenu, tpmRightButton|tpmBottomAlign|tpmReturnCmd,
		uintptr(pt.X), uintptr(pt.Y), 0, hwndMain, 0)
	procPostMessageW.Call(hwndMain, wmNull, 0, 0)

	runtime.KeepAlive(pt)
	handleMenuCommand(int32(r))
}

func appendMenu(hmenu, flags, id uintptr, text string) {
	if text == "" {
		procAppendMenuW.Call(hmenu, mfSeparator, 0, 0)
		return
	}
	p, err := windows.UTF16PtrFromString(text)
	if err != nil {
		return
	}
	procAppendMenuW.Call(hmenu, flags, id, uintptr(unsafe.Pointer(p)))
	runtime.KeepAlive(p)
}

func handleMenuCommand(id int32) {
	switch id {
	case menuOpen:
		showMainWindow()
	case menuAutostart:
		on := !autostartEnabled()
		if err := setAutostart(on); err != nil {
			log.Printf("设置开机自启动失败: %v", err)
			return
		}
		log.Printf("开机自启动已%s", map[bool]string{true: "启用", false: "关闭"}[on])
	case menuQuit:
		quitApp()
	}
}

func quitApp() {
	if !quitting.CompareAndSwap(false, true) {
		return
	}
	log.Printf("正在退出")
	saveWindowState()
	removeTrayIcon()
	editorMu.Lock()
	ew, eh := editorW, editorHwnd
	editorMu.Unlock()
	if ew != nil && eh != 0 {
		ew.Dispatch(func() { procDestroyWindow.Call(eh) })
	}
	// DestroyWindow 必须由窗口所属线程执行
	if wv != nil {
		wv.Dispatch(func() {
			if hwndMain != 0 {
				procDestroyWindow.Call(hwndMain)
			}
		})
	}
}

func addTrayIcon() {
	if hwndMain == 0 {
		return
	}
	if trayIcon == 0 {
		trayIcon = loadTrayIcon()
	}
	nid := notifyIconData{
		CbSize:           uint32(unsafe.Sizeof(notifyIconData{})),
		Wnd:              windows.HWND(hwndMain),
		UID:              trayUID,
		UFlags:           nifMessage | nifIcon | nifTip,
		UCallbackMessage: wmTray,
		HIcon:            windows.Handle(trayIcon),
	}
	copy(nid.SzTip[:], windows.StringToUTF16(trayTip))
	r, _, _ := procShellNotifyIconW.Call(nimAdd, uintptr(unsafe.Pointer(&nid)))
	if r == 0 {
		r2, _, _ := procShellNotifyIconW.Call(nimModify, uintptr(unsafe.Pointer(&nid)))
		if r2 == 0 {
			log.Printf("警告: 托盘图标注册失败")
		}
	}
	runtime.KeepAlive(nid)
}

func removeTrayIcon() {
	if hwndMain == 0 {
		return
	}
	nid := notifyIconData{
		CbSize: uint32(unsafe.Sizeof(notifyIconData{})),
		Wnd:    windows.HWND(hwndMain),
		UID:    trayUID,
	}
	procShellNotifyIconW.Call(nimDelete, uintptr(unsafe.Pointer(&nid)))
	runtime.KeepAlive(nid)
}

func loadTrayIcon() uintptr {
	data, err := fs.ReadFile(frontendFS, "frontend/icon.ico")
	if err == nil {
		path := filepath.Join(os.TempDir(), fmt.Sprintf("agent-todo-board-%d.ico", os.Getpid()))
		if err := os.WriteFile(path, data, 0o600); err == nil {
			p, perr := windows.UTF16PtrFromString(path)
			if perr == nil {
				r, _, _ := procLoadImageW.Call(0, uintptr(unsafe.Pointer(p)), imageIcon,
					0, 0, lrLoadFromFile|lrDefaultSize)
				runtime.KeepAlive(p)
				os.Remove(path)
				if r != 0 {
					return r
				}
			}
		}
	}
	r, _, _ := procLoadIconW.Call(0, idiApplication)
	return r
}

func claimSingleInstance(name string) bool {
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return true
	}
	r, _, e := procCreateMutexW.Call(0, 0, uintptr(unsafe.Pointer(p)))
	runtime.KeepAlive(p)
	if r == 0 {
		log.Printf("创建互斥量失败: %v", e)
		return true
	}
	instanceMutex = windows.Handle(r)
	return e != windows.ERROR_ALREADY_EXISTS
}

func activateExistingWindow() {
	p, err := windows.UTF16PtrFromString(appTitle)
	if err != nil {
		return
	}
	hwnd, _, _ := procFindWindowW.Call(0, uintptr(unsafe.Pointer(p)))
	runtime.KeepAlive(p)
	if hwnd == 0 {
		return
	}
	procShowWindow.Call(hwnd, swRestore)
	procShowWindow.Call(hwnd, swShow)
	procSetForegroundWindow.Call(hwnd)
}

func waitForSignal() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	sig := <-ch
	log.Printf("收到信号 %v，准备退出", sig)
}

func fatal(msg string, err error) {
	text := msg
	if err != nil {
		text = msg + ": " + err.Error()
	}
	log.Printf("致命错误: %s", text)
	if !isHeadless {
		windows.MessageBox(0, mustUTF16Ptr(text), mustUTF16Ptr(appTitle), windows.MB_OK|windows.MB_ICONERROR)
	}
	os.Exit(1)
}

func mustUTF16Ptr(s string) *uint16 {
	p, err := windows.UTF16PtrFromString(s)
	if err != nil {
		p, _ = windows.UTF16PtrFromString("")
	}
	return p
}

const runKeyPath = `Software\Microsoft\Windows\CurrentVersion\Run`
const runValueName = "AgentTodoBoard"

func autostartEnabled() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	v, _, err := k.GetStringValue(runValueName)
	if err != nil {
		return false
	}
	return strings.TrimSpace(v) != ""
}

func setAutostart(enabled bool) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE|registry.QUERY_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if enabled {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		return k.SetStringValue(runValueName, `"`+exe+`"`)
	}
	if err := k.DeleteValue(runValueName); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return err
	}
	return nil
}

func computerFQDN() string {
	// 3 = ComputerNameDnsFullyQualified, 1 = ComputerNameDnsHostname
	for _, kind := range []uint32{3, 1} {
		size := uint32(256)
		buf := make([]uint16, size)
		if err := windows.GetComputerNameEx(kind, &buf[0], &size); err == nil && size > 0 {
			if name := windows.UTF16ToString(buf[:size]); name != "" {
				return name
			}
		}
	}
	if h, err := os.Hostname(); err == nil {
		return h
	}
	return ""
}

func sanitizeFileName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < 32 || strings.ContainsRune(`\/:*?"<>|`, r) {
			b.WriteRune('-')
			continue
		}
		b.WriteRune(r)
	}
	out := strings.Trim(b.String(), " .-")
	if out == "" {
		out = "todo"
	}
	return out
}

/* ---------------- 编辑窗口（第二个 WebView2，独立线程与消息循环） ---------------- */

func openEditorWindow(sectionID, taskID int64) {
	if baseURL == "" {
		return
	}
	url := fmt.Sprintf("%s/editor.html?sid=%d&tid=%d&t=%d", baseURL, sectionID, taskID, time.Now().UnixNano())

	editorMu.Lock()
	existing := editorW
	hwnd := editorHwnd
	spawning := editorSpawning
	if existing == nil && !spawning {
		editorSpawning = true
	}
	editorMu.Unlock()

	if existing != nil {
		existing.Dispatch(func() {
			if hwnd != 0 {
				procShowWindow.Call(hwnd, swHide)
			}
			existing.Navigate(url)
		})
		return
	}
	if spawning {
		return
	}
	go runEditorWindow(url)
}

func runEditorWindow(url string) {
	runtime.LockOSThread()

	w := webview2.NewWithOptions(webview2.WebViewOptions{
		Debug:     false,
		AutoFocus: true,
		DataPath:  filepath.Join(appDataDir(), "WebView2Editor"),
		WindowOptions: webview2.WindowOptions{
			Title:  "编辑任务",
			Width:  editorW0,
			Height: editorH0,
			Center: true,
		},
	})
	if w == nil {
		editorMu.Lock()
		editorSpawning = false
		editorMu.Unlock()
		log.Printf("创建编辑窗口失败：请确认已安装 Microsoft Edge WebView2 Runtime")
		return
	}

	hwnd := uintptr(w.Window())
	log.Printf("编辑窗口句柄: 0x%x", hwnd)
	procShowWindow.Call(hwnd, swHide)
	setupEditorProc(hwnd)
	applyFramelessTo(hwnd, editorW0, editorH0)
	stripMaximizeBoxTo(hwnd)
	setMinClientSize(w, hwnd, editorW0, editorH0, editorMinW, editorMinH)

	editorMu.Lock()
	editorW = w
	editorHwnd = hwnd
	editorSpawning = false
	editorMu.Unlock()

	w.Navigate(url)
	installChildSubclass(hwnd)
	w.Run()

	editorMu.Lock()
	editorW = nil
	editorHwnd = 0
	editorMu.Unlock()
	log.Printf("编辑窗口已结束")
}

// framelessMessage 处理无边框窗口的非客户区计算与拖拽热区。
func framelessMessage(hwnd, msg, wp, lp uintptr) (uintptr, bool) {
	switch msg {
	case wmNCCalcSize:
		if wp != 0 {
			return 0, true
		}
	case wmNCHitTest:
		return hitTestBorder(hwnd, lp), true
	}
	return 0, false
}

func hitTestBorder(hwnd, lp uintptr) uintptr {
	x := int32(int16(lp & 0xFFFF))
	y := int32(int16((lp >> 16) & 0xFFFF))
	var r winRect
	procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&r)))

	const border = 6
	left := x < r.Left+border
	right := x >= r.Right-border
	top := y < r.Top+border
	bottom := y >= r.Bottom-border

	switch {
	case top && left:
		return htTopLeft
	case top && right:
		return htTopRight
	case bottom && left:
		return htBottomLeft
	case bottom && right:
		return htBottomRight
	case left:
		return htLeft
	case right:
		return htRight
	case top:
		return htTop
	case bottom:
		return htBottom
	}
	return htClient
}

func setupEditorProc(hwnd uintptr) {
	cb := windows.NewCallback(editorProc)
	if cb == 0 {
		return
	}
	prev, _, _ := procSetWindowLongPtrW.Call(hwnd, gwlpWndProc, cb)
	if prev != 0 {
		editorProcPrev = prev
	}
}

func editorProc(hwnd, msg, wp, lp uintptr) uintptr {
	if r, handled := framelessMessage(hwnd, msg, wp, lp); handled {
		return r
	}
	if msg == wmClose {
		procShowWindow.Call(hwnd, swHide)
		notifyMainRefresh()
		return 0
	}
	if editorProcPrev != 0 {
		r, _, _ := procCallWindowProcW.Call(editorProcPrev, hwnd, msg, wp, lp)
		return r
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, msg, wp, lp)
	return r
}

func showEditorWindow(title string) {
	editorMu.Lock()
	w, hwnd := editorW, editorHwnd
	editorMu.Unlock()
	if w == nil || hwnd == 0 {
		return
	}
	w.Dispatch(func() {
		if title != "" {
			w.SetTitle(title)
		}
		procShowWindow.Call(hwnd, swShow)
		procSetForegroundWindow.Call(hwnd)
	})
}

func hideEditorWindow() {
	editorMu.Lock()
	w, hwnd := editorW, editorHwnd
	editorMu.Unlock()
	if w == nil || hwnd == 0 {
		return
	}
	w.Dispatch(func() { procShowWindow.Call(hwnd, swHide) })
	// 编辑窗口关闭后立刻通知主窗口刷新（不等轮询）
	notifyMainRefresh()
}

// notifyMainRefresh 让主窗口立即刷新（JS 侧 100ms 防抖）。
func notifyMainRefresh() {
	if wv == nil {
		return
	}
	wv.Dispatch(func() {
		wv.Eval("window.__todoRefreshNow && window.__todoRefreshNow()")
	})
}

func dragEditorWindow() {
	editorMu.Lock()
	w, hwnd := editorW, editorHwnd
	editorMu.Unlock()
	if w == nil || hwnd == 0 {
		return
	}
	startWindowDrag(w, hwnd)
}

func startWindowDrag(w webview2.WebView, hwnd uintptr) {
	w.Dispatch(func() {
		procReleaseCapture.Call()
		procSendMessageW.Call(hwnd, wmNCLButtonDown, htCaption, 0)
	})
}

// resizeWindow 自绘缩放：WebView2 铺满客户区且系统缩放循环在本窗口不生效，
// 故按边缘方向跟踪鼠标位置，直接 SetWindowPos（JS 在 pointerdown/pointerup 调用起止）。
func resizeWindow(edge string, editor bool) {
	if _, ok := htByEdge[edge]; !ok {
		return
	}
	hwnd, w := hwndMain, wv
	minW, minH := minClientW, minClientH
	if editor {
		editorMu.Lock()
		hwnd = editorHwnd
		w = editorW
		editorMu.Unlock()
		minW, minH = editorMinW, editorMinH
	}
	if hwnd == 0 || w == nil {
		return
	}
	w.Dispatch(func() { startResize(w, hwnd, edge, minW, minH) })
}

var (
	resizeMu   sync.Mutex
	resizeStop chan struct{}
)

func endResize() {
	resizeMu.Lock()
	if resizeStop != nil {
		close(resizeStop)
		resizeStop = nil
	}
	resizeMu.Unlock()
}

var (
	resizePending atomic.Bool
	resizeTarget  [4]int // x, y, w, h
)

func startResize(w webview2.WebView, hwnd uintptr, edge string, minW, minH int) {
	if resizeBusy() {
		return
	}
	stop := make(chan struct{})
	resizeMu.Lock()
	resizeStop = stop
	resizeMu.Unlock()

	var start point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&start)))
	var wr winRect
	procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&wr)))
	x0, y0 := int(wr.Left), int(wr.Top)
	w0, h0 := int(wr.Right-wr.Left), int(wr.Bottom-wr.Top)
	left := strings.Contains(edge, "left")
	right := strings.Contains(edge, "right")
	top := strings.Contains(edge, "top")
	bottom := strings.Contains(edge, "bottom")

	resizeTarget = [4]int{x0, y0, w0, h0}
	log.Printf("开始缩放 edge=%s hwnd=0x%x %d,%d %dx%d", edge, hwnd, x0, y0, w0, h0)

	// 冻结重绘（只在 UI 线程做一次）
	w.Dispatch(func() {
		procSetCapture.Call(hwnd)
	})

	go func() {
		ticker := time.NewTicker(33 * time.Millisecond)
		defer ticker.Stop()
		lastMove := time.Now()
		seenDown := false
		reason := "pointerup"
		maxFrame := time.Duration(0)
		for {
			select {
			case <-stop:
				goto done
			case <-ticker.C:
			}
			if leftButtonDown() {
				seenDown = true
			} else if seenDown {
				reason = "左键已松开"
				break
			}
			if time.Since(lastMove) > 3*time.Second {
				reason = "3 秒无位移"
				break
			}

			var p point
			procGetCursorPos.Call(uintptr(unsafe.Pointer(&p)))
			dx := int(p.X) - int(start.X)
			dy := int(p.Y) - int(start.Y)

			x, y, cw, ch := x0, y0, w0, h0
			if left {
				cw = w0 - dx
				if cw < minW {
					cw = minW
				}
				x = x0 + w0 - cw
			} else if right {
				cw = w0 + dx
				if cw < minW {
					cw = minW
				}
			}
			if top {
				ch = h0 - dy
				if ch < minH {
					ch = minH
				}
				y = y0 + h0 - ch
			} else if bottom {
				ch = h0 + dy
				if ch < minH {
					ch = minH
				}
			}

			if resizeTarget == [4]int{x, y, cw, ch} {
				continue
			}
			resizeTarget = [4]int{x, y, cw, ch}
			lastMove = time.Now()
			t0 := time.Now()
			// 合并帧：上一帧还没被 UI 线程取走就跳过，避免消息队列堆积
			if !resizePending.Swap(true) {
				w.Dispatch(func() { applyResize(hwnd) })
			}
			if d := time.Since(t0); d > maxFrame {
				maxFrame = d
			}
		}
	done:
		target := resizeTarget
		log.Printf("缩放结束 edge=%s reason=%s rect=%d,%d %dx%d 最大派发耗时=%v", edge, reason, target[0], target[1], target[2], target[3], maxFrame)
		w.Dispatch(func() {
			resizePending.Store(false)
			procReleaseCapture.Call()
			procSetWindowPos.Call(hwnd, 0,
				uintptr(int64(int32(target[0]))), uintptr(int64(int32(target[1]))),
				uintptr(target[2]), uintptr(target[3]),
				swpNoZOrder|swpNoActivate)
		})
		saveWindowState()
		resizeMu.Lock()
		resizeStop = nil
		resizeMu.Unlock()
	}()
}

// applyResize 在 UI 线程按最新目标尺寸移动窗口（不重绘，避免每帧整窗合成）。
func applyResize(hwnd uintptr) {
	t := resizeTarget
	procSetWindowPos.Call(hwnd, 0,
		uintptr(int64(int32(t[0]))), uintptr(int64(int32(t[1]))),
		uintptr(t[2]), uintptr(t[3]),
		swpNoZOrder|swpNoActivate)
	resizePending.Store(false)
}

var htByEdge = map[string]uintptr{
	"left":        htLeft,
	"right":       htRight,
	"top":         htTop,
	"bottom":      htBottom,
	"topleft":     htTopLeft,
	"topright":    htTopRight,
	"bottomleft":  htBottomLeft,
	"bottomright": htBottomRight,
}

/* ---------------- 窗口状态记忆 ---------------- */

const (
	smXVirtualScreen  = 76
	smYVirtualScreen  = 77
	smCXVirtualScreen = 78
	smCYVirtualScreen = 79

	stateFileName = "window.json"
)

type windowState struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

func stateFilePath() string {
	return filepath.Join(appDataDir(), stateFileName)
}

// loadWindowState 读取已保存的窗口位置尺寸；文件损坏时备份为 .bad，不用默认值覆盖。
func loadWindowState() (windowState, bool) {
	path := stateFilePath()
	raw, err := os.ReadFile(path)
	if err != nil {
		return windowState{}, false
	}
	var st windowState
	if err := json.Unmarshal(raw, &st); err != nil {
		log.Printf("窗口状态文件无法解析（已备份为 %s.bad）: %v", path, err)
		if renameErr := os.Rename(path, path+".bad"); renameErr != nil {
			log.Printf("备份窗口状态失败: %v", renameErr)
		}
		return windowState{}, false
	}
	if st.W <= 0 || st.H <= 0 {
		return windowState{}, false
	}
	return st, true
}

func saveWindowState() {
	if hwndMain == 0 {
		return
	}
	var r winRect
	procGetWindowRect.Call(hwndMain, uintptr(unsafe.Pointer(&r)))
	w := int(r.Right - r.Left)
	h := int(r.Bottom - r.Top)
	if w <= 0 || h <= 0 {
		return
	}
	st := windowState{X: int(r.Left), Y: int(r.Top), W: w, H: h}
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return
	}
	path := stateFilePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		log.Printf("保存窗口状态失败: %v", err)
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		log.Printf("保存窗口状态失败: %v", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		log.Printf("保存窗口状态失败: %v", err)
	}
}

func scheduleSaveState() {
	if !saveEnabled.Load() {
		return
	}
	stateMu.Lock()
	if stateTimer != nil {
		stateTimer.Stop()
	}
	stateTimer = time.AfterFunc(600*time.Millisecond, saveWindowState)
	stateMu.Unlock()
}

func virtualScreen() (x, y, w, h int) {
	vx, _, _ := procGetSystemMetrics.Call(smXVirtualScreen)
	vy, _, _ := procGetSystemMetrics.Call(smYVirtualScreen)
	vw, _, _ := procGetSystemMetrics.Call(smCXVirtualScreen)
	vh, _, _ := procGetSystemMetrics.Call(smCYVirtualScreen)
	return int(int32(vx)), int(int32(vy)), int(int32(vw)), int(int32(vh))
}

func rectVisible(x, y, w, h int) bool {
	vx, vy, vw, vh := virtualScreen()
	left := max(x, vx)
	top := max(y, vy)
	right := min(x+w, vx+vw)
	bottom := min(y+h, vy+vh)
	return right-left >= 80 && bottom-top >= 60
}

// restoreWindow 恢复上次的位置与尺寸，返回本次目标尺寸。
func restoreWindow() (int, int) {
	st, ok := loadWindowState()
	if !ok {
		return windowW, windowH
	}
	w := max(st.W, minClientW)
	h := max(st.H, minClientH)
	x, y := st.X, st.Y

	if !rectVisible(x, y, w, h) {
		log.Printf("保存的窗口位置 (%d,%d) 不在任何显示器可见范围内，改用默认位置", x, y)
		return w, h
	}

	vx, vy, vw, vh := virtualScreen()
	w = min(w, vw)
	h = min(h, vh)
	x = min(max(x, vx), vx+vw-w)
	y = min(max(y, vy), vy+vh-h)

	procSetWindowPos.Call(hwndMain, 0,
		uintptr(int64(int32(x))), uintptr(int64(int32(y))),
		uintptr(w), uintptr(h),
		swpNoZOrder|swpNoActivate|swpFrameChanged)
	log.Printf("恢复窗口位置 (%d,%d) 尺寸 %dx%d", x, y, w, h)
	return w, h
}

/* ---------------- WebView2 子窗口子类化（让边缘命中测试落到父窗口） ---------------- */

type childSubclass struct {
	prev   uintptr
	parent uintptr
}

var (
	subclassMu sync.Mutex
	subclassed = map[uintptr]childSubclass{}
)

// installChildSubclass 递归挂在 parent 的所有子窗口上：边缘区域返回 HTTRANSPARENT，
// 使命中测试继续落到父窗口；否则鼠标落在 WebView2 上，父窗口收不到 WM_NCHITTEST，系统不会缩放。
func installChildSubclass(parent uintptr) {
	if parent == 0 {
		return
	}
	var walk func(uintptr)
	walk = func(h uintptr) {
		child, _, _ := procFindWindowExW.Call(h, 0, 0, 0)
		for child != 0 {
			ensureChildSubclass(uintptr(child), parent)
			walk(uintptr(child))
			child, _, _ = procFindWindowExW.Call(h, child, 0, 0)
		}
	}
	walk(parent)
}

func ensureChildSubclass(child, parent uintptr) {
	subclassMu.Lock()
	_, known := subclassed[child]
	subclassMu.Unlock()
	if known {
		return
	}
	cb := windows.NewCallback(childProc)
	if cb == 0 {
		return
	}
	prev, _, _ := procSetWindowLongPtrW.Call(child, gwlpWndProc, cb)
	if prev == 0 {
		return
	}
	subclassMu.Lock()
	subclassed[child] = childSubclass{prev: prev, parent: parent}
	subclassMu.Unlock()
}

func childProc(hwnd, msg, wp, lp uintptr) uintptr {
	subclassMu.Lock()
	entry := subclassed[hwnd]
	subclassMu.Unlock()

	if msg == wmNCHitTest && entry.parent != 0 {
		if hitTestBorder(entry.parent, lp) != htClient {
			return htTransparent
		}
		return htClient
	}
	if entry.prev != 0 {
		r, _, _ := procCallWindowProcW.Call(entry.prev, hwnd, msg, wp, lp)
		return r
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, msg, wp, lp)
	return r
}

// watchChildSubclass 周期性补挂（WebView2 会在导航时重建子窗口）。
func watchChildSubclass() {
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			if quitting.Load() {
				return
			}
			installChildSubclass(hwndMain)
			editorMu.Lock()
			eh := editorHwnd
			editorMu.Unlock()
			installChildSubclass(eh)
		}
	}()
}

/* ---------------- 边缘抓取（不依赖页面事件） ----------------
   页面/子窗口可能吃掉鼠标消息，这里直接轮询物理左键与光标位置：
   在窗口四边/四角按下即进入缩放，避免"点了没反应"。 */

func leftButtonDown() bool {
	r, _, _ := procGetAsyncKeyState.Call(vkLeftButton)
	return int16(r) < 0
}

func watchEdgeGrab() {
	go func() {
		wasDown := false
		for !quitting.Load() {
			time.Sleep(15 * time.Millisecond)
			down := leftButtonDown()
			if down && !wasDown && !resizeBusy() {
				tryEdgeGrab()
			}
			wasDown = down
		}
	}()
}

func resizeBusy() bool {
	resizeMu.Lock()
	busy := resizeStop != nil
	resizeMu.Unlock()
	return busy
}

func tryEdgeGrab() {
	type target struct {
		hwnd uintptr
		w    webview2.WebView
		minW int
		minH int
	}
	targets := []target{{hwndMain, wv, minClientW, minClientH}}
	editorMu.Lock()
	if editorW != nil && editorHwnd != 0 {
		targets = append(targets, target{editorHwnd, editorW, editorMinW, editorMinH})
	}
	editorMu.Unlock()

	var pt point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	cx, cy := int(pt.X), int(pt.Y)

	const zone = 6
	for _, t := range targets {
		if t.hwnd == 0 || t.w == nil {
			continue
		}
		var wr winRect
		procGetWindowRect.Call(t.hwnd, uintptr(unsafe.Pointer(&wr)))
		l, tp, r, b := int(wr.Left), int(wr.Top), int(wr.Right), int(wr.Bottom)
		if cx < l-zone || cx > r+zone || cy < tp-zone || cy > b+zone {
			continue
		}
		left := cx < l+zone
		right := cx > r-zone
		top := cy < tp+zone
		bottom := cy > b-zone
		var edge string
		switch {
		case top && left:
			edge = "topleft"
		case top && right:
			edge = "topright"
		case bottom && left:
			edge = "bottomleft"
		case bottom && right:
			edge = "bottomright"
		case left:
			edge = "left"
		case right:
			edge = "right"
		case top:
			edge = "top"
		case bottom:
			edge = "bottom"
		default:
			continue
		}
		hw, wb, mw, mh := t.hwnd, t.w, t.minW, t.minH
		log.Printf("边缘抓取 edge=%s hwnd=0x%x cursor=%d,%d", edge, hw, cx, cy)
		wb.Dispatch(func() { startResize(wb, hw, edge, mw, mh) })
		return
	}
}
