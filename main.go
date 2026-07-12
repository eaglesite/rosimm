package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gocolly/colly"
	"github.com/gocolly/colly/extensions"
	"github.com/google/uuid"
)

type spiderStatsData struct {
	sync.Mutex
	Status      string
	CurrentPage int
	TotalPages  int
	ImagesSaved int
	StartTime   string
}

type sourceState struct {
	running bool
	stats   spiderStatsData
}

var (
	sourceStates  = make(map[string]*sourceState)
	sourceStatesMutex sync.Mutex

	config        = loadConfig()
	adminPassword = config.AdminPassword
	frontendPassword = config.FrontendPassword
	serverPort    = config.ServerPort
	sessions      = make(map[string]sessionData)
	sessionsMutex sync.Mutex

	scheduledConfigMutex sync.Mutex
	lastScheduledRun     time.Time

	imageCache     = make(map[string][]string)
	imageCacheMu   sync.RWMutex

	dataDir string
)

type sessionData struct {
	username string
	role     string
	expireAt time.Time
}

type RequestConfig struct {
	Timeout    int               `json:"timeout"`
	UserAgent  string            `json:"user_agent"`
	RetryCount int               `json:"retry_count"`
	RetryDelay int               `json:"retry_delay"`
	Proxy      string            `json:"proxy"`
	Headers    map[string]string `json:"headers"`
}

type Config struct {
	AdminPassword    string        `json:"admin_password"`
	FrontendPassword string        `json:"frontend_password"`
	ServerPort       string        `json:"server_port"`
	Schedule         Schedule      `json:"schedule"`
	Sources          []Source      `json:"sources"`
	RequestConfig    RequestConfig `json:"request_config"`
}

type Schedule struct {
	Enabled bool   `json:"enabled"`
	Time    string `json:"time"`
	ImgType string `json:"img_type"`
}

type Source struct {
	Name   string   `json:"name"`
	Label  string   `json:"label"`
	Prefix string   `json:"prefix"`
	URLs   []string `json:"urls"`
}

func initSourceStates() {
	sourceStatesMutex.Lock()
	defer sourceStatesMutex.Unlock()
	for _, s := range config.Sources {
		if _, ok := sourceStates[s.Name]; !ok {
			sourceStates[s.Name] = &sourceState{}
		}
	}
}

func defaultRequestConfig() RequestConfig {
	return RequestConfig{
		Timeout:    120,
		UserAgent:  "",
		RetryCount: 3,
		RetryDelay: 1,
		Proxy:      "",
		Headers: map[string]string{
			"Accept-Language": "zh-CN,zh;q=0.9",
			"Cache-Control":   "no-cache",
			"Pragma":          "no-cache",
		},
	}
}

func defaultConfig() Config {
	return Config{
		AdminPassword:    "admin",
		FrontendPassword: "123456",
		ServerPort:       "80",
		Schedule: Schedule{
			Enabled: false,
			Time:    "08:00",
			ImgType: "new",
		},
		Sources: []Source{
			{Name: "rosi", Label: "图片系列", Prefix: "NO.", URLs: []string{"https://rosi51.com/rosi?page=%d&type=%s"}},
		},
		RequestConfig: defaultRequestConfig(),
	}
}

func applyRequestConfigDefaults(cfg *Config) {
	if cfg.Schedule.Time == "" {
		cfg.Schedule.Time = "08:00"
	}
	if cfg.Schedule.ImgType == "" {
		cfg.Schedule.ImgType = "new"
	}
	if len(cfg.Sources) == 0 {
		cfg.Sources = []Source{
			{Name: "rosi", Label: "图片系列", Prefix: "NO.", URLs: []string{"https://rosi51.com/rosi?page=%d&type=%s"}},
		}
	}
	if cfg.RequestConfig.Timeout <= 0 {
		cfg.RequestConfig.Timeout = 120
	}
	if cfg.RequestConfig.RetryCount < 0 {
		cfg.RequestConfig.RetryCount = 3
	}
	if cfg.RequestConfig.RetryDelay <= 0 {
		cfg.RequestConfig.RetryDelay = 1
	}
	if cfg.RequestConfig.Headers == nil {
		cfg.RequestConfig.Headers = map[string]string{
			"Accept-Language": "zh-CN,zh;q=0.9",
			"Cache-Control":   "no-cache",
			"Pragma":          "no-cache",
		}
	}
}

