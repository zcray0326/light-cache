# 多阶段构建:builder 阶段编译,运行镜像只放二进制,体积小
FROM golang:1.25 AS builder

WORKDIR /app
# 先拷依赖(利用 docker 层缓存:go.mod 不变则跳过 go mod download)
COPY go.mod go.sum ./
RUN go mod download

# 拷源码并编译
COPY . .
RUN CGO_ENABLED=0 go build -o /light-cache ./cmd/server

# 运行镜像:alpine + 二进制
FROM alpine:latest
RUN apk --no-cache add ca-certificates
COPY --from=builder /light-cache /light-cache

EXPOSE 8001 9999
ENTRYPOINT ["/light-cache"]
CMD ["-port=8001"]

