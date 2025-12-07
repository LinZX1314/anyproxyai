package main

import (
	"context"
	"embed"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"time"

	"openai-router-go/internal/config"
	"openai-router-go/internal/database"
	"openai-router-go/internal/router"
	"openai-router-go/internal/service"
	"openai-router-go/internal/system"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

//go:embed frontend/dist
var assets embed.FS

// 全局日志文件句柄
var logFile *os.File

// setupFileLogging 设置文件日志
func setupFileLogging() (*os.File, error) {
	// 创建 log 目录
	logDir := "log"
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create log directory: %v", err)
	}

	// 使用当前日期作为日志文件名
	today := time.Now().Format("2006-01-02")
	logPath := filepath.Join(logDir, today+".log")

	// 打开或创建日志文件（追加模式）
	file, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, fmt.Errorf("failed to open log file: %v", err)
	}

	// 设置日志同时输出到控制台和文件
	mw := io.MultiWriter(os.Stdout, file)
	log.SetOutput(mw)

	return file, nil
}

// checkPortAvailable 检查端口是否可用
func checkPortAvailable(host string, port int) error {
	addr := fmt.Sprintf("%s:%d", host, port)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("port %d is already in use: %v", port, err)
	}
	listener.Close()
	return nil
}

// showPortInUseError 显示端口占用错误对话框
func showPortInUseError(port int) {
	// Windows 下使用 MessageBox 显示错误
	system.ShowErrorDialog(
		"Port Already In Use",
		fmt.Sprintf("API port %d is already in use.\n\nPlease check if another instance is running or change the port in config.json.", port),
	)
}

func main() {
	// 初始化日志
	log.SetFormatter(&log.TextFormatter{
		FullTimestamp: true,
	})
	log.SetLevel(log.InfoLevel)

	// 加载配置
	cfg := config.LoadConfig()

	// 如果启用了文件日志，设置文件日志
	if cfg.EnableFileLog {
		var err error
		logFile, err = setupFileLogging()
		if err != nil {
			log.Warnf("Failed to setup file logging: %v", err)
		} else {
			log.Info("File logging enabled, logs will be saved to log/ directory")
			defer logFile.Close()
		}
	}

	// 检查端口是否被占用
	if err := checkPortAvailable(cfg.Host, cfg.Port); err != nil {
		log.Errorf("Port check failed: %v", err)
		showPortInUseError(cfg.Port)
		os.Exit(1)
	}
	log.Infof("Port %d is available", cfg.Port)

	// 初始化数据库
	db, err := database.InitDB(cfg.DatabasePath)
	if err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}
	defer db.Close()

	// 创建服务
	routeService := service.NewRouteService(db)
	proxyService := service.NewProxyService(routeService, cfg)

	// 初始化开机自启动管理器
	autoStart := system.NewAutoStart()

	// 创建应用实例
	app := &App{
		routeService: routeService,
		proxyService: proxyService,
		config:       cfg,
		autoStart:    autoStart,
	}

	// 启动后台 API 服务器
	go func() {
		gin.SetMode(gin.ReleaseMode)
		r := router.SetupAPIRouter(cfg, routeService, proxyService)
		addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)
		log.Infof("API server started at %s/api", addr)
		if err := r.Run(addr); err != nil {
			log.Errorf("Failed to start API server: %v", err)
		}
	}()

	// 创建 Wails 应用
	log.Info("Starting Wails GUI application...")

	// 从 embed.FS 中提取 frontend/dist 子目录
	distFS, err := fs.Sub(assets, "frontend/dist")
	if err != nil {
		log.Fatalf("Failed to get dist subdirectory: %v", err)
	}

	err = wails.Run(&options.App{
		Title:  "AnyProxyAi Manager",
		Width:  1280,
		Height: 800,
		AssetServer: &assetserver.Options{
			Assets: distFS,
		},
		BackgroundColour: &options.RGBA{R: 27, G: 38, B: 54, A: 1},
		OnStartup:        app.startup,
		OnBeforeClose:    app.beforeClose,
		Bind: []interface{}{
			app,
		},
	})

	if err != nil {
		log.Fatalf("Error running Wails app: %v", err)
	}
}

