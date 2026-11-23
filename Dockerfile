FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -o /ci-cd ./cmd/server

# 테스트 단계 — `docker build --target test .`. git 조회 테스트는 실제 git 저장소를 만들어 돈다(git 이 없으면 실패한다).
FROM build AS test
RUN apk add --no-cache git
RUN go vet ./... && go test ./...

FROM docker:27-cli
RUN apk add --no-cache git && git config --global --add safe.directory '*'
# safe.directory: 에이전트(root)가 호스트 사용자 소유 clone을 다룬다 — 이 컨테이너는 통제된 배포 전용
# docker CLI + compose 플러그인 포함 이미지 — agent가 형제 compose를 제어(docker.sock 마운트)
COPY --from=build /ci-cd /usr/local/bin/ci-cd
ENTRYPOINT ["ci-cd"]
