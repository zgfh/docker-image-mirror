package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
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
	cacheURL string // 本地缓存 registry 地址，例如 http://127.0.0.1:5001
	client   *http.Client
	mu       sync.Mutex
	tokens   map[string]string // docker.io 的 token 缓存
}

func main() {
	cacheAddr := ":5001"
	proxyAddr := ":5000"

	// 1. 启动内嵌的 cache registry (基于 go-containerregistry)
	go func() {
		log.Printf("Cache registry listening on %s\n", cacheAddr)
		// registry.New() 提供一个标准的、内存级别的 Registry 实现
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

	// 2. 启动代理服务器
	tr := &http.Transport{
		DisableCompression: true, // 必须禁用自动解压，保证流式二进制透传
	}
	p := &Proxy{
		cacheURL: "http://127.0.0.1" + cacheAddr,
		client:   &http.Client{Transport: tr},
		tokens:   make(map[string]string),
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
	// 如果在内置映射表中，使用映射的真实域名；否则直接把 prefix 当作域名使用
	upstream, ok := upstreamMap[prefix]
	if !ok {
		// 默认将 prefix 当作上游域名 (适用于 m.daocloud.io 等自定义加速器)
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

// serveFromCache 尝试从本地缓存 registry 获取数据
func (p *Proxy) serveFromCache(w http.ResponseWriter, r *http.Request, repo, kind, ref string) bool {
	cachePath := fmt.Sprintf("%s/v2/%s/%s/%s", p.cacheURL, repo, kind, ref)
	log.Printf("Checking cache for %s/%s\n", repo, ref)
	req, _ := http.NewRequest(r.Method, cachePath, nil)
	req.Header = r.Header.Clone()
	req.Header.Del("Authorization") // 本地缓存不需要认证

	resp, err := p.client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		log.Printf("Cache check failed for %s/%s: %v\n", repo, ref, err)
		if resp != nil {
			resp.Body.Close()
		}
		log.Printf("Cache miss for %s/%s\n", repo, ref)
		return false // 缓存未命中或出错
	}
	log.Printf("Cache hit for %s/%s\n", repo, ref)
	defer resp.Body.Close()

	// 缓存命中，直接透传响应
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
	return true
}

// serveFromUpstream 从上游拉取数据，流式返回客户端，同时推送到缓存
func (p *Proxy) serveFromUpstream(w http.ResponseWriter, r *http.Request, upstream, repo, kind, ref string) {
	upstreamURL := fmt.Sprintf("https://%s/v2/%s/%s/%s", upstream, repo, kind, ref)
	req, _ := http.NewRequest(r.Method, upstreamURL, nil)

	// 传递 Accept 头
	if accept := r.Header.Get("Accept"); accept != "" {
		req.Header.Set("Accept", accept)
	}

	// 处理 Docker Hub 的认证
	if upstream == "registry-1.docker.io" {
		token := p.getDockerHubToken(repo)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	}
	log.Printf("Requesting upstream %s/%s\n", repo, ref)
	resp, err := p.client.Do(req)
	if err != nil {
		http.Error(w, "upstream request failed: "+err.Error(), http.StatusBadGateway)
		log.Printf("Upstream request failed for %s/%s: %v\n", repo, ref, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		copyHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
		log.Printf("Upstream returned non-200 for %s/%s: %d\n", repo, ref, resp.StatusCode)
		return
	}

	// 如果是 HEAD 请求，不涉及实际数据下载，直接返回 Header
	if r.Method == http.MethodHead {
		copyHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		log.Printf("Served HEAD request for %s/%s\n", repo, ref)
		return
	}

	if kind == "manifests" {
		p.handleManifestStream(w, resp, repo, ref)
	} else if kind == "blobs" {
		p.handleBlobStream(w, resp, repo, ref)
	}
}

// handleManifestStream 处理 Manifest 请求（通常较小，直接全读入内存）
func (p *Proxy) handleManifestStream(w http.ResponseWriter, resp *http.Response, repo, ref string) {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		http.Error(w, "read manifest failed", http.StatusInternalServerError)
		return
	}

	// 1. 返回给客户端
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(http.StatusOK)
	w.Write(body)
	log.Printf("Served manifest %s/%s, size %d bytes\n", repo, ref, len(body))

	// 2. 异步推送到 cache
	contentType := resp.Header.Get("Content-Type")
	log.Printf("Caching manifest %s/%s to cache\n", repo, ref)
	go p.cacheManifest(repo, ref, contentType, body)
}

// handleBlobStream 处理 Blob (镜像层) 请求，核心流式 Tee 逻辑
func (p *Proxy) handleBlobStream(w http.ResponseWriter, resp *http.Response, repo, digest string) {
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(http.StatusOK)

	// 使用 io.Pipe 实现流式 tee
	pr, pw := io.Pipe()

	// 后台 goroutine：从管道读取数据并上传到缓存
	go func() {
		defer pr.Close()
		p.uploadBlobToCache(repo, digest, pr)
	}()

	// 同时写入 HTTP Response (客户端) 和 Pipe Writer (缓存上传)
	mw := io.MultiWriter(w, pw)
	io.Copy(mw, resp.Body)

	// 数据读取完毕，关闭 writer 通知缓存上传完成
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
	// 1. 初始化上传会话
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

	// 2. PATCH 流式数据
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

	// 3. PUT 完成上传
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

// getDockerHubToken 获取 Docker Hub 的匿名 Bearer Token
func (p *Proxy) getDockerHubToken(repo string) string {
	p.mu.Lock()
	defer p.mu.Unlock()

	// 简单缓存
	if t, ok := p.tokens[repo]; ok {
		log.Printf("Using cached token for %s\n", repo)
		return t
	}

	// docker.io 需要自动补 library/ 前缀
	scopeRepo := repo
	if !strings.Contains(repo, "/") {
		scopeRepo = "library/" + repo
	}

	authURL := fmt.Sprintf("https://auth.docker.io/token?service=registry.docker.io&scope=repository:%s:pull", scopeRepo)
	resp, err := p.client.Get(authURL)
	if err != nil {
		log.Printf("Failed to get Docker Hub token for %s: %v\n", repo, err)
		return ""
	}
	defer resp.Body.Close()

	var data struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return ""
	}

	p.tokens[repo] = data.Token
	return data.Token
}

// copyHeaders 安全地复制 HTTP 头
func copyHeaders(dst, src http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
	// 移除安全相关头，避免影响本地代理
	dst.Del("Transfer-Encoding")
}