func loadConfig() Config {
	file, err := os.ReadFile(configPath)
	if err != nil {
		fmt.Println("配置文件不存在，使用默认配置")
		return defaultConfig()
	}

	var cfg Config
	err = json.Unmarshal(file, &cfg)
	if err != nil {
		fmt.Println("配置文件解析错误，使用默认配置")
		return defaultConfig()
	}

	applyRequestConfigDefaults(&cfg)
	return cfg
}

func applyRequestConfigToCollector(c *colly.Collector) {
	rc := config.RequestConfig

	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   time.Duration(rc.Timeout) * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		MaxIdleConns:          500,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   time.Duration(rc.Timeout) * time.Second,
		ExpectContinueTimeout: 10 * time.Second,
		MaxIdleConnsPerHost:   100,
	}

	if rc.Proxy != "" {
		proxyURL, err := url.Parse(rc.Proxy)
		if err == nil {
			transport.Proxy = http.ProxyURL(proxyURL)
		}
	}

	c.WithTransport(transport)

	if rc.UserAgent != "" {
		c.UserAgent = rc.UserAgent
	} else {
		extensions.RandomUserAgent(c)
	}

	c.OnRequest(func(r *colly.Request) {
		for k, v := range rc.Headers {
			r.Headers.Add(k, v)
		}
	})

	c.SetRequestTimeout(time.Duration(rc.Timeout) * time.Second)
}

func getSource(name string) *Source {
	for i := range config.Sources {
		if config.Sources[i].Name == name {
			return &config.Sources[i]
		}
	}
	return nil
}

var configPath = "config.json"

func init() {
	if v := os.Getenv("CONFIG_PATH"); v != "" {
		configPath = v
	}
}

func saveConfigFile() {
	newCfg := Config{
		AdminPassword:    adminPassword,
		FrontendPassword: frontendPassword,
		ServerPort:       serverPort,
		Schedule:         config.Schedule,
		Sources:          config.Sources,
		RequestConfig:    config.RequestConfig,
	}
	file, err := json.MarshalIndent(newCfg, "", "    ")
	if err == nil {
		if err := os.WriteFile(configPath, file, 0644); err != nil {
			fmt.Printf("[配置] 保存失败（只读文件系统？）: %v\n", err)
		}
	}
}

func getImageFiles() ([]string, error) {
	var images []string
	err := filepath.Walk(dataDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			ext := strings.ToLower(filepath.Ext(path))
			if ext == ".jpg" || ext == ".jpeg" || ext == ".png" || ext == ".gif" {
				images = append(images, filepath.Base(path))
			}
		}
		return nil
	})
	return images, err
}

func buildImageCache() {
	allImages, err := getImageFiles()
	if err != nil {
		fmt.Println("构建图片缓存失败:", err)
		return
	}
	cache := make(map[string][]string, len(config.Sources))
	for _, s := range config.Sources {
		var filtered []string
		for _, img := range allImages {
			if strings.HasPrefix(img, s.Prefix) {
				filtered = append(filtered, img)
			}
		}
		sort.Slice(filtered, func(i, j int) bool {
			if len(filtered[i]) != len(filtered[j]) {
				return len(filtered[i]) > len(filtered[j])
			}
			return filtered[i] > filtered[j]
		})
		cache[s.Name] = filtered
	}

	imageCacheMu.Lock()
	imageCache = cache
	imageCacheMu.Unlock()
	fmt.Printf("[缓存] 图片缓存构建完成，共 %d 个源\n", len(cache))
}

func addToImageCache(source, imgName string) {
	imageCacheMu.Lock()
	defer imageCacheMu.Unlock()
	list := imageCache[source]
	idx := sort.Search(len(list), func(i int) bool {
		if len(list[i]) != len(imgName) {
			return len(list[i]) <= len(imgName)
		}
		return list[i] <= imgName
	})
	imageCache[source] = append(list, "")
	copy(imageCache[source][idx+1:], imageCache[source][idx:])
	imageCache[source][idx] = imgName
}

