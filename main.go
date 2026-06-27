package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/registry"
)

// upstreamMap 将请求路径中的前缀映射到真实的 registry 地址
var upstreamMap = map[string]string{
	"docker.io": "registry-1.docker.io",
	"gcr.io":    "gcr.io",
	"quay.io":   "quay.io",
	"ghcr.io":   "ghcr.io",
}

// Proxy 结构体保存代理配置和状态
type Proxy struct {
	cacheURL       string // 本地或外部缓存 registry 地址，例如 http://127.0.0.1:5001
	client         *http.Client
	upstreamClient *http.Client // 访问上游 registry 的客户端（可配置代理）
	mu             sync.Mutex
	tokens         map[string]string // docker.io 的 token 缓存
}

func main() {
	cacheAddr := ":5001"
	proxyAddr := os.Getenv("PROXY_ADDR")
	if proxyAddr == "" {
		proxyAddr = ":5000"
	}

	// 读取外部缓存镜像仓库地址（如 registry.internal:5000）
	externalCache := os.Getenv("CACHE_REGISTRY")

	var cacheURL string

	if externalCache != "" {
		// 使用外部部署的缓存 registry
		cacheURL = "http://" + externalCache
		log.Printf("Using external cache registry: %s\n", cacheURL)
	} else {
		// 1. 启动内嵌的内存型 cache registry
		go func() {
			log.Printf("Cache registry listening on %s\n", cacheAddr)
			srv := &http.Server{
				Addr:    cacheAddr,
				Handler: registry.New(),
			}
			if err := srv.ListenAndServe(); err != nil {
				log.Fatalf("cache registry failed: %v", err)
			}
		}()

		// 等待 cache registry 启动
		time.Sleep(500 * time.Millisecond)
		cacheURL = "http://127.0.0.1" + cacheAddr
	}

	// 2. 配置上游代理
	mirrorProxy := os.Getenv("MIRROR_PROXY")
	mirrorNoProxy := os.Getenv("MIRROR_NO_PROXY")

	var proxyFunc func(*http.Request) (*url.URL, error)
	if mirrorProxy != "" {
		proxyURL, err := url.Parse(mirrorProxy)
		if err != nil {
			log.Fatalf("Invalid MIRROR_PROXY: %v", err)
		}
		noProxyList := parseNoProxy(mirrorNoProxy)

		proxyFunc = func(req *http.Request) (*url.URL, error) {
			host := req.URL.Hostname()
			for _, np := range noProxyList {
				if matchNoProxy(host, np) {
					log.Printf("Bypass proxy for %s (matches NO_PROXY: %s)\n", host, np)
					return nil, nil // 直连
				}
			}
			log.Printf("Using proxy %s for %s\n", proxyURL.Host, host)
			return proxyURL, nil
		}
		log.Printf("Mirror proxy enabled: %s (no_proxy: %s)\n", proxyURL.Host, mirrorNoProxy)
	}

	// 3. 启动代理服务器
	cacheTr := &http.Transport{
		DisableCompression: true, // 必须禁用自动解压，保证流式二进制透传
	}
	upstreamTr := &http.Transport{
		DisableCompression: true,
		Proxy:              proxyFunc,
	}
	p := &Proxy{
		cacheURL:       cacheURL,
		client:         &http.Client{Transport: cacheTr},
		upstreamClient: &http.Client{Transport: upstreamTr},
		tokens:         make(map[string]string),
	}

	log.Printf("Docker image proxy listening on %s\n", proxyAddr)
	log.Fatal(http.ListenAndServe(proxyAddr, p))
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 处理 /v2/ 探活请求
	if r.URL.Path == "/v2/" || r.URL.Path == "/v2" {
		w.Header().Set("Docker-Distribution-API-Version", "registry/2.0")
		w.WriteHeader(http.StatusOK)
		log.Printf("Probe /v2/ from %s\n", r.RemoteAddr)
		return
	}

	// 解析路径: /v2/<prefix>/<repo>/manifests/<ref> 或 /v2/<prefix>/<repo>/blobs/<digest>
	prefix, repo, kind, ref, ok := parseV2Path(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}

	// 1. 尝试从缓存获取
	if p.serveFromCache(w, r, repo, kind, ref) {
		log.Printf("Served %s/%s from cache\n", repo, ref)
		return
	}
	log.Printf("Cache miss for %s/%s, fetching from upstream\n", repo, ref)

	// 2. 缓存未命中，从上游拉取
	upstream, ok := upstreamMap[prefix]
	if !ok {
		upstream = prefix
	}

	log.Printf("Fetching %s/%s from upstream\n", repo, ref)
	p.serveFromUpstream(w, r, upstream, repo, kind, ref)
}

