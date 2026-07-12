# Vercel 容器部署指南

## 环境变量

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `PORT` | `80` | 监听端口 |
| `DATA_DIR` | `.` | 图片存储目录（Vercel `/tmp/images`） |
| `CONFIG_PATH` | `/app/config.json` | 配置文件路径 |

## 部署

```bash
# 构建镜像
docker build -f Dockerfile.vercel -t rosimm .

# 推送 VCR
docker tag rosimm vcr.vercel.com/your-project/dockerfile:latest
docker push vcr.vercel.com/your-project/dockerfile:latest

# 或直接 Vercel 网页部署（选择 Dockerfile.vercel）
```

## 本地运行

```powershell
$env:DATA_DIR="./images"; $env:PORT="8080"; go run .
```

## 跨平台编译

```powershell
$env:GOOS="linux"; $env:GOARCH="amd64"; $env:CGO_ENABLED="0"
go build -ldflags="-s -w" -o rosi_linux .
```
