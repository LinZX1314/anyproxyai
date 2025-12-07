//go:build windows
// +build windows

package system

import (
	"context"
	_ "embed"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/energye/systray"
	log "github.com/sirupsen/logrus"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

//go:embed icon.ico
var trayIconData []byte

var (
	user32           = syscall.NewLazyDLL("user32.dll")
	procPeekMessageW = user32.NewProc("PeekMessageW")
)

const (
	PM_NOREMOVE = 0x0000
)

type MSG struct {
	HWnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      struct{ X, Y int32 }
}

// pumpMessages 手动泵送 Windows 消息，保持托盘响应
func pumpMessages() {
	var msg MSG
	// PeekMessage 不会阻塞，只是检查消息队列
	procPeekMessageW.Call(uintptr(unsafe.Pointer(&msg)), 0, 0, 0, PM_NOREMOVE)
}

// SystemTray 系统托盘管理器
type SystemTray struct {
	ctx          context.Context
	mShow        *systray.MenuItem
	mQuit        *systray.MenuItem
	isRunning    int32 // 使用 atomic
	isReady      int32 // 托盘是否已就绪
	mu           sync.RWMutex
	quitCallback func()
	stopCh       chan struct{}
	actionCh     chan func() // 用于在托盘线程执行操作
}

// NewSystemTray 创建系统托盘管理器
func NewSystemTray(ctx context.Context) *SystemTray {
	return &SystemTray{
		ctx:      ctx,
		stopCh:   make(chan struct{}),
		actionCh: make(chan func(), 10),
	}
}

// SetQuitCallback 设置退出回调
func (s *SystemTray) SetQuitCallback(callback func()) {
	s.quitCallback = callback
}

// ShowWindow 显示窗口
func (s *SystemTray) ShowWindow() {
	log.Info("System tray: Showing window")

	// 使用 goroutine 避免阻塞托盘消息循环
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Errorf("ShowWindow panic: %v", r)
			}
		}()

		runtime.WindowShow(s.ctx)
		runtime.WindowUnminimise(s.ctx)

		// 将窗口置于前台
		runtime.WindowSetAlwaysOnTop(s.ctx, true)
		time.Sleep(100 * time.Millisecond)
		runtime.WindowSetAlwaysOnTop(s.ctx, false)
	}()
}

// HideWindow 隐藏窗口
func (s *SystemTray) HideWindow() {
	log.Info("System tray: Hiding window")
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Errorf("HideWindow panic: %v", r)
			}
		}()
		runtime.WindowHide(s.ctx)
	}()
}

// QuitApp 退出应用
func (s *SystemTray) QuitApp() {
	log.Info("System tray: Quitting application")

	if !atomic.CompareAndSwapInt32(&s.isRunning, 1, 0) {
		return
	}

	// 安全关闭 stop channel
	s.mu.Lock()
	select {
	case <-s.stopCh:
		// already closed
	default:
		close(s.stopCh)
	}
	s.mu.Unlock()

	// 先退出托盘
	systray.Quit()

	// 调用退出回调或直接退出
	if s.quitCallback != nil {
		s.quitCallback()
	} else {
		os.Exit(0)
	}
}

// Setup 设置系统托盘
func (s *SystemTray) Setup() error {
	if !atomic.CompareAndSwapInt32(&s.isRunning, 0, 1) {
		return nil
	}

	// 在后台启动系统托盘
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Errorf("System tray setup panic: %v", r)
				atomic.StoreInt32(&s.isRunning, 0)
			}
		}()

		systray.Run(s.onReady, s.onExit)
	}()

	log.Info("System tray setup completed")
	return nil
}

// onReady 托盘就绪回调
func (s *SystemTray) onReady() {
	// 设置托盘图标
	if len(trayIconData) > 0 {
		systray.SetIcon(trayIconData)
	}
	systray.SetTitle("AnyProxyAi")
	systray.SetTooltip("AnyProxyAi - Click to open")

	// 设置左键单击直接打开窗口
	systray.SetOnClick(func(menu systray.IMenu) {
		s.ShowWindow()
	})

	// 设置左键双击也打开窗口
	systray.SetOnDClick(func(menu systray.IMenu) {
		s.ShowWindow()
	})

	// 设置右键点击显示菜单 - 确保右键菜单能正常弹出
	systray.SetOnRClick(func(menu systray.IMenu) {
		menu.ShowMenu()
	})

	// 添加菜单项 (使用英文以确保兼容性)
	s.mShow = systray.AddMenuItem("Open", "Open main window")
	s.mShow.Click(func() {
		s.ShowWindow()
	})

	systray.AddSeparator()

	s.mQuit = systray.AddMenuItem("Exit", "Exit AnyProxyAi")
	s.mQuit.Click(func() {
		log.Info("Quit menu clicked")
		s.QuitApp()
	})

	// 标记托盘已就绪
	atomic.StoreInt32(&s.isReady, 1)

	// 启动消息泵保持托盘活跃
	go s.messagePump()

	// 定期刷新保持托盘活跃
	go s.keepAlive()
}

// messagePump 消息泵 - 定期泵送 Windows 消息保持托盘响应
func (s *SystemTray) messagePump() {
	// 使用较短间隔泵送消息
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			if atomic.LoadInt32(&s.isRunning) == 0 {
				return
			}
			// 泵送消息保持响应
			pumpMessages()
		case action := <-s.actionCh:
			// 执行排队的操作
			if action != nil {
				action()
			}
		}
	}
}

// keepAlive 保持托盘活跃 - 修复 Windows 托盘无响应问题
func (s *SystemTray) keepAlive() {
	// 使用更短的间隔来保持托盘响应
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	refreshCount := 0

	for {
		select {
		case <-s.stopCh:
			return
		case <-ticker.C:
			if atomic.LoadInt32(&s.isRunning) == 0 {
				return
			}

			refreshCount++

			// 每次都刷新 tooltip 以保持消息泵活跃
			if atomic.LoadInt32(&s.isReady) == 1 {
				systray.SetTooltip("AnyProxyAi - API Proxy Manager")
			}

			// 每 20 秒刷新一次图标
			if refreshCount%10 == 0 && len(trayIconData) > 0 {
				if atomic.LoadInt32(&s.isReady) == 1 {
					systray.SetIcon(trayIconData)
				}
			}
		}
	}
}

// onExit 托盘退出回调
func (s *SystemTray) onExit() {
	atomic.StoreInt32(&s.isRunning, 0)
	atomic.StoreInt32(&s.isReady, 0)
	log.Info("System tray exited")
}

// Quit 退出托盘
func (s *SystemTray) Quit() {
	if atomic.CompareAndSwapInt32(&s.isRunning, 1, 0) {
		s.mu.Lock()
		select {
		case <-s.stopCh:
			// already closed
		default:
			close(s.stopCh)
		}
		s.mu.Unlock()
		systray.Quit()
	}
}
