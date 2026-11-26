package main

// 배선: 추적 ID 의 감싸개는 mux 전체에 씌운다(통신 규약 3.4 — 경로가 맞지 않는 404 와 main 이 더하는 GET /health 도 머리글을 받게).
// main() 은 포트를 열고 돌아오지 않으므로 소스를 읽어 http.Server 의 Handler 가 shared.TrackRequests(logger, mux) 인지 본다.

import (
	"go/ast"
	"go/parser"
	"go/token"
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
	for index, want := range []string{"logger", "mux"} {
		if argument, isIdent := callExpression.Args[index].(*ast.Ident); !isIdent || argument.Name != want {
			t.Fatalf("shared.TrackRequests 의 %d번째 인자는 %s 여야 한다", index+1, want)
		}
	}
}
