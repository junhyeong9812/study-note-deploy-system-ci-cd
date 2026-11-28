//go:build race

package shared

// timeBudgetFactor — -race 는 가림을 열 배 넘게 느리게 한다 — 시간 상한을 4배로 넓힌다(제곱은 그래도 걸린다).
const timeBudgetFactor = 4
