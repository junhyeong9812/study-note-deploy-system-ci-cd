FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -o /ci-cd ./cmd/server

FROM docker:27-cli
RUN apk add --no-cache git
# docker CLI + compose 플러그인 포함 이미지 — agent가 형제 compose를 제어(docker.sock 마운트)
COPY --from=build /ci-cd /usr/local/bin/ci-cd
ENTRYPOINT ["ci-cd"]