func generateSessionID() string {
	return uuid.New().String()
}

func createSession(username, role string) string {
	sessionID := generateSessionID()
	sessionsMutex.Lock()
	sessions[sessionID] = sessionData{
		username: username,
		role:     role,
		expireAt: time.Now().Add(24 * time.Hour),
	}
	sessionsMutex.Unlock()
	return sessionID
}

func validateSession(sessionID string) bool {
	sessionsMutex.Lock()
	defer sessionsMutex.Unlock()

	data, exists := sessions[sessionID]
	if !exists {
		return false
	}

	if time.Now().After(data.expireAt) {
		delete(sessions, sessionID)
		return false
	}

	return true
}

func cleanExpiredSessions() {
	sessionsMutex.Lock()
	defer sessionsMutex.Unlock()
	now := time.Now()
	for id, data := range sessions {
		if now.After(data.expireAt) {
			delete(sessions, id)
		}
	}
}

func startSessionCleaner() {
	go func() {
		for {
			time.Sleep(1 * time.Hour)
			cleanExpiredSessions()
		}
	}()
}

func deleteSession(sessionID string) {
	sessionsMutex.Lock()
	delete(sessions, sessionID)
	sessionsMutex.Unlock()
}

func authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sessionID, err := r.Cookie("session_id")
		if err != nil || !validateSession(sessionID.Value) {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		next(w, r)
	}
}

func adminAuthMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sessionID, err := r.Cookie("admin_session_id")
		if err != nil || !validateSession(sessionID.Value) {
			http.Redirect(w, r, "/login_admin", http.StatusFound)
			return
		}
		next(w, r)
	}
}

func getStatsAndRunning(source string) (*spiderStatsData, *bool) {
	sourceStatesMutex.Lock()
	defer sourceStatesMutex.Unlock()
	state, ok := sourceStates[source]
	if !ok {
		state = &sourceState{}
		sourceStates[source] = state
	}
	return &state.stats, &state.running
}

func spiderSaveImage(source, imgUrl, name, logPrefix string, stats *spiderStatsData, running *bool) {
	spiderMutex.Lock()
	if !*running {
		spiderMutex.Unlock()
		return
	}
	spiderMutex.Unlock()

	imgname := name + ".jpg"
	rc := config.RequestConfig
	tr := &http.Transport{}
	if rc.Proxy != "" {
		proxyURL, err := url.Parse(rc.Proxy)
		if err == nil {
			tr.Proxy = http.ProxyURL(proxyURL)
		}
	}
	client := &http.Client{Timeout: time.Duration(rc.Timeout) * time.Second, Transport: tr}

	var resp *http.Response
	var err error
	for attempt := 0; attempt <= rc.RetryCount; attempt++ {
		spiderMutex.Lock()
		if !*running {
			spiderMutex.Unlock()
			return
		}
		spiderMutex.Unlock()

		fmt.Println(logPrefix + "正在获取图片 " + imgname)
		resp, err = client.Get(imgUrl)
		if err == nil {
			break
		}
		fmt.Printf("%s获取图片失败 (尝试 %d/%d): %v\n", logPrefix, attempt+1, rc.RetryCount+1, err)
		if attempt < rc.RetryCount {
			for i := 0; i < rc.RetryDelay*10; i++ {
				spiderMutex.Lock()
				if !*running {
					spiderMutex.Unlock()
					return
				}
				spiderMutex.Unlock()
				time.Sleep(100 * time.Millisecond)
			}
		}
	}
	if err != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if err == nil {
			resp.Body.Close()
			fmt.Printf("%s获取图片返回非正常状态码: %d，跳过\n", logPrefix, resp.StatusCode)
		}
		fmt.Println(logPrefix + "获取图片错误，已跳过")
		return
	}
	defer resp.Body.Close()

	out, err := os.Create(filepath.Join(dataDir, imgname))
	if err != nil {
		fmt.Println(logPrefix + "创建文件错误，跳过")
		return
	}
	defer out.Close()

	_, err = io.Copy(out, resp.Body)
	if err != nil {
		fmt.Println(logPrefix + "保存图片错误，跳过")
		return
	}

	stats.Lock()
	stats.ImagesSaved++
	stats.Unlock()

	addToImageCache(source, imgname)

	fmt.Println(logPrefix + "保存图片成功 " + imgname)
	for i := 0; i < 10; i++ {
		spiderMutex.Lock()
		if !*running {
			spiderMutex.Unlock()
			return
		}
		spiderMutex.Unlock()
		time.Sleep(100 * time.Millisecond)
	}
}

var spiderMutex sync.Mutex

func runSpider(source, imgType string, startPage, endPage int) {
	srcCfg := getSource(source)
	if srcCfg == nil {
		fmt.Printf("[%s] 未知的爬虫源\n", source)
		return
	}

	stats, running := getStatsAndRunning(source)

	spiderMutex.Lock()
	if *running {
		spiderMutex.Unlock()
		return
	}
	*running = true
	spiderMutex.Unlock()

	stats.Lock()
	stats.Status = "running"
	stats.CurrentPage = startPage
	stats.TotalPages = endPage
	stats.ImagesSaved = 0
	stats.StartTime = time.Now().Format(time.RFC3339)
	stats.Unlock()

	fmt.Printf("[%s] 爬虫开始运行 - 类型: %s, 页数: %d-%d\n", source, imgType, startPage, endPage)

	stopped := false
	c := colly.NewCollector()
	applyRequestConfigToCollector(c)

	c.OnHTML("div.outerwide ul.mainList", func(e *colly.HTMLElement) {
		e.ForEach(".item a img.mainList_cover", func(i int, element *colly.HTMLElement) {
			spiderSaveImage(source, element.Attr("src"), element.Attr("title"), "["+source+"] ", stats, running)
		})
	})

	c.OnHTML("div.outerwide ul.block2", func(e *colly.HTMLElement) {
		e.ForEach("li", func(i int, element *colly.HTMLElement) {
			spiderSaveImage(source, element.ChildAttr("div.portfolio-thumb img", "src"), element.ChildAttr("div.portfolio-thumb img", "title"), "["+source+"] ", stats, running)
		})
	})

	c.OnError(func(r *colly.Response, err error) {
		fmt.Println("[" + source + "] 请求错误:" + err.Error())
	})

	for page := startPage; page <= endPage; page++ {
		spiderMutex.Lock()
		if !*running {
			spiderMutex.Unlock()
			break
		}
		spiderMutex.Unlock()

		stats.Lock()
		stats.CurrentPage = page
		stats.Unlock()

		for _, urlTpl := range srcCfg.URLs {
			spiderMutex.Lock()
			if !*running {
				stopped = true
				spiderMutex.Unlock()
				goto done
			}
			spiderMutex.Unlock()

			webUrl := fmt.Sprintf(urlTpl, page, imgType)
			fmt.Println("[" + source + "] 访问: " + webUrl)
			c.Visit(webUrl)

			for i := 0; i < 25; i++ {
				spiderMutex.Lock()
				if !*running {
					stopped = true
					spiderMutex.Unlock()
					goto done
				}
				spiderMutex.Unlock()
				time.Sleep(200 * time.Millisecond)
			}
		}
	}

done:

	spiderMutex.Lock()
	stats.Lock()
	if !*running || stopped {
		stats.Status = "stopped"
	} else {
		stats.Status = "finished"
	}
	stats.CurrentPage = endPage
	*running = false
	stats.Unlock()
	spiderMutex.Unlock()

	fmt.Printf("[%s] 爬虫运行完成\n", source)
}