// parseV2Path 解析 OCI Distribution Spec 的路径
func parseV2Path(path string) (prefix, repo, kind, ref string, ok bool) {
	p := strings.TrimPrefix(path, "/v2/")
	idx := strings.Index(p, "/")
	if idx == -1 {
		log.Printf("Invalid path format: %s\n", path)
		return "", "", "", "", false
	}
	prefix = p[:idx]
	rest := p[idx+1:]

	if i := strings.Index(rest, "/manifests/"); i >= 0 {
		repo = rest[:i]
		ref = rest[i+len("/manifests/"):]
		kind = "manifests"
		log.Printf("Parsed path: prefix=%s, repo=%s, kind=%s, ref=%s\n", prefix, repo, kind, ref)
		return prefix, repo, kind, ref, true
	}
	if i := strings.Index(rest, "/blobs/"); i >= 0 {
		repo = rest[:i]
		ref = rest[i+len("/blobs/"):]
		kind = "blobs"
		log.Printf("Parsed path: prefix=%s, repo=%s, kind=%s, ref=%s\n", prefix, repo, kind, ref)
		return prefix, repo, kind, ref, true
	}
	log.Printf("Failed to parse path: %s\n", path)
	return "", "", "", "", false
}

// serveFromCache 尝试从本地或外部缓存 registry 获取数据
func (p *Proxy) serveFromCache(w http.ResponseWriter, r *http.Request, repo, kind, ref string) bool {
	cachePath := fmt.Sprintf("%s/v2/%s/%s/%s", p.cacheURL, repo, kind, ref)
	log.Printf("Checking cache for %s/%s\n", repo, ref)
	req, _ := http.NewRequest(r.Method, cachePath, nil)
	req.Header = r.Header.Clone()
	req.Header.Del("Authorization") // 无账号密码的缓存仓库不需要认证

	resp, err := p.client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		log.Printf("Cache check failed for %s/%s: %v\n", repo, ref, err)
		if resp != nil {
			resp.Body.Close()
		}
		log.Printf("Cache miss for %s/%s\n", repo, ref)
		return false
	}
	log.Printf("Cache hit for %s/%s\n", repo, ref)
	defer resp.Body.Close()

	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
	return true
}

// serveFromUpstream 从上游拉取数据，流式返回客户端，同时推送到缓存
func (p *Proxy) serveFromUpstream(w http.ResponseWriter, r *http.Request, upstream, repo, kind, ref string) {
	upstreamURL := fmt.Sprintf("https://%s/v2/%s/%s/%s", upstream, repo, kind, ref)

	userAgent := r.Header.Get("User-Agent")
	if userAgent == "" {
		userAgent = "docker/20.10.0"
	}

	req, _ := http.NewRequest(r.Method, upstreamURL, nil)
	req.Header.Set("Accept", r.Header.Get("Accept"))
	req.Header.Set("User-Agent", userAgent)

	resp, err := p.upstreamClient.Do(req)
	if err != nil {
		http.Error(w, "upstream request failed: "+err.Error(), http.StatusBadGateway)
		return
	}

	if resp.StatusCode == http.StatusUnauthorized {
		wwwAuth := resp.Header.Get("WWW-Authenticate")
		resp.Body.Close()

		if wwwAuth != "" {
			token := p.fetchToken(wwwAuth, repo)
			if token != "" {
				req, _ = http.NewRequest(r.Method, upstreamURL, nil)
				req.Header.Set("Accept", r.Header.Get("Accept"))
				req.Header.Set("User-Agent", userAgent)
				req.Header.Set("Authorization", "Bearer "+token)

				resp, err = p.upstreamClient.Do(req)
				if err != nil {
					http.Error(w, "upstream retry failed: "+err.Error(), http.StatusBadGateway)
					return
				}
			} else {
				http.Error(w, "failed to fetch auth token", http.StatusUnauthorized)
				return
			}
		} else {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		copyHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
		return
	}

	if r.Method == http.MethodHead {
		copyHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		return
	}

	if kind == "manifests" {
		p.handleManifestStream(w, resp, repo, ref)
	} else if kind == "blobs" {
		p.handleBlobStream(w, resp, repo, ref)
	}
}

// fetchToken 解析 WWW-Authenticate 头并获取 Bearer Token
func (p *Proxy) fetchToken(wwwAuth, repo string) string {
	parts := strings.TrimPrefix(wwwAuth, "Bearer ")
	params := map[string]string{}
	for _, p := range strings.Split(parts, ",") {
		if idx := strings.Index(p, "="); idx != -1 {
			key := strings.TrimSpace(p[:idx])
			val := strings.Trim(p[idx+1:], "\"")
			params[key] = val
		}
	}

	realm := params["realm"]
	if realm == "" {
		return ""
	}

	q := url.Values{}
	if service, ok := params["service"]; ok {
		q.Set("service", service)
	}
	if scope, ok := params["scope"]; ok {
		q.Set("scope", scope)
	} else {
		q.Set("scope", fmt.Sprintf("repository:%s:pull", repo))
	}

	tokenURL := realm + "?" + q.Encode()
	resp, err := p.upstreamClient.Get(tokenURL)
	if err != nil {
		log.Printf("fetch token failed: %v", err)
		return ""
	}
	defer resp.Body.Close()

	var data struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return ""
	}
	return data.Token
}

