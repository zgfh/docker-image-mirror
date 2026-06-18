# Docker Image Mirror

轻量级 Docker 镜像缓存代理，基于 Go 实现。

## 功能

- 🚀 本地缓存 Docker 镜像
- 📦 支持 Docker Registry V2 API
- 💾 流式缓存，边下边存
- 🔄 自动重试机制
- 🏥 健康检查接口

## 快速开始

### 使用 Docker Compose

```bash
make docker-run
```

### 本地开发

```bash
make install-deps
make run
```

## 使用示例

### 推送镜像到代理

```bash
# 标记镜像
docker tag nginx:latest localhost:5000/docker.io/nginx:latest

# 推送（需要修改 daemon.json 允许 localhost:5000）
docker push localhost:5000/docker.io/nginx:latest
```

### 从代理拉取镜像

```bash
docker pull localhost:5000/docker.io/nginx:latest
```

## API 端点

- `GET /v2/` - Docker Registry V2 基础端点
- `GET /v2/{image}/manifests/{reference}` - 获取 Manifest
- `PUT /v2/{image}/manifests/{reference}` - 推送 Manifest
- `GET /v2/{image}/blobs/{digest}` - 获取 Blob
- `GET /health` - 健康检查

## 存储位置

默认存储在 `/var/lib/docker-mirror`

## 许可证

MIT
