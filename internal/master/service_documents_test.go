package master

// 배포 대상의 목록 — 코드 · 설정 예시 · compose · README 가 같은 목록을 말한다. 기준: requirement-spec.md(2026-11-25 claude-fallback) §10 끝.
//
// 정한 계약:
//   - docker-compose.yml 의 agent 가 붙이는 저장소(`${HOME}/study-note-deploy-system-<이름>`)와 .env.example 의 `AGENT_URL_<이름>` 의
//     이름 집합이 같고, 그 이름은 모두 master 가 받는 서비스다(claude 포함).
//   - .env.example 의 claude 묶음: AGENT_URL_CLAUDE 는 AGENT_URL_BACKEND 와 같은 서버의 자리 표시 · DEPLOY_DIR_CLAUDE=${HOME}/study-note-deploy-system-claude ·
//     DEPLOY_SVC_CLAUDE=claude-runner · DEPLOY_HEALTH_CLAUDE 는 15200 의 /health 이고 127.0.0.1 이 아니다(실행 자리는 LAN 주소에만 연다) ·
//     DEPLOY_MODE_CLAUDE 는 없다(단일 컨테이너).
//   - README 가 claude 를 배포 대상으로 적는다.
//   - 이 시험은 저장소의 세 파일을 읽는다 — Dockerfile 의 test 단계가 그 셋을 /src 에 복사해야 한다.

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func repositoryFile(t *testing.T, name string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join("..", "..", name))
	if err != nil {
		t.Fatalf("저장소의 %s 를 읽지 못했다(Dockerfile 의 test 단계가 복사해야 한다): %v", name, err)
	}
	return string(content)
}

var (
	mountedRepository = regexp.MustCompile(`\$\{HOME\}/study-note-deploy-system-([a-z]+):`)
	agentURLName      = regexp.MustCompile(`(?m)^#?AGENT_URL_([A-Z]+)=`)
	envLine           = func(name string) *regexp.Regexp { return regexp.MustCompile(`(?m)^#?` + name + `=([^#\n]*)`) } // 값은 줄 끝의 # 설명 앞까지(자리 표시에 공백이 든다)
)

func nameSet(matches [][]string, lower bool) []string {
	seen := map[string]bool{}
	for _, match := range matches {
		name := match[1]
		if lower {
			name = strings.ToLower(name)
		}
		seen[name] = true
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func TestDeployServiceListsAgreeAcrossDocuments(t *testing.T) {
	compose := nameSet(mountedRepository.FindAllStringSubmatch(repositoryFile(t, "docker-compose.yml"), -1), false)
	example := nameSet(agentURLName.FindAllStringSubmatch(repositoryFile(t, ".env.example"), -1), true)
	if strings.Join(compose, ",") != strings.Join(example, ",") {
		t.Fatalf("compose 의 저장소(%v)와 .env.example 의 AGENT_URL_(%v)이 다르다", compose, example)
	}
	if !strings.Contains(","+strings.Join(compose, ",")+",", ",claude,") {
		t.Fatalf("배포 대상에 claude 가 없다: %v", compose)
	}
	for _, service := range compose { // 문서의 이름은 모두 master 가 받는다
		t.Setenv("AGENT_URL_"+strings.ToUpper(service), "http://127.0.0.1:1")
	}
	master := newTestMaster(t)
	for _, service := range compose {
		recorder := call(master, "POST", "/deploy", `{"service":"`+service+`","commit_sha":"`+workflowSha+`","request_id":"gh-doc-`+service+`"}`)
		if recorder.Code != 202 {
			t.Fatalf("문서에 있는 %s 를 master 가 받지 않는다: %d %s", service, recorder.Code, recorder.Body.String())
		}
	}
}

func TestEnvironmentExampleDescribesClaude(t *testing.T) {
	example := repositoryFile(t, ".env.example")
	value := func(name string) string {
		match := envLine(name).FindStringSubmatch(example)
		if match == nil {
			t.Fatalf(".env.example 에 %s 가 없다", name)
		}
		return strings.TrimSpace(match[1])
	}
	if backend, claude := value("AGENT_URL_BACKEND"), value("AGENT_URL_CLAUDE"); backend != claude {
		t.Fatalf("claude 는 backend 와 같은 서버의 agent 로 간다: AGENT_URL_BACKEND=%s · AGENT_URL_CLAUDE=%s", backend, claude)
	}
	if directory := value("DEPLOY_DIR_CLAUDE"); directory != "${HOME}/study-note-deploy-system-claude" {
		t.Fatalf("배포 폴더: %s", directory)
	}
	if service := value("DEPLOY_SVC_CLAUDE"); service != "claude-runner" {
		t.Fatalf("compose 서비스는 실행 자리의 claude-runner: %s", service)
	}
	health := value("DEPLOY_HEALTH_CLAUDE")
	if !strings.HasSuffix(health, ":15200/health") || strings.Contains(health, "127.0.0.1") || strings.Contains(health, "localhost") {
		t.Fatalf("헬스는 LAN 주소의 15200 /health(실행 자리는 BIND_ADDR 에만 연다): %s", health)
	}
	if envLine("DEPLOY_MODE_CLAUDE").MatchString(example) {
		t.Fatal("claude 는 단일 컨테이너 — DEPLOY_MODE_CLAUDE 를 두지 않는다")
	}
}

func TestReadmeListsClaudeAsDeployTarget(t *testing.T) {
	readme := repositoryFile(t, "README.md")
	if !strings.Contains(readme, "`claude`") || !strings.Contains(readme, "claude-runner") {
		t.Fatal("README 에 배포 대상 `claude`(compose 서비스 claude-runner)가 없다")
	}
}
