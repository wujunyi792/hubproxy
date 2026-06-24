package main

import (
	"embed"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"hubproxy/config"
	"hubproxy/handlers"
	"hubproxy/utils"
)

//go:embed public/*
var staticFiles embed.FS

var (
	globalLimiter    *utils.IPRateLimiter
	serviceStartTime = time.Now()
)

var Version = "dev"

func serveEmbedFile(c *gin.Context, filename string) {
	data, err := staticFiles.ReadFile(filename)
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}

	contentType := "text/html; charset=utf-8"
	if strings.HasSuffix(filename, ".ico") {
		contentType = "image/x-icon"
	}
	c.Data(http.StatusOK, contentType, data)
}

func buildRouter(cfg *config.AppConfig) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	router := gin.Default()

	router.Use(gin.CustomRecovery(func(c *gin.Context, recovered interface{}) {
		log.Printf("Panic 已恢复: %v", recovered)
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": "Internal server error",
			"code":  "INTERNAL_ERROR",
		})
	}))

	router.Use(utils.RateLimitMiddleware(globalLimiter))

	initHealthRoutes(router)
	handlers.InitImageTarRoutes(router)

	if cfg.Server.EnableFrontend {
		router.GET("/", func(c *gin.Context) {
			serveEmbedFile(c, "public/index.html")
		})
		router.GET("/public/*filepath", func(c *gin.Context) {
			filepath := strings.TrimPrefix(c.Param("filepath"), "/")
			serveEmbedFile(c, "public/"+filepath)
		})
		router.GET("/images.html", func(c *gin.Context) {
			serveEmbedFile(c, "public/images.html")
		})
		router.GET("/search.html", func(c *gin.Context) {
			serveEmbedFile(c, "public/search.html")
		})
		router.GET("/favicon.ico", func(c *gin.Context) {
			serveEmbedFile(c, "public/favicon.ico")
		})
	} else {
		router.GET("/", func(c *gin.Context) { c.Status(http.StatusNotFound) })
		router.GET("/public/*filepath", func(c *gin.Context) { c.Status(http.StatusNotFound) })
		router.GET("/images.html", func(c *gin.Context) { c.Status(http.StatusNotFound) })
		router.GET("/search.html", func(c *gin.Context) { c.Status(http.StatusNotFound) })
		router.GET("/favicon.ico", func(c *gin.Context) { c.Status(http.StatusNotFound) })
	}

	handlers.RegisterSearchRoute(router)

	router.Any("/token", handlers.ProxyDockerAuthGin)
	router.Any("/token/*path", handlers.ProxyDockerAuthGin)
	router.Any("/v2/*path", handlers.ProxyDockerRegistryGin)
	router.NoRoute(handlers.GitHubProxyHandler)

	return router
}

func main() {
	if err := config.LoadConfig(); err != nil {
		fmt.Printf("配置加载失败: %v\n", err)
		return
	}

	utils.InitHTTPClients()
	globalLimiter = utils.InitGlobalLimiter()
	handlers.InitDockerProxy()
	handlers.InitImageStreamer()
	handlers.InitDebouncer()

	cfg := config.GetConfig()
	router := buildRouter(cfg)

	fmt.Printf("HubProxy 启动成功\n")
	fmt.Printf("监听地址: %s:%d\n", cfg.Server.Host, cfg.Server.Port)
	fmt.Printf("限流配置: %d请求/%g小时\n", cfg.RateLimit.RequestLimit, cfg.RateLimit.PeriodHours)
	if cfg.Server.EnableH2C {
		fmt.Printf("H2c: 已启用\n")
	}
	fmt.Printf("版本号: %s\n", Version)
	fmt.Printf("项目地址: https://github.com/wujunyi792/hubproxy\n")

	server := &http.Server{
		Addr:         fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port),
		ReadTimeout:  60 * time.Second,
		WriteTimeout: 30 * time.Minute,
		IdleTimeout:  120 * time.Second,
	}

	if cfg.Server.EnableH2C {
		server.Handler = h2c.NewHandler(router, &http2.Server{
			MaxConcurrentStreams:         250,
			IdleTimeout:                  300 * time.Second,
			MaxReadFrameSize:             4 << 20,
			MaxUploadBufferPerConnection: 8 << 20,
			MaxUploadBufferPerStream:     2 << 20,
		})
	} else {
		server.Handler = router
	}

	if err := server.ListenAndServe(); err != nil {
		fmt.Printf("启动服务失败: %v\n", err)
	}
}

func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%d秒", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%d分钟%d秒", int(d.Minutes()), int(d.Seconds())%60)
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%d小时%d分钟", int(d.Hours()), int(d.Minutes())%60)
	}

	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	return fmt.Sprintf("%d天%d小时", days, hours)
}

func getUptimeInfo() (time.Duration, float64, string) {
	uptime := time.Since(serviceStartTime)
	return uptime, uptime.Seconds(), formatDuration(uptime)
}

func initHealthRoutes(router *gin.Engine) {
	router.GET("/ready", func(c *gin.Context) {
		_, uptimeSec, uptimeHuman := getUptimeInfo()
		c.JSON(http.StatusOK, gin.H{
			"ready":           true,
			"service":         "hubproxy",
			"version":         Version,
			"start_time_unix": serviceStartTime.Unix(),
			"uptime_sec":      uptimeSec,
			"uptime_human":    uptimeHuman,
		})
	})
}
