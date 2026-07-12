# Vercel 容器部署指南

## 环境变量

| 变量名 | 默认值 | 说明 |
|--------|--------|------|
| `PORT` | `80` | HTTP 监听端口（Vercel 自动注入，无需手动设置） |
| `DATA_DIR` | `.` | 图片存储目录（Vercel 建议设为 `/tmp/images`） |
| `CONFIG_PATH` | `/app/config.json` | 配置文件路径（Dockerfile 中已预设） |

### 非持久化说明

Vercel 容器文件系统是**临时只读**的（`/tmp` 除外）。以下行为会受影响：

- **图片存储**：设置 `DATA_DIR=/tmp/images` 可正常存储，但容器重启后丢失
- **配置修改**（密码/定时/请求配置）：管理页面的修改操作会尝试写 `config.json`，失败时会打印日志但不影响运行
- **图片持久化**：如需永久存储需接入外部对象存储（如 R2/S3），暂未实现

## 部署步骤

### 1. 准备 Vercel 项目

```bash
# 安装 Vercel CLI
npm install -g vercel

# 登录
vercel login

# 创建项目（如果未创建）
vercel project create rosi-spider
```

### 2. 配置 Vercel Registry

使用 Vercel Container Registry 托管 Docker 镜像：

```bash
# 登录 VCR
vercel cr login

# 创建 registry
vercel cr create rosi-spider

# 构建并推送
docker build -t rosi-spider -f Dockerfile.vercel .
docker tag rosi-spider cr.vercel.app/rosi-spider/rosi-spider:latest
docker push cr.vercel.app/rosi-spider/rosi-spider:latest
```

### 3. 部署

```bash
# 在项目根目录创建 vercel.json
cat > vercel.json << 'EOF'
{
  "builds": [
    {
      "src": "Dockerfile.vercel",
      "use": "@vercel/docker"
    }
  ],
  "routes": [
    { "src": "/(.*)", "dest": "/" }
  ]
}
EOF

# 部署
vercel deploy --prod
```

### 4. 设置环境变量（Vercel Dashboard）

在 Vercel 项目 Settings → Environment Variables 中添加：

| 变量名 | 值 | 环境 |
|--------|----|------|
| `DATA_DIR` | `/tmp/images` | Production |
| `CONFIG_PATH` | `/app/config.json` | Production |

### 5. 首次使用

1. 访问 `https://your-domain.vercel.app/login`，密码 `123456`（前端）
2. 访问 `https://your-domain.vercel.app/login_admin`，密码 `admin`（管理端）
3. 在管理面板 → 定时任务中启用每日定时爬取，或手动启动爬虫

## 本地开发

```bash
# 直接运行
set DATA_DIR=./images && set PORT=8080 && go run .
```

## 构建可执行文件

```bash
# Linux 静态编译
$env:GOOS="linux"; $env:GOARCH="amd64"; $env:CGO_ENABLED="0"
go build -ldflags="-s -w" -o rosi_linux .
```
