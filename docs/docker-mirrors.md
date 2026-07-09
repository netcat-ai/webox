# Docker 镜像源

`webox` 构建依赖两个 Docker Hub base image：

- `rust:1.96-bookworm`
- `debian:bookworm`

默认 Dockerfile 和 Compose 配置使用官方 Docker Hub 和 Debian apt 源，依赖 Docker daemon 的全局代理。

## 项目级覆盖

当前默认 `.env.example`：

```dotenv
RUST_BUILDER_IMAGE=rust:1.96-bookworm
DEBIAN_RUNTIME_IMAGE=debian:bookworm
```

默认不覆盖 Debian apt 源：

```dotenv
APT_DEBIAN_MIRROR=
APT_DEBIAN_SECURITY_MIRROR=
```

国内镜像 fallback：

```dotenv
RUST_BUILDER_IMAGE=docker.m.daocloud.io/library/rust:1.96-bookworm
DEBIAN_RUNTIME_IMAGE=docker.m.daocloud.io/library/debian:bookworm
APT_DEBIAN_MIRROR=http://mirrors.aliyun.com/debian
APT_DEBIAN_SECURITY_MIRROR=http://mirrors.aliyun.com/debian-security
```

agentgateway release 下载源也可以替换：

```dotenv
AGENTGATEWAY_RELEASE_URL_BASE=https://github.com/agentgateway/agentgateway/releases/download
```

如果 GitHub Release 不稳定，把它指向自建缓存或代理，路径规则保持为：

```text
${AGENTGATEWAY_RELEASE_URL_BASE}/${AGENTGATEWAY_VERSION}/agentgateway-linux-${arch}
```

先验证镜像是否可用：

```bash
docker buildx imagetools inspect "$RUST_BUILDER_IMAGE"
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
