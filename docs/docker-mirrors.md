# Docker 镜像源

`webox` 构建依赖两个 Docker Hub base image：

- `golang:1.26-bookworm`
- `debian:bookworm-slim`

默认 Dockerfile 和 Compose 配置使用官方 Docker Hub 和 Debian apt 源，依赖 Docker daemon 的全局代理。

## 项目级覆盖

当前默认 `.env.example`：

```dotenv
GO_BUILDER_IMAGE=golang:1.26-bookworm
DEBIAN_RUNTIME_IMAGE=debian:bookworm-slim
```

默认不覆盖 Debian apt 源：

```dotenv
APT_DEBIAN_MIRROR=
APT_DEBIAN_SECURITY_MIRROR=
```

如果仅 Debian apt 较慢，可以使用阿里云 apt 镜像，同时继续从官方 Docker Registry 拉取基础镜像：

```dotenv
GO_BUILDER_IMAGE=golang:1.26-bookworm
DEBIAN_RUNTIME_IMAGE=debian:bookworm-slim
APT_DEBIAN_MIRROR=http://mirrors.aliyun.com/debian
APT_DEBIAN_SECURITY_MIRROR=http://mirrors.aliyun.com/debian-security
```

项目不通过第三方 registry 反向代理重写基础镜像地址。先验证官方镜像是否可用：

```bash
docker buildx imagetools inspect "$GO_BUILDER_IMAGE"
docker buildx imagetools inspect "$DEBIAN_RUNTIME_IMAGE"
```

## Docker daemon 级加速

阿里云 ACR 的 Docker Hub 加速器属于 daemon 级配置。拿到你的加速器地址后，配置：

```json
{
  "registry-mirrors": ["https://<your-aliyun-accelerator>.mirror.aliyuncs.com"]
}
```

OrbStack 使用：

```bash
orb config docker
orb restart docker
```

Linux Docker Engine 通常编辑 `/etc/docker/daemon.json` 后重启 Docker。

验证：

```bash
docker info --format '{{json .RegistryConfig.Mirrors}}'
```
