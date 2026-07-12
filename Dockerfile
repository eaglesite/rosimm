# ============================================================
# Dockerfile — Vercel Container Registry 部署
# ============================================================
FROM golang:1.25-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /server .

# ============================================================
FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app

COPY --from=builder /server /server
COPY --from=builder /app/login.html .
COPY --from=builder /app/login_admin.html .
COPY --from=builder /app/index.html .
COPY --from=builder /app/admin.html .

# 配置文件（可通过 CONFIG_PATH 环境变量覆盖）
COPY config.json .
ENV CONFIG_PATH=/app/config.json

EXPOSE 80

CMD ["/server"]