// handleManifestStream 处理 Manifest 请求
func (p *Proxy) handleManifestStream(w http.ResponseWriter, resp *http.Response, repo, ref string) {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		http.Error(w, "read manifest failed", http.StatusInternalServerError)
		return
	}

	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(http.StatusOK)
	w.Write(body)
	log.Printf("Served manifest %s/%s, size %d bytes\n", repo, ref, len(body))

	contentType := resp.Header.Get("Content-Type")
	log.Printf("Caching manifest %s/%s to cache\n", repo, ref)
	go p.cacheManifest(repo, ref, contentType, body)
}

// handleBlobStream 处理 Blob 请求
func (p *Proxy) handleBlobStream(w http.ResponseWriter, resp *http.Response, repo, digest string) {
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(http.StatusOK)

	pr, pw := io.Pipe()

	go func() {
		defer pr.Close()
		p.uploadBlobToCache(repo, digest, pr)
	}()

	mw := io.MultiWriter(w, pw)
	io.Copy(mw, resp.Body)

	pw.Close()
}

// cacheManifest 上传 manifest 到缓存仓库
func (p *Proxy) cacheManifest(repo, ref, contentType string, body []byte) {
	putURL := fmt.Sprintf("%s/v2/%s/manifests/%s", p.cacheURL, repo, ref)
	log.Printf("Putting manifest %s/%s to cache\n", repo, ref)
	req, _ := http.NewRequest("PUT", putURL, bytes.NewReader(body))
	if contentType == "" {
		contentType = "application/vnd.docker.distribution.manifest.v2+json"
	}
	req.Header.Set("Content-Type", contentType)

	resp, err := p.client.Do(req)
	if err != nil {
		log.Printf("cache manifest failed: %v", err)
		return
	}
	resp.Body.Close()
	log.Printf("Cached manifest %s/%s to cache, status: %d\n", repo, ref, resp.StatusCode)
}

// uploadBlobToCache 流式上传 Blob 到缓存仓库
func (p *Proxy) uploadBlobToCache(repo, digest string, body io.Reader) {
	postURL := fmt.Sprintf("%s/v2/%s/blobs/uploads/", p.cacheURL, repo)
	log.Printf("Initiating blob upload for %s/%s to cache\n", repo, digest)
	resp, err := p.client.Post(postURL, "", nil)
	if err != nil {
		log.Printf("cache blob init failed: %v", err)
		return
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		log.Printf("cache blob init bad status: %d", resp.StatusCode)
		return
	}

	location := resp.Header.Get("Location")
	if !strings.HasPrefix(location, "http") {
		location = p.cacheURL + location
	}
	log.Printf("Initiated blob upload for %s/%s, location: %s\n", repo, digest, location)

	patchReq, _ := http.NewRequest("PATCH", location, body)
	patchReq.Header.Set("Content-Type", "application/octet-stream")
	patchResp, err := p.client.Do(patchReq)
	if err != nil {
		log.Printf("cache blob patch failed: %v", err)
		return
	}
	patchResp.Body.Close()

	if patchResp.StatusCode != http.StatusAccepted {
		log.Printf("cache blob patch bad status: %d", patchResp.StatusCode)
		return
	}

	nextLocation := patchResp.Header.Get("Location")
	if nextLocation == "" {
		nextLocation = location
	}
	if !strings.HasPrefix(nextLocation, "http") {
		nextLocation = p.cacheURL + nextLocation
	}

	finalURL := nextLocation
	if strings.Contains(finalURL, "?") {
		finalURL += "&digest=" + digest
	} else {
		finalURL += "?digest=" + digest
	}

	putReq, _ := http.NewRequest("PUT", finalURL, nil)
	putResp, err := p.client.Do(putReq)
	if err != nil {
		log.Printf("cache blob put failed: %v", err)
		return
	}
	putResp.Body.Close()
	log.Printf("Cached blob %s/%s to cache, status: %d\n", repo, digest, putResp.StatusCode)
}

// parseNoProxy 解析逗号分隔的 NO_PROXY 列表
func parseNoProxy(noProxy string) []string {
	if noProxy == "" {
		return nil
	}
	items := strings.Split(noProxy, ",")
	result := make([]string, 0, len(items))
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item != "" {
			result = append(result, item)
		}
	}
	return result
}

// matchNoProxy 检查 host 是否匹配 no_proxy 规则
// 规则：
//   - ".example.com" 匹配所有 *.example.com 的子域名
//   - "example.com" 精确匹配 example.com
func matchNoProxy(host, pattern string) bool {
	host = strings.TrimSuffix(host, ".")
	pattern = strings.TrimSuffix(pattern, ".")
	if strings.HasPrefix(pattern, ".") {
		return strings.HasSuffix(host, pattern) || host == pattern[1:]
	}
	return host == pattern
}

// copyHeaders 安全地复制 HTTP 头
func copyHeaders(dst, src http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
	dst.Del("Transfer-Encoding")
}
