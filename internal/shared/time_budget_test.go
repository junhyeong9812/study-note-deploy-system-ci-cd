//go:build !race

package shared

// timeBudgetFactor — 시간 상한을 넓히는 배수. -race 로 돌면 4배(time_budget_race_test.go).
const timeBudgetFactor = 1
