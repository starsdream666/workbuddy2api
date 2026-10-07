# syntax=docker/dockerfile:1
FROM golang:1.23-alpine AS build
# 构建期网络（拉基础镜像 / go mod download）在受限网络下走本地代理：
#   BUILD_PROXY=http://host.docker.internal:7890 docker compose build
# 注意容器内的 127.0.0.1 不是宿主机，必须用 host.docker.internal。
# 未传参时为空 = 直连（默认行为不变）。
ARG HTTP_PROXY
ARG HTTPS_PROXY
ARG NO_PROXY
# Go module 代理可单独换国内源（同样由 build arg 传入，为空 = 官方默认）。
ARG GOPROXY
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# 一次编译全部二进制（工具进镜像，容器内可直接跑脚本）。全部 -trimpath -s -w。
# importauth：复用 WorkBuddy 桌面端已登录凭证（realm=workbuddy）→ auths/，需把宿主机凭证文件只读挂进容器。
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/wb2api ./cmd/server \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/signin_bin ./cmd/signin \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/login ./cmd/login \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/credit ./cmd/credit \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/importauth ./cmd/importauth

FROM alpine:3.20
# python3：login.sh 的 JSON 解析 / 签到 / 落盘；bash：shell 脚本体。
RUN apk add --no-cache wget ca-certificates tzdata python3 bash \
 && adduser -D -u 10001 app \
 && mkdir -p /app/auths /app/data \
 && chown -R app:app /app
WORKDIR /app
# 脚本置入 + 去 CRLF（Windows 检出可能性）在切到 app 之前以 root 完成——
# app 对 root 所有文件无写权限，sed -i 需要写权限。
COPY --from=build /out/wb2api /app/wb2api
COPY --from=build /out/signin_bin /app/signin_bin
COPY --from=build /out/login /app/login
COPY --from=build /out/credit /app/credit
COPY --from=build /out/importauth /app/importauth
COPY login.sh signin.sh credit.sh /app/
COPY scripts/probe_active.py /app/scripts/probe_active.py
RUN sed -i 's/\r$//' /app/login.sh /app/signin.sh /app/credit.sh && chmod 755 /app/login.sh /app/signin.sh /app/credit.sh
# 镜像不带真实配置：落 example 作为默认（生产由挂载卷 /app/config.json 覆盖）
# realm 说明：cn（默认）= Copilot/CodeBuddy CN；workbuddy = WorkBuddy AI（www.workbuddy.ai，旧名 ai 仍兼容）；
# codebuddy = www.codebuddy.ai（与 workbuddy 共用账号池，只换出站档案）。
# 账号按 auths/*.json 里的 realm 字段自动分池，两条线 token 不互通。
COPY config.example.json /app/config.json
COPY LICENSE NOTICE /app/
USER app
EXPOSE 7863
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s \
  CMD wget -qO- http://127.0.0.1:7863/healthz || exit 1
ENTRYPOINT ["/app/wb2api", "-config", "/app/config.json"]