// App 结构体用于 Wails 绑定
type App struct {
	ctx          context.Context
	routeService *service.RouteService
	proxyService *service.ProxyService
	config       *config.Config
	autoStart    *system.AutoStart
	systemTray   *system.SystemTray
	forceQuit    bool
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	log.Info("=== Wails application startup callback executed ===")

	// 初始化系统托盘
	a.systemTray = system.NewSystemTray(ctx)
	a.systemTray.SetQuitCallback(func() {
		a.forceQuit = true
		a.config.MinimizeToTray = false
		runtime.Quit(ctx)
	})
	if err := a.systemTray.Setup(); err != nil {
		log.Warnf("Failed to setup system tray: %v", err)
	}
}

// beforeClose 在窗口关闭前调用
func (a *App) beforeClose(ctx context.Context) (prevent bool) {
	if a.forceQuit {
		log.Info("Force quit from tray, closing application")
		return false
	}

	if a.config.MinimizeToTray {
		log.Info("Minimizing to tray instead of closing")
		runtime.WindowHide(a.ctx)
		return true
	}

	log.Info("Application closing")
	return false
}

// GetRoutes 获取所有路由
func (a *App) GetRoutes() ([]map[string]interface{}, error) {
	routes, err := a.routeService.GetAllRoutes()
	if err != nil {
		return nil, err
	}

	result := make([]map[string]interface{}, len(routes))
	for i, route := range routes {
		result[i] = map[string]interface{}{
			"id":      route.ID,
			"name":    route.Name,
			"model":   route.Model,
			"api_url": route.APIUrl,
			"api_key": route.APIKey,
			"group":   route.Group,
			"format":  route.Format,
			"enabled": route.Enabled,
			"created": route.CreatedAt,
			"updated": route.UpdatedAt,
		}
	}
	return result, nil
}

// AddRoute 添加路由
func (a *App) AddRoute(name, model, apiUrl, apiKey, group, format string) error {
	return a.routeService.AddRoute(name, model, apiUrl, apiKey, group, format)
}

// UpdateRoute 更新路由
func (a *App) UpdateRoute(id int64, name, model, apiUrl, apiKey, group, format string) error {
	return a.routeService.UpdateRoute(id, name, model, apiUrl, apiKey, group, format)
}

// DeleteRoute 删除路由
func (a *App) DeleteRoute(id int64) error {
	return a.routeService.DeleteRoute(id)
}

// GetStats 获取统计信息
func (a *App) GetStats() (map[string]interface{}, error) {
	stats, err := a.routeService.GetStats()
	if err != nil {
		return nil, err
	}
	return stats, nil
}

// GetDailyStats 获取每日统计（用于热力图）
func (a *App) GetDailyStats(days int) ([]map[string]interface{}, error) {
	return a.routeService.GetDailyStats(days)
}

// GetHourlyStats 获取今日按小时统计（用于折线图）
func (a *App) GetHourlyStats() ([]map[string]interface{}, error) {
	return a.routeService.GetHourlyStats()
}

// GetModelRanking 获取模型使用排行
func (a *App) GetModelRanking(limit int) ([]map[string]interface{}, error) {
	return a.routeService.GetModelRanking(limit)
}

// GetConfig 获取配置
func (a *App) GetConfig() map[string]interface{} {
	return map[string]interface{}{
		"localApiKey":         a.config.LocalAPIKey,
		"openaiEndpoint":      fmt.Sprintf("http://%s:%d", a.config.Host, a.config.Port),
		"redirectEnabled":     a.config.RedirectEnabled,
		"redirectKeyword":     a.config.RedirectKeyword,
		"redirectTargetModel": a.config.RedirectTargetModel,
		"redirectTargetName":  a.config.RedirectTargetName,
		"minimizeToTray":      a.config.MinimizeToTray,
		"autoStart":           a.config.AutoStart,
		"enableFileLog":       a.config.EnableFileLog,
	}
}

// UpdateConfig 更新配置
func (a *App) UpdateConfig(redirectEnabled bool, redirectKeyword, redirectTargetModel string) error {
	a.config.RedirectEnabled = redirectEnabled
	a.config.RedirectKeyword = redirectKeyword
	a.config.RedirectTargetModel = redirectTargetModel
	return a.config.Save()
}

// UpdateLocalApiKey 更新本地 API Key
func (a *App) UpdateLocalApiKey(newApiKey string) error {
	a.config.LocalAPIKey = newApiKey
	return a.config.Save()
}

