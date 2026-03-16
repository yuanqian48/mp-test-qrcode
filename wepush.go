package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strconv"
	"sync"
	"time"
)

const (
	appID        = "wx39c379788eb1286a"
	scope        = "snsapi_login"
	redirectURI  = "http://mp.weixin.qq.com/debug/cgi-bin/sandbox?t=sandbox/login"
	baseURL      = "https://open.weixin.qq.com/connect/qrconnect"
	pollBaseURL  = "https://lp.open.weixin.qq.com/connect/l/qrconnect"
	qrCodeURLFmt = "https://open.weixin.qq.com/connect/qrcode/%s"
)

var (
	reG           = regexp.MustCompile(`var G\s*=\s*"([^"]+)"`)
	reImgSrc      = regexp.MustCompile(`src="/connect/qrcode/([^"]+)"`)
	reErrCode     = regexp.MustCompile(`wx_errcode\s*=\s*(\d+)`)
	reCode        = regexp.MustCompile(`wx_code\s*=\s*['"]([^'"]+)['"]`)
	reMetaRefresh = regexp.MustCompile(`<meta\s+http-equiv="refresh"\s+content="[^"]*url=([^"]+)"`)
	reLocation    = regexp.MustCompile(`window\.location\s*=\s*['"]([^'"]+)['"]`)

	// 提取 appID 和 appsecret
	reAppID       = regexp.MustCompile(`<span class="frm_input_box">(wx[0-9a-f]{16,18})</span>`)
	reAppSecretNew = regexp.MustCompile(`<span class="frm_input_box">([a-f0-9]{32})</span>`)
)

type result struct {
	status string // "success", "cancel", "expired"
	code   string
}

type state struct {
	client      *http.Client
	uuid        string
	resultCh    chan *result
	wg          sync.WaitGroup
	mu          sync.Mutex
	loginResult *result
	stopCh      chan struct{}
}

func main() {
	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Jar:     jar,
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	s := &state{
		client:   client,
		resultCh: make(chan *result, 1),
		stopCh:   make(chan struct{}),
	}

	// 第一步：初始化获取 uuid
	if err := s.init(); err != nil {
		fmt.Printf("初始化失败: %v\n", err)
		return
	}
	fmt.Printf("获取到 uuid: %s\n", s.uuid)

	// 第二步：显示二维码
	qrURL := fmt.Sprintf(qrCodeURLFmt, s.uuid)
	fmt.Printf("\n二维码链接: %s\n", qrURL)
	fmt.Println("请用微信扫描二维码（扫描后等待 5-10 秒）...")

	// 第三步：启动轮询
	s.wg.Add(2)
	go s.pollBase()
	go s.pollLast404()

	res := <-s.resultCh
	close(s.stopCh)
	s.wg.Wait()

	if res.status != "success" {
		fmt.Printf("登录失败，状态: %s\n", res.status)
		return
	}
	fmt.Printf("成功获取授权码: %s\n", res.code)

	// 第四步：获取测试号信息
	appIDFromPage, appSecret, err := fetchTestAccountInfo(s.client, res.code)
	if err != nil {
		fmt.Printf("获取测试号信息失败: %v\n", err)
		return
	}
	fmt.Printf("appid: %s\n", appIDFromPage)
	fmt.Printf("appsecret: %s\n", appSecret)
}