func runScheduledCrawl(source, imgType string) {
	srcCfg := getSource(source)
	if srcCfg == nil {
		return
	}

	stats, running := getStatsAndRunning(source)

	spiderMutex.Lock()
	if *running {
		spiderMutex.Unlock()
		fmt.Printf("[定时任务][%s] 爬虫正在运行中，跳过本次定时任务\n", source)
		return
	}
	*running = true
	spiderMutex.Unlock()

	stats.Lock()
	stats.Status = "scheduled"
	stats.CurrentPage = 1
	stats.TotalPages = 1
	stats.ImagesSaved = 0
	stats.StartTime = time.Now().Format(time.RFC3339)
	stats.Unlock()

	fmt.Printf("[定时任务][%s] 每日定时爬取启动 - 类型: %s\n", source, imgType)

	imageCacheMu.RLock()
	existingImages := imageCache[source]
	if existingImages == nil {
		existingImages = []string{}
	}
	imageCacheMu.RUnlock()
	existingMap := make(map[string]bool, len(existingImages))
	for _, img := range existingImages {
		existingMap[img] = true
	}

	c := colly.NewCollector()
	applyRequestConfigToCollector(c)

	stopped := false
	allExist := true
	logPrefix := "[定时任务][" + source + "] "

	c.OnHTML("div.outerwide ul.mainList", func(e *colly.HTMLElement) {
		e.ForEach(".item a img.mainList_cover", func(i int, element *colly.HTMLElement) {
			imgSrc := element.Attr("src")
			name := element.Attr("title")
			imgname := name + ".jpg"

			if _, exists := existingMap[imgname]; exists {
				fmt.Println(logPrefix + "图片已存在，跳过: " + imgname)
				return
			}
			allExist = false
			spiderSaveImage(source, imgSrc, name, logPrefix, stats, running)
		})
	})

	c.OnHTML("div.outerwide ul.block2", func(e *colly.HTMLElement) {
		e.ForEach("li", func(i int, element *colly.HTMLElement) {
			imgSrc := element.ChildAttr("div.portfolio-thumb img", "src")
			name := element.ChildAttr("div.portfolio-thumb img", "title")
			imgname := name + ".jpg"

			if _, exists := existingMap[imgname]; exists {
				fmt.Println(logPrefix + "图片已存在，跳过: " + imgname)
				return
			}
			allExist = false
			spiderSaveImage(source, imgSrc, name, logPrefix, stats, running)
		})
	})

	c.OnError(func(r *colly.Response, err error) {
		fmt.Println(logPrefix + "请求错误:" + err.Error())
	})

	for _, urlTpl := range srcCfg.URLs {
		webUrl := fmt.Sprintf(urlTpl, 1, imgType)
		fmt.Println(logPrefix + "访问: " + webUrl)
		c.Visit(webUrl)
	}

	if allExist {
		fmt.Println(logPrefix + "所有图片均已存在，无需下载")
	}

	spiderMutex.Lock()
	stats.Lock()
	if !*running || stopped {
		stats.Status = "stopped"
	} else {
		stats.Status = "finished"
	}
	*running = false
	stats.Unlock()
	spiderMutex.Unlock()

	fmt.Println(logPrefix + "每日定时爬取完成")
}

func startScheduler() {
	go func() {
		fmt.Printf("[定时器] 调度器已启动，计划时间: %s\n", config.Schedule.Time)
		for {
			now := time.Now()
			currentTime := now.Format("15:04")
			currentDate := now.Format("2006-01-02")

		scheduledConfigMutex.Lock()
		enabled := config.Schedule.Enabled
		scheduledTime := config.Schedule.Time
		imgType := config.Schedule.ImgType

		runToday := lastScheduledRun.Format("2006-01-02") == currentDate
		alreadyRun := runToday && lastScheduledRun.Format("15:04") >= scheduledTime
		if enabled && currentTime == scheduledTime && !alreadyRun {
			lastScheduledRun = time.Now()
			scheduledConfigMutex.Unlock()
			fmt.Printf("[定时器] 到达预定时间 %s，开始执行每日定时爬取\n", scheduledTime)
			for _, s := range config.Sources {
				go runScheduledCrawl(s.Name, imgType)
			}
		} else {
			scheduledConfigMutex.Unlock()
		}

			time.Sleep(60 * time.Second)
		}
	}()
}