// FetchRemoteModels 获取远程模型列表
func (a *App) FetchRemoteModels(apiUrl, apiKey string) ([]string, error) {
	return a.proxyService.FetchRemoteModels(apiUrl, apiKey)
}

// ImportRouteFromFormat 从不同格式导入路由
func (a *App) ImportRouteFromFormat(name, model, apiUrl, apiKey, group, targetFormat string) (string, error) {
	return a.routeService.ImportRouteFromFormat(name, model, apiUrl, apiKey, group, targetFormat)
}

// GetAppSettings 获取应用设置
func (a *App) GetAppSettings() map[string]interface{} {
	autoStartEnabled := false
	if a.autoStart != nil {
		autoStartEnabled = a.autoStart.IsAutoStartEnabled()
	}

	return map[string]interface{}{
		"minimizeToTray":   a.config.MinimizeToTray,
		"autoStart":        a.config.AutoStart,
		"autoStartEnabled": autoStartEnabled,
	}
}

// SetMinimizeToTray 设置关闭时最小化到托盘
func (a *App) SetMinimizeToTray(enabled bool) error {
	log.Infof("Setting minimize to tray: %v", enabled)
	a.config.MinimizeToTray = enabled

	if err := a.config.Save(); err != nil {
		log.Errorf("Failed to save config: %v", err)
		return fmt.Errorf("failed to save config: %v", err)
	}

	log.Info("Minimize to tray setting updated successfully")
	return nil
}

// SetAutoStart 设置开机自启动
func (a *App) SetAutoStart(enabled bool) error {
	log.Infof("Setting auto-start: %v", enabled)

	if a.autoStart == nil {
		log.Error("Auto-start manager not initialized")
		return fmt.Errorf("auto-start manager not initialized")
	}

	if enabled {
		if err := a.autoStart.EnableAutoStart(); err != nil {
			log.Errorf("Failed to enable auto-start: %v", err)
			return fmt.Errorf("failed to enable auto-start: %v", err)
		}
	} else {
		if err := a.autoStart.DisableAutoStart(); err != nil {
			log.Errorf("Failed to disable auto-start: %v", err)
			return fmt.Errorf("failed to disable auto-start: %v", err)
		}
	}

	a.config.AutoStart = enabled
	if err := a.config.Save(); err != nil {
		log.Errorf("Failed to save config: %v", err)
		return fmt.Errorf("failed to save config: %v", err)
	}

	log.Info("Auto-start setting updated successfully")
	return nil
}

// ShowWindow 显示窗口
func (a *App) ShowWindow() {
	log.Info("Showing window")
	runtime.WindowShow(a.ctx)
	runtime.WindowUnminimise(a.ctx)
}

// HideWindow 隐藏窗口
func (a *App) HideWindow() {
	log.Info("Hiding window")
	runtime.WindowHide(a.ctx)
}

// QuitApp 退出应用
func (a *App) QuitApp() {
	log.Info("Quitting application")
	a.forceQuit = true
	a.config.MinimizeToTray = false
	runtime.Quit(a.ctx)
}

// ClearStats 清除统计数据
func (a *App) ClearStats() error {
	err := a.routeService.ClearStats()
	if err != nil {
		return fmt.Errorf("failed to clear statistics: %v", err)
	}

	log.Info("Statistics cleared successfully")
	return nil
}

// SetEnableFileLog 设置是否启用文件日志
func (a *App) SetEnableFileLog(enabled bool) error {
	log.Infof("Setting enable file log: %v", enabled)
	a.config.EnableFileLog = enabled

	if err := a.config.Save(); err != nil {
		log.Errorf("Failed to save config: %v", err)
		return fmt.Errorf("failed to save config: %v", err)
	}

	// 如果启用日志且当前没有日志文件，则设置文件日志
	if enabled && logFile == nil {
		var err error
		logFile, err = setupFileLogging()
		if err != nil {
			log.Warnf("Failed to setup file logging: %v", err)
			return fmt.Errorf("failed to setup file logging: %v", err)
		}
		log.Info("File logging enabled")
	}

	// 如果禁用日志，关闭日志文件并重置输出
	if !enabled && logFile != nil {
		log.Info("File logging disabled")
		log.SetOutput(os.Stdout)
		logFile.Close()
		logFile = nil
	}

	log.Info("File log setting updated successfully")
	return nil
}