// ==================== 通用重定向处理函数====================
func performRequestWithRedirects(client *http.Client, startURL string, stepName string) ([]byte, error) {
	currentURL := startURL
	maxRedirects := 10

	for i := 0; i < maxRedirects; i++ {
		fmt.Printf("【%s】请求 #%d: %s\n", stepName, i+1, currentURL)

		req, err := http.NewRequest("GET", currentURL, nil)
		if err != nil {
			return nil, fmt.Errorf("创建请求失败: %w", err)
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/134.0.0.0 Safari/537.36")
		req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,image/apng,*/*;q=0.8")
		req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
		req.Header.Set("Connection", "keep-alive")
		req.Header.Set("Upgrade-Insecure-Requests", "1")
		req.Header.Set("Sec-Fetch-Dest", "document")
		req.Header.Set("Sec-Fetch-Mode", "navigate")
		req.Header.Set("Sec-Fetch-Site", "none")
		req.Header.Set("Sec-Fetch-User", "?1")
		req.Header.Set("Cache-Control", "max-age=0")
		req.Header.Set("Referer", "https://open.weixin.qq.com/")

		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("请求失败: %w", err)
		}
		fmt.Printf("【%s】状态码: %d\n", stepName, resp.StatusCode)

		// 处理 3xx 重定向
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			location := resp.Header.Get("Location")
			if location == "" {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				return nil, fmt.Errorf("收到 %d 但无Location头", resp.StatusCode)
			}
			fmt.Printf("【%s】重定向至: %s\n", stepName, location)

			base, err := url.Parse(currentURL)
			if err != nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				return nil, fmt.Errorf("解析当前URL失败: %w", err)
			}
			locationURL, err := base.Parse(location)
			if err != nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				return nil, fmt.Errorf("解析Location失败: %w", err)
			}
			currentURL = locationURL.String()

			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			continue
		}

		// 读取 body
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("读取响应体失败: %w", err)
		}

		// 处理页面内跳转
		if m := reMetaRefresh.FindSubmatch(body); m != nil {
			redirectURL := string(m[1])
			fmt.Printf("【%s】检测到meta refresh跳转: %s\n", stepName, redirectURL)
			base, _ := url.Parse(currentURL)
			redirectAbs, _ := base.Parse(redirectURL)
			currentURL = redirectAbs.String()
			continue
		}
		if m := reLocation.FindSubmatch(body); m != nil {
			redirectURL := string(m[1])
			fmt.Printf("【%s】检测到JavaScript跳转: %s\n", stepName, redirectURL)
			base, _ := url.Parse(currentURL)
			redirectAbs, _ := base.Parse(redirectURL)
			currentURL = redirectAbs.String()
			continue
		}

		// 无跳转，返回最终 body
		return body, nil
	}
	return nil, fmt.Errorf("重定向次数过多")
}

func fetchTestAccountInfo(client *http.Client, code string) (string, string, error) {
	// 第一步：sandboxqrlogion
	qrloginURL := fmt.Sprintf("https://mp.weixin.qq.com/debug/cgi-bin/sandboxqrlogion?code=%s", code)
	fmt.Printf("第一步: 请求 sandboxqrlogion\n")
	if _, err := performRequestWithRedirects(client, qrloginURL, "sandboxqrlogion"); err != nil {
		return "", "", fmt.Errorf("第一步 sandboxqrlogion 失败: %w", err)
	}

	// 第二步：sandbox/login
	callbackURL := fmt.Sprintf("http://mp.weixin.qq.com/debug/cgi-bin/sandbox?t=sandbox/login&code=%s&state=", code)
	fmt.Printf("第二步: 请求 sandbox/login\n")
	body, err := performRequestWithRedirects(client, callbackURL, "sandbox/login")
	if err != nil {
		return "", "", fmt.Errorf("第二步 sandbox/login 失败: %w", err)
	}

	// 提取 appID 和 appsecret
	var appID, appSecret string
	if m := reAppID.FindSubmatch(body); m != nil {
		appID = string(m[1])
	}
	if m := reAppSecretNew.FindSubmatch(body); m != nil {
		appSecret = string(m[1])
	}

	if appID == "" {
		return "", "", fmt.Errorf("未找到 appID")
	}
	if appSecret == "" {
		return "", "", fmt.Errorf("未找到 appsecret")
	}
	return appID, appSecret, nil
}

func (s *state) init() error {
	params := url.Values{}
	params.Set("appid", appID)
	params.Set("scope", scope)
	params.Set("redirect_uri", redirectURI)
	fullURL := baseURL + "?" + params.Encode()

	req, err := http.NewRequest("GET", fullURL, nil)
	if err != nil {
		return fmt.Errorf("创建请求失败: %w", err)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/134.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,image/apng,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Upgrade-Insecure-Requests", "1")
	req.Header.Set("Sec-Fetch-Dest", "document")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-Site", "none")
	req.Header.Set("Sec-Fetch-User", "?1")
	req.Header.Set("Cache-Control", "max-age=0")
	req.Header.Set("Referer", "https://open.weixin.qq.com/")

	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("请求登录页失败: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("读取响应失败: %w", err)
	}

	if m := reG.FindSubmatch(body); m != nil {
		s.uuid = string(m[1])
	} else if m := reImgSrc.FindSubmatch(body); m != nil {
		s.uuid = string(m[1])
	} else {
		return fmt.Errorf("无法从页面中提取 uuid")
	}
	return nil
}

func (s *state) pollBase() {
	defer s.wg.Done()
	for {
		select {
		case <-s.stopCh:
			return
		default:
		}

		params := url.Values{}
		params.Set("uuid", s.uuid)
		params.Set("_", strconv.FormatInt(time.Now().UnixNano()/1e6, 10))
		pollURL := pollBaseURL + "?" + params.Encode()

		req, err := http.NewRequest("GET", pollURL, nil)
		if err != nil {
			fmt.Printf("创建基础轮询请求失败: %v\n", err)
			time.Sleep(2 * time.Second)
			continue
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/134.0.0.0 Safari/537.36")
		req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,image/apng,*/*;q=0.8")
		req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
		req.Header.Set("Connection", "keep-alive")
		req.Header.Set("Upgrade-Insecure-Requests", "1")
		req.Header.Set("Sec-Fetch-Dest", "document")
		req.Header.Set("Sec-Fetch-Mode", "navigate")
		req.Header.Set("Sec-Fetch-Site", "none")
		req.Header.Set("Sec-Fetch-User", "?1")
		req.Header.Set("Cache-Control", "max-age=0")
		req.Header.Set("Referer", "https://open.weixin.qq.com/")

		resp, err := s.client.Do(req)
		if err != nil {
			fmt.Printf("基础轮询请求失败: %v\n", err)
			time.Sleep(2 * time.Second)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		errcode, code := parseResponse(body)
		if errcode == 0 {
			time.Sleep(2 * time.Second)
			continue
		}

		if s.handleResult(errcode, code) {
			return
		}
		time.Sleep(2 * time.Second)
	}
}

func (s *state) pollLast404() {
	defer s.wg.Done()
	for {
		select {
		case <-s.stopCh:
			return
		default:
		}

		params := url.Values{}
		params.Set("uuid", s.uuid)
		params.Set("last", "404")
		params.Set("_", strconv.FormatInt(time.Now().UnixNano()/1e6, 10))
		pollURL := pollBaseURL + "?" + params.Encode()

		req, err := http.NewRequest("GET", pollURL, nil)
		if err != nil {
			fmt.Printf("创建 last404 轮询请求失败: %v\n", err)
			time.Sleep(2 * time.Second)
			continue
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/134.0.0.0 Safari/537.36")
		req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,image/apng,*/*;q=0.8")
		req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
		req.Header.Set("Connection", "keep-alive")
		req.Header.Set("Upgrade-Insecure-Requests", "1")
		req.Header.Set("Sec-Fetch-Dest", "document")
		req.Header.Set("Sec-Fetch-Mode", "navigate")
		req.Header.Set("Sec-Fetch-Site", "none")
		req.Header.Set("Sec-Fetch-User", "?1")
		req.Header.Set("Cache-Control", "max-age=0")
		req.Header.Set("Referer", "https://open.weixin.qq.com/")

		resp, err := s.client.Do(req)
		if err != nil {
			fmt.Printf("last404轮询请求失败: %v\n", err)
			time.Sleep(2 * time.Second)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		errcode, code := parseResponse(body)
		if errcode == 0 {
			time.Sleep(2 * time.Second)
			continue
		}

		if s.handleResult(errcode, code) {
			return
		}
		time.Sleep(2 * time.Second)
	}
}

func (s *state) handleResult(errcode int, code string) bool {
	switch errcode {
	case 405:
		s.setResult(&result{status: "success", code: code})
		return true
	case 403:
		s.setResult(&result{status: "cancel"})
		return true
	case 402:
		s.setResult(&result{status: "expired"})
		return true
	case 404:
	default:
	}
	return false
}

func parseResponse(body []byte) (errcode int, code string) {
	if m := reErrCode.FindSubmatch(body); m != nil {
		errcode, _ = strconv.Atoi(string(m[1]))
	}
	if m := reCode.FindSubmatch(body); m != nil {
		code = string(m[1])
	}
	return
}

func (s *state) setResult(r *result) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loginResult == nil {
		s.loginResult = r
		select {
		case s.resultCh <- r:
		default:
		}
	}
}