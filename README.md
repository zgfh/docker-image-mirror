# Docker Image Mirror
一个轻量级、高性能的 Docker 镜像代理与缓存服务。使用 Go 编写，单文件运行，无需额外部署重量级的 Registry 服务。
支持通过多层级路径前缀将请求路由到任意上游 Registry（如 `docker.io`、`gcr.io`、`m.daocloud.io` 等），并提供流式透传与本地缓存加速。
## ✨ 特性
- **多前缀动态路由**：不仅支持内置映射，还能将任意前缀直接作为上游域名使用。
- **多层级镜像路径**：完美支持 `prefix/namespace/repo/subrepo/image:tag` 这种多斜杠的复杂路径结构。
- **流式透传与缓存**：客户端请求未命中缓存时，边从上游下载边流式返回给客户端，同时利用 `io.Pipe` 异步推送到本地缓存，内存零膨胀。
- **内置缓存仓库**：基于 `go-containerregistry` 内嵌了一个标准 OCI Registry，开箱即用，无需额外部署缓存服务。
- **自动认证处理**：自动解析 `WWW-Authenticate` 响应头，匿名获取 Bearer Token。完美兼容 Docker Hub、GCR、DaoCloud 等各类需要挑战认证的 Registry。
## 🚀 架构原理
```text
            ┌──────────────────────────────────────────────────┐
docker pull │  proxy.com/m.daocloud.io/docker.io/library/nginx │
            └──────────────────────────────────────────────────┘
                                  │
                                  ▼
        ┌─────────────────────────────────────────────────┐
        │            Proxy Server (Go HTTP)               │
        │  1. 解析路径：prefix=m.daocloud.io, repo=.../nginx│
        │  2. 检查本地 cache registry                      │
        │  3. HIT  → 直接返回缓存数据                      │
        │  4. MISS → 请求 upstream                        │
        │     ├─ 处理 401 自动获取 Token                   │
        │     ├─ stream 给客户端 (io.MultiWriter)          │
        │     └─ tee 推送到 cache (chunked upload)         │
        └─────────────────────────────────────────────────┘
                  │                            │
                  ▼                            ▼
        ┌─────────────────┐         ┌──────────────────┐
        │  Cache Registry │         │ Upstream         │
        │  (本地, embedded)│         │ m.daocloud.io /  │
        │  :5001 (rw)     │         │ registry-1...    │
        └─────────────────┘         └──────────────────┘
```
## 📦 快速开始
### 1. 编译与运行
```bash
git clone git@github.com:zgfh/docker-image-mirror.git
cd docker-image-mirror
go build -o docker-image-mirror
# 运行代理服务
./docker-image-mirror

# 或docker方式
# 运行容器
docker run -d --name docker-image-mirror -p 5000:5000 ghcr.io/zgfh/docker-image-mirror:latest
```
默认情况下：
- 代理服务监听在 `:5000` 端口。
- 内置缓存 Registry 监听在 `:5001` 端口。
### 2. 配置 Docker / Podman 客户端
由于本代理默认使用 HTTP 协议，你需要配置容器运行时允许使用 HTTP 拉取。
**Docker (`/etc/docker/daemon.json`)：**
```json
{
  "insecure-registries": ["192.168.1.104:5000"]
}
```
修改后重启 Docker：`sudo systemctl restart docker`
**Podman：**
无需修改配置文件，直接在命令中带上 `--tls-verify=false` 即可。
## 🐳 使用方法
你可以直接在镜像地址前加上代理的地址，并保留原有的多层级路径：
```bash
# 1. 通过 DaoCloud 加速器拉取 Docker Hub 的 alpine
docker pull 192.168.1.104:5000/m.daocloud.io/docker.io/library/alpine:latest
# 2. 直接拉取 Docker Hub 官方镜像 (自动处理 Token 认证)
docker pull 192.168.1.104:5000/docker.io/library/nginx:latest
# 3. 拉取 Google Container Registry 镜像
docker pull 192.168.1.104:5000/gcr.io/google-containers/pause:3.5
# 4. 拉取阿里云代理的镜像
docker pull 192.168.1.104:5000/registry.cn-hangzhou.aliyuncs.com/google_containers/pause:3.5
```
**缓存验证**：
当你第一次拉取某个镜像时，观察代理程序日志会显示从上游拉取并推送到缓存的过程。
当你第二次拉取相同镜像时，日志会显示 `Cache hit`，且拉取速度极快。
## ⚙️ 高级说明
### 路径解析规则
代理程序严格遵循 OCI Distribution Spec 解析路径：
- 截取 URL 第一段作为 `prefix`（用于决定上游地址）。
- 剩余部分作为 `repo` 原封不动透传给上游和本地缓存。
例如请求路径 `/v2/m.daocloud.io/docker.io/library/alpine/manifests/latest`：
- `prefix` = `m.daocloud.io`
- `repo` = `docker.io/library/alpine`
- 最终向上游发起请求：`https://m.daocloud.io/v2/docker.io/library/alpine/manifests/latest`
### 内置映射表
代码中内置了一个简单的映射表，你可以根据需要自行修改：
```go
var upstreamMap = map[string]string{
	"docker.io": "registry-1.docker.io",
	"gcr.io":    "gcr.io",
	"quay.io":   "quay.io",
	"ghcr.io":   "ghcr.io",
}
```
如果请求的 `prefix` 不在映射表中（例如 `m.daocloud.io`），代理会自动将 `prefix` 本身作为上游的域名进行请求。
## 📄 License
MIT License
