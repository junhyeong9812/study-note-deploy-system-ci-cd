package main

// 배선: 추적 ID 의 감싸개는 mux 전체에 씌운다(통신 규약 3.4 — 경로가 맞지 않는 404 와 main 이 더하는 GET /health 도 머리글을 받게).
// main() 은 포트를 열고 돌아오지 않으므로 소스를 읽어 http.Server 의 Handler 가 shared.TrackRequests(logger, mux) 인지 본다.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestServerHandlerWrapsWholeMuxWithRequestTracking(t *testing.T) {
	fileSet := token.NewFileSet()
	file, err := parser.ParseFile(fileSet, "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var handlers []ast.Expr
	ast.Inspect(file, func(node ast.Node) bool {
		literal, isLiteral := node.(*ast.CompositeLit)
		if !isLiteral {
			return true
		}
		selector, isSelector := literal.Type.(*ast.SelectorExpr)
		if !isSelector || selector.Sel.Name != "Server" {
			return true
		}
		for _, element := range literal.Elts {
			if field, isField := element.(*ast.KeyValueExpr); isField {
				if key, isIdent := field.Key.(*ast.Ident); isIdent && key.Name == "Handler" {
					handlers = append(handlers, field.Value)
				}
			}
		}
		return true
	})
	if len(handlers) != 1 {
		t.Fatalf("http.Server 의 Handler 가 하나여야 한다: %d", len(handlers))
	}
	callExpression, isCall := handlers[0].(*ast.CallExpr)
	if !isCall {
		t.Fatalf("Handler 가 감싸개 호출이 아니다(mux 를 그대로 넘긴다)")
	}
	function, isSelector := callExpression.Fun.(*ast.SelectorExpr)
	if !isSelector || function.Sel.Name != "TrackRequests" || len(callExpression.Args) != 2 {
		t.Fatalf("Handler 는 shared.TrackRequests(logger, mux) 여야 한다")
	}
	if packageName, isIdent := function.X.(*ast.Ident); !isIdent || packageName.Name != "shared" {
		t.Fatalf("Handler 는 shared.TrackRequests 여야 한다")
	}
	if argument, isIdent := callExpression.Args[0].(*ast.Ident); !isIdent || argument.Name != "logger" {
		t.Fatalf("shared.TrackRequests 의 첫째 인자는 logger 여야 한다")
	}
	// 둘째 인자: 접속 줄의 감싸개가 mux 전체를 감싼다 — shared.WriteAccessLines(accessLineService(mode), mux) (⑬ T4c · 규약 3.4).
	// TrackRequests 의 안쪽이어야 접속 줄의 request_id 가 그 요청의 추적 ID 와 같다.
	inner, isInnerCall := callExpression.Args[1].(*ast.CallExpr)
	if !isInnerCall {
		t.Fatalf("shared.TrackRequests 의 둘째 인자는 shared.WriteAccessLines(accessLineService(mode), mux) 여야 한다")
	}
	innerFunction, isInnerSelector := inner.Fun.(*ast.SelectorExpr)
	if !isInnerSelector || innerFunction.Sel.Name != "WriteAccessLines" || len(inner.Args) != 2 {
		t.Fatalf("shared.TrackRequests 의 둘째 인자는 shared.WriteAccessLines(…, mux) 여야 한다")
	}
	if packageName, isIdent := innerFunction.X.(*ast.Ident); !isIdent || packageName.Name != "shared" {
		t.Fatalf("WriteAccessLines 는 shared 의 것이어야 한다")
	}
	service, isServiceCall := inner.Args[0].(*ast.CallExpr)
	if !isServiceCall {
		t.Fatalf("WriteAccessLines 의 첫째 인자는 accessLineService(mode) 여야 한다")
	}
	if name, isIdent := service.Fun.(*ast.Ident); !isIdent || name.Name != "accessLineService" || len(service.Args) != 1 {
		t.Fatalf("WriteAccessLines 의 첫째 인자는 accessLineService(mode) 여야 한다")
	}
	if argument, isIdent := service.Args[0].(*ast.Ident); !isIdent || argument.Name != "mode" {
		t.Fatalf("accessLineService 의 인자는 mode 여야 한다")
	}
	if argument, isIdent := inner.Args[1].(*ast.Ident); !isIdent || argument.Name != "mux" {
		t.Fatalf("WriteAccessLines 의 둘째 인자는 mux 여야 한다")
	}
}

// 접속 줄의 service 는 모드로 가른다(설정의 SERVER_NAME 이 아니다 — 에이전트의 SERVER_NAME 은 ci-agent-<서버>).
func TestAccessLineServiceByMode(t *testing.T) {
	t.Setenv("SERVER_NAME", "ci-agent-home")
	if got := accessLineService("master"); got != "ci-master" {
		t.Fatalf("master → ci-master: %q", got)
	}
	if got := accessLineService("agent"); got != "ci-agent" {
		t.Fatalf("agent → ci-agent: %q", got)
	}
}

// 접속 줄이 표준 출력으로 가므로 compose 의 두 서비스에 nginx 여섯과 같은 로그 상한을 둔다(규약 3.6 — json-file · 10m · 3).
func TestComposeBoundsLogsOfBothServices(t *testing.T) {
	content, err := os.ReadFile("../../docker-compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	text := string(content)
	for _, service := range []string{"master", "agent"} {
		start := strings.Index(text, "\n  "+service+":\n")
		if start < 0 {
			t.Fatalf("compose 에 %s 서비스가 없다", service)
		}
		block := text[start+1:]
		if next := regexp.MustCompile(`\n  [a-z][a-z0-9_-]*:\n`).FindStringIndex(block[1:]); next != nil {
			block = block[:next[0]+1]
		}
		want := regexp.MustCompile(`(?m)^    logging:[^\n]*\n      driver: json-file\n      options:\n        max-size: "10m"\n        max-file: "3"\n`)
		if !want.MatchString(block) {
			t.Fatalf("%s 서비스에 logging: { driver: json-file, options: { max-size: \"10m\", max-file: \"3\" } } 이 없다:\n%s", service, block)
		}
	}
}