func main() {
	initSourceStates()

	if v := os.Getenv("DATA_DIR"); v != "" {
		dataDir = v
	} else {
		dataDir = "."
	}
	os.MkdirAll(dataDir, 0755)

	if v := os.Getenv("PORT"); v != "" {
		serverPort = v
	}

	http.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "login.html")
	})

	http.HandleFunc("/login_admin", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "login_admin.html")
	})

	http.HandleFunc("/api/login", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "参数解析错误", http.StatusBadRequest)
			return
		}

		password := r.FormValue("password")
		if password == frontendPassword {
			sessionID := createSession("frontend", "frontend")
			http.SetCookie(w, &http.Cookie{
				Name:     "session_id",
				Value:    sessionID,
				HttpOnly: true,
				MaxAge:   86400,
				Path:     "/",
			})
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true,
				"message": "登录成功",
			})
		} else {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"message": "密码错误",
			})
		}
	})

	http.HandleFunc("/api/login/admin", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "参数解析错误", http.StatusBadRequest)
			return
		}

		password := r.FormValue("password")
		if password == adminPassword {
			sessionID := createSession("admin", "admin")
			http.SetCookie(w, &http.Cookie{
				Name:     "admin_session_id",
				Value:    sessionID,
				HttpOnly: true,
				MaxAge:   86400,
				Path:     "/",
			})
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true,
				"message": "登录成功",
			})
		} else {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"message": "密码错误",
			})
		}
	})

	http.HandleFunc("/api/logout", func(w http.ResponseWriter, r *http.Request) {
		sessionID, err := r.Cookie("session_id")
		if err == nil {
			deleteSession(sessionID.Value)
		}
		adminSessionID, err := r.Cookie("admin_session_id")
		if err == nil {
			deleteSession(adminSessionID.Value)
		}
		http.SetCookie(w, &http.Cookie{
			Name:     "session_id",
			Value:    "",
			HttpOnly: true,
			MaxAge:   -1,
			Path:     "/",
		})
		http.SetCookie(w, &http.Cookie{
			Name:     "admin_session_id",
			Value:    "",
			HttpOnly: true,
			MaxAge:   -1,
			Path:     "/",
		})
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"message": "退出成功",
		})
	})

	http.HandleFunc("/", authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, "index.html")
	}))

	http.HandleFunc("/admin", adminAuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, "admin.html")
	}))

	http.Handle("/images/", authMiddleware(func(w http.ResponseWriter, r *http.Request) {
		ext := strings.ToLower(filepath.Ext(r.URL.Path))
		if ext != ".jpg" && ext != ".jpeg" && ext != ".png" && ext != ".gif" {
			http.NotFound(w, r)
			return
		}
		http.StripPrefix("/images/", http.FileServer(http.Dir(dataDir))).ServeHTTP(w, r)
	}))

	http.HandleFunc("/api/sources", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		json.NewEncoder(w).Encode(config.Sources)
	})

	http.HandleFunc("/api/images", func(w http.ResponseWriter, r *http.Request) {
		source := r.URL.Query().Get("source")
		if source == "" {
			if len(config.Sources) > 0 {
				source = config.Sources[0].Name
			} else {
				source = "rosi"
			}
		}

		imageCacheMu.RLock()
		filtered, exists := imageCache[source]
		if !exists {
			filtered = []string{}
		}
		allFiltered := make([]string, len(filtered))
		copy(allFiltered, filtered)
		imageCacheMu.RUnlock()

		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page < 1 {
			page = 1
		}

		pageSize, _ := strconv.Atoi(r.URL.Query().Get("pageSize"))
		if pageSize < 1 {
			pageSize = 20
		}

		total := len(filtered)
		totalPages := (total + pageSize - 1) / pageSize

		start := (page - 1) * pageSize
		end := start + pageSize
		if start >= total {
			filtered = []string{}
		} else if end > total {
			filtered = filtered[start:total]
		} else {
			filtered = filtered[start:end]
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"images":     filtered,
			"allImages":  allFiltered,
			"count":      len(filtered),
			"total":      total,
			"page":       page,
			"pageSize":   pageSize,
			"totalPages": totalPages,
			"time":       time.Now().Format(time.RFC3339),
		})
	})

	http.HandleFunc("/api/images/count", func(w http.ResponseWriter, r *http.Request) {
		source := r.URL.Query().Get("source")
		if source == "" {
			if len(config.Sources) > 0 {
				source = config.Sources[0].Name
			} else {
				source = "rosi"
			}
		}

		imageCacheMu.RLock()
		images, exists := imageCache[source]
		count := 0
		if exists {
			count = len(images)
		}
		imageCacheMu.RUnlock()

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"total": count,
			"time":  time.Now().Format(time.RFC3339),
		})
	})

	http.HandleFunc("/api/spider/status", func(w http.ResponseWriter, r *http.Request) {
		source := r.URL.Query().Get("source")
		if source == "" {
			if len(config.Sources) > 0 {
				source = config.Sources[0].Name
			} else {
				source = "rosi"
			}
		}

		stats, running := getStatsAndRunning(source)

		stats.Lock()
		defer stats.Unlock()

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"running":     *running,
			"status":      stats.Status,
			"currentPage": stats.CurrentPage,
			"totalPages":  stats.TotalPages,
			"imagesSaved": stats.ImagesSaved,
			"startTime":   stats.StartTime,
		})
	})

	http.HandleFunc("/api/spider/start", adminAuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "参数解析错误", http.StatusBadRequest)
			return
		}

		source := r.FormValue("source")
		if source == "" {
			if len(config.Sources) > 0 {
				source = config.Sources[0].Name
			} else {
				source = "rosi"
			}
		}

		if getSource(source) == nil {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"message": "未知的爬虫源: " + source,
			})
			return
		}

		imgType := r.FormValue("imgType")
		if imgType == "" {
			imgType = "new"
		}

		startPage, err := strconv.Atoi(r.FormValue("startPage"))
		if err != nil || startPage < 1 {
			startPage = 1
		}

		endPage, err := strconv.Atoi(r.FormValue("endPage"))
		if err != nil || endPage < startPage {
			endPage = 100
		}

		_, running := getStatsAndRunning(source)
		spiderMutex.Lock()
		if *running {
			spiderMutex.Unlock()
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"message": "该爬虫已在运行中",
			})
			return
		}
		spiderMutex.Unlock()

		go runSpider(source, imgType, startPage, endPage)

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"message": "爬虫已启动",
			"params": map[string]interface{}{
				"source":    source,
				"imgType":   imgType,
				"startPage": startPage,
				"endPage":   endPage,
			},
		})
	}))

	http.HandleFunc("/api/spider/stop", adminAuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "参数解析错误", http.StatusBadRequest)
			return
		}

		source := r.FormValue("source")
		if source == "" {
			if len(config.Sources) > 0 {
				source = config.Sources[0].Name
			} else {
				source = "rosi"
			}
		}

		stats, running := getStatsAndRunning(source)

		spiderMutex.Lock()
		*running = false
		spiderMutex.Unlock()

		stats.Lock()
		stats.Status = "stopped"
		stats.Unlock()

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"message": "爬虫已停止",
		})
	}))

	http.HandleFunc("/api/spider/logs", adminAuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"logs": []string{"日志功能开发中"},
		})
	}))

	http.HandleFunc("/api/images/rebuild-cache", adminAuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
		buildImageCache()
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"message": "图片缓存已重建",
		})
	}))

	http.HandleFunc("/api/schedule/status", adminAuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
		scheduledConfigMutex.Lock()
		defer scheduledConfigMutex.Unlock()

		spiderMutex.Lock()
		sourceStatesMutex.Lock()
		isRunning := false
		for _, s := range config.Sources {
			state, ok := sourceStates[s.Name]
			if ok && state.running {
				isRunning = true
				break
			}
		}
		sourceStatesMutex.Unlock()
		spiderMutex.Unlock()

		lastRunStr := ""
		if !lastScheduledRun.IsZero() {
			lastRunStr = lastScheduledRun.Format("2006-01-02 15:04")
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"enabled":   config.Schedule.Enabled,
			"time":      config.Schedule.Time,
			"imgType":   config.Schedule.ImgType,
			"nextRun":   config.Schedule.Time,
			"lastRun":   lastRunStr,
			"isRunning": isRunning,
		})
	}))

	http.HandleFunc("/api/schedule/update", adminAuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "参数解析错误", http.StatusBadRequest)
			return
		}

		enabled := r.FormValue("enabled") == "true"
		runTime := r.FormValue("time")
		imgType := r.FormValue("imgType")

		if runTime == "" {
			runTime = "08:00"
		}
		if imgType == "" {
			imgType = "new"
		}

		scheduledConfigMutex.Lock()
		config.Schedule.Enabled = enabled
		config.Schedule.Time = runTime
		config.Schedule.ImgType = imgType
		scheduledConfigMutex.Unlock()

		saveConfigFile()

		fmt.Printf("[定时器] 设置已更新 - 启用: %v, 时间: %s, 类型: %s\n", enabled, runTime, imgType)

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"message": "定时任务设置已更新",
		})
	}))

	http.HandleFunc("/api/password/change", adminAuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "参数解析错误", http.StatusBadRequest)
			return
		}

		oldPassword := r.FormValue("oldPassword")
		newPassword := r.FormValue("newPassword")

		if oldPassword != adminPassword {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"message": "旧密码错误",
			})
			return
		}

		if len(newPassword) < 4 {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false,
				"message": "新密码长度不能少于4位",
			})
			return
		}

		adminPassword = newPassword

		saveConfigFile()

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"message": "密码修改成功",
		})
	}))

	http.HandleFunc("/api/request-config", adminAuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Access-Control-Allow-Origin", "*")
		json.NewEncoder(w).Encode(config.RequestConfig)
	}))

	http.HandleFunc("/api/request-config/update", adminAuthMiddleware(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "参数解析错误", http.StatusBadRequest)
			return
		}

		timeout, _ := strconv.Atoi(r.FormValue("timeout"))
		if timeout <= 0 {
			timeout = 120
		}

		retryCount, _ := strconv.Atoi(r.FormValue("retry_count"))
		if retryCount < 0 {
			retryCount = 3
		}

		retryDelay, _ := strconv.Atoi(r.FormValue("retry_delay"))
		if retryDelay <= 0 {
			retryDelay = 1
		}

		userAgent := r.FormValue("user_agent")
		proxy := r.FormValue("proxy")

		config.RequestConfig.Timeout = timeout
		config.RequestConfig.RetryCount = retryCount
		config.RequestConfig.RetryDelay = retryDelay
		config.RequestConfig.UserAgent = userAgent
		config.RequestConfig.Proxy = proxy

		saveConfigFile()

		fmt.Printf("[请求配置] 已更新 - 超时: %ds, 重试: %d, User-Agent: %s\n", timeout, retryCount, userAgent)

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true,
			"message": "请求配置已更新（部分设置将在下次爬虫启动时生效）",
		})
	}))

	fmt.Println("图片预览服务器已启动")
	fmt.Printf("访问地址: http://localhost:%s\n", serverPort)
	fmt.Printf("管理页面: http://localhost:%s/admin\n", serverPort)
	fmt.Printf("数据目录: %s\n", dataDir)

	if config.Schedule.Enabled {
		fmt.Printf("定时任务: 已启用，每日 %s 自动爬取\n", config.Schedule.Time)
	} else {
		fmt.Println("定时任务: 未启用")
	}
	fmt.Println("按 Ctrl+C 停止服务器")

	buildImageCache()
	startScheduler()
	startSessionCleaner()

	srv := &http.Server{Addr: ":" + serverPort}

	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, os.Interrupt)
		<-sigChan
		fmt.Println("\n正在关闭服务器...")
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}()

	err := srv.ListenAndServe()
	if err != nil && err != http.ErrServerClosed {
		fmt.Printf("服务器启动失败: %v\n", err)
	}
	fmt.Println("服务器已关闭")
}
