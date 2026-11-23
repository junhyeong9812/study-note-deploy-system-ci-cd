package agent

// 테스트 격리 — 조회 대상 git 이 개발자의 전역 git 설정(~/.gitconfig · ~/.config/git/config)을 읽지 않게
// 패키지 전체의 HOME · XDG_CONFIG_HOME 을 빈 임시 디렉터리로 둔다. 기준: requirement-spec.md §12.8 "테스트".

import (
	"fmt"
	"os"
	"testing"
)

func TestMain(main *testing.M) {
	home, err := os.MkdirTemp("", "agent-test-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "임시 HOME 을 만들지 못했다:", err)
		os.Exit(1)
	}
	for name, value := range map[string]string{"HOME": home, "XDG_CONFIG_HOME": home + "/.config"} {
		if err := os.Setenv(name, value); err != nil {
			fmt.Fprintln(os.Stderr, name, "설정 실패:", err)
			os.Exit(1)
		}
	}
	code := main.Run()
	os.RemoveAll(home)
	os.Exit(code)
}

func TestTestsRunWithIsolatedHome(t *testing.T) {
	home := os.Getenv("HOME")
	if home == "" || os.Getenv("XDG_CONFIG_HOME") != home+"/.config" {
		t.Fatalf("HOME·XDG_CONFIG_HOME 이 격리되지 않았다: %q %q", home, os.Getenv("XDG_CONFIG_HOME"))
	}
	if _, err := os.Stat(home + "/.gitconfig"); !os.IsNotExist(err) {
		t.Fatalf("격리한 HOME 에 전역 git 설정이 있다: %v", err)
	}
}
