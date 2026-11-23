package agent

// 서버 전체 값 — 기준: requirement-spec.md(2026-11-24 resource-metrics) §2.1 · §10.1 · §10.2 · §11.1.
// 정한 계산(명세가 정하지 않은 세부):
//   - CPU: /proc/stat 첫 줄의 user..steal(앞 8칸)의 합이 전체, idle + iowait 가 쉬는 시간. steal 은 바쁨으로 센다.
//     guest · guest_nice 는 이미 user · nice 에 들어 있어 더하지 않는다.
//   - 메모리: kB 는 1024 바이트. mem_used = MemTotal − MemAvailable.
//   - 디스크 용량: statfs("/") — 전체 = Blocks×Bsize, 사용 = (Blocks−Bfree)×Bsize (df 의 Size · Used).
//   - 네트워크: lo · veth* · br-* · docker* · cni* · flannel* 은 세지 않는다(가상 장치 — 호스트 장치와 겹쳐 센다).
//   - 디스크 입출력: 통째 디스크만(sd? · vd? · xvd? · hd? · nvmeXnY · mmcblkX) — 파티션 · loop · ram · dm- · md · sr · zram 은 세지 않는다. 섹터는 512 바이트.
//   - 차이의 기준은 장치별 · 방향별. 새 장치 · 줄어든 카운터는 그 몫만 빼고 기준을 새로 잡는다. 합칠 몫이 없으면 null.

import (
	"errors"
	"strconv"
	"syscall"
	"testing"
	"time"
)

var hostKeys = []string{"cpu_pct", "load1", "mem_total", "mem_used", "disk_total", "disk_used",
	"net_rx_bps", "net_tx_bps", "io_read_bps", "io_write_bps"}

var hostRateKeys = []string{"cpu_pct", "net_rx_bps", "net_tx_bps", "io_read_bps", "io_write_bps"}

// requireHostLevels — 차이가 아닌 값(부하 · 메모리 · 디스크 용량)이 받침 파일 그대로인가.
func requireHostLevels(t *testing.T, situation string, host map[string]any, load1 float64) {
	t.Helper()
	requireNumber(t, situation+" load1", member(t, host, "load1", situation), load1, 1e-9)
	requireNumber(t, situation+" mem_total", member(t, host, "mem_total", situation), 1024000000, 0)
	requireNumber(t, situation+" mem_used", member(t, host, "mem_used", situation), 768000000, 0)
	requireNumber(t, situation+" disk_total", member(t, host, "disk_total", situation), 4096000, 0)
	requireNumber(t, situation+" disk_used", member(t, host, "disk_used", situation), 3072000, 0)
}

func requireHostRates(t *testing.T, situation string, host map[string]any) {
	t.Helper()
	requireNumber(t, situation+" cpu_pct", member(t, host, "cpu_pct", situation), 63.333, 0.06)
	requireNumber(t, situation+" net_rx_bps", member(t, host, "net_rx_bps", situation), 1200, 0.5)
	requireNumber(t, situation+" net_tx_bps", member(t, host, "net_tx_bps", situation), 3400, 0.5)
	requireNumber(t, situation+" io_read_bps", member(t, host, "io_read_bps", situation), 11264, 0.5)
	requireNumber(t, situation+" io_write_bps", member(t, host, "io_write_bps", situation), 51200, 0.5)
}

func requireHostRatesNull(t *testing.T, situation string, host map[string]any) {
	t.Helper()
	for _, key := range hostRateKeys {
		requireNull(t, situation+" "+key, member(t, host, key, situation))
	}
}

func TestResourceFirstCollectionSendsLevelsAndNullRates(t *testing.T) {
	harness := newResourceHarness(t, defaultResourceSettings())
	entry := harness.collect()
	body := entry.body(t)
	requireKeys(t, "body", body, "interval_ms", "host", "containers", "gpus", "missing")
	requireNull(t, "첫 수집의 interval_ms", body["interval_ms"])
	host := hostOf(t, entry)
	requireKeys(t, "host", host, hostKeys...)
	requireHostLevels(t, "첫 수집", host, 0.11)
	requireHostRatesNull(t, "첫 수집(앞 표본 없음)", host)
	requireMissing(t, entry, "첫 수집 — 읽지 못한 갈래 없음")
}

func TestResourceHostRatesFromTwoSamples(t *testing.T) {
	harness := newResourceHarness(t, defaultResourceSettings())
	harness.collect()
	harness.clock.advance(30 * time.Second)
	harness.writeProc(nextProc())
	entry := harness.collect()
	requireNumber(t, "interval_ms", member(t, entry.body(t), "interval_ms", "body"), 30000, 0)
	host := hostOf(t, entry)
	requireHostLevels(t, "둘째 수집", host, 0.42)
	requireHostRates(t, "둘째 수집", host)
	harness.statfsMutex.Lock()
	paths := append([]string(nil), harness.statfsPaths...)
	harness.statfsMutex.Unlock()
	if len(paths) == 0 || paths[0] != "/" {
		t.Fatalf("디스크 용량은 diskPath(\"/\")에서 재야 한다: %v", paths)
	}
}

func TestResourceHostDiskSpaceFromStatfs(t *testing.T) {
	harness := newResourceHarness(t, defaultResourceSettings())
	harness.setStatfs(func(path string, stat *syscall.Statfs_t) error {
		stat.Blocks, stat.Bfree, stat.Bavail, stat.Bsize = 122127716, 102457780, 96216912, 4096 // 실제 ext4 꼴(예약 블록 있음)
		return nil
	})
	host := hostOf(t, harness.collect())
	requireNumber(t, "disk_total", host["disk_total"], 122127716*4096, 0)
	requireNumber(t, "disk_used", host["disk_used"], (122127716-102457780)*4096, 0)
}

func TestResourceRatesAreNullAfterCountersDecreaseAndRecoverNextTime(t *testing.T) {
	harness := newResourceHarness(t, defaultResourceSettings())
	harness.writeProc(nextProc())
	harness.collect()
	harness.clock.advance(30 * time.Second)
	rebooted := rebootProc() // 재부팅 — 모든 누적 값이 줄었다
	harness.writeProc(rebooted)
	host := hostOf(t, harness.collect())
	requireHostRatesNull(t, "누적 값이 줄어든 수집", host)
	requireHostLevels(t, "누적 값이 줄어든 수집", host, 0.11)

	harness.clock.advance(30 * time.Second)
	advanceAll(&rebooted)
	harness.writeProc(rebooted)
	requireHostRates(t, "줄어든 다음 수집(새 기준에서 정상)", hostOf(t, harness.collect()))
}

// rebootProc — baseProc 과 같은 장치 · 같은 수준 값이지만 누적 값이 모두 nextProc 보다 작다.
func rebootProc() procState {
	state := baseProc()
	for index := range state.cpu {
		state.cpu[index] /= 10
	}
	for index := range state.network {
		state.network[index].rx, state.network[index].tx = 10, 10
	}
	for index := range state.disks {
		state.disks[index].sectorsRead, state.disks[index].sectorsWritten = 1, 1
	}
	return state
}

// advanceAll — CPU · 네트워크 · 디스크 누적 값을 baseProc → nextProc 만큼 더 움직인다(장치 순서가 baseProc 과 같을 때).
// 30초 뒤면 requireHostRates 의 값이 나온다.
func advanceAll(state *procState) {
	advanceCPU(state)
	base, next := baseProc(), nextProc()
	for index := range state.network {
		state.network[index].rx += next.network[index].rx - base.network[index].rx
		state.network[index].tx += next.network[index].tx - base.network[index].tx
	}
	for index := range state.disks {
		state.disks[index].sectorsRead += next.disks[index].sectorsRead - base.disks[index].sectorsRead
		state.disks[index].sectorsWritten += next.disks[index].sectorsWritten - base.disks[index].sectorsWritten
	}
}

func TestResourceRatesAreNullWhenGapExceedsTwoMinutes(t *testing.T) {
	harness := newResourceHarness(t, defaultResourceSettings())
	harness.collect()
	harness.clock.advance(2*time.Minute + time.Second)
	harness.writeProc(nextProc())
	entry := harness.collect()
	requireHostRatesNull(t, "간격 2분 초과", hostOf(t, entry))

	harness.clock.advance(30 * time.Second)
	state := nextProc()
	advanceAll(&state)
	harness.writeProc(state)
	requireHostRates(t, "2분 초과 다음 수집(30초 — 정상)", hostOf(t, harness.collect()))
}

func TestResourceRatesAtExactlyTwoMinutesAreNumbers(t *testing.T) {
	harness := newResourceHarness(t, defaultResourceSettings())
	harness.collect()
	harness.clock.advance(2 * time.Minute)
	harness.writeProc(nextProc())
	host := hostOf(t, harness.collect())
	requireNumber(t, "2분 간격의 rx", host["net_rx_bps"], 300, 0.5) // 36000 / 120
	requireNumber(t, "2분 간격의 cpu_pct", host["cpu_pct"], 63.333, 0.06)
}

func TestResourceRatesUseMonotonicClockNotWallClock(t *testing.T) {
	for _, shift := range []time.Duration{-time.Hour, time.Hour} {
		harness := newResourceHarness(t, defaultResourceSettings())
		first := harness.collect()
		harness.clock.advance(30 * time.Second)
		harness.clock.shiftWall(shift) // 벽시계만 한 시간 뒤로 · 앞으로
		harness.writeProc(nextProc())
		entry := harness.collect()
		requireHostRates(t, "벽시계가 "+shift.String()+" 움직인 수집", hostOf(t, entry))
		requireNumber(t, "interval_ms(단조 시계)", entry.body(t)["interval_ms"], 30000, 0)
		firstAt, _ := strconv.ParseInt(first.field("at"), 10, 64)
		secondAt, _ := strconv.ParseInt(entry.field("at"), 10, 64)
		if secondAt-firstAt != (30*time.Second + shift).Milliseconds() {
			t.Fatalf("at 은 벽시계여야 한다: %d → %d", firstAt, secondAt)
		}
	}
}

func TestResourceDeviceCounterResetAffectsOnlyThatDevice(t *testing.T) {
	harness := newResourceHarness(t, defaultResourceSettings())
	harness.collect()
	harness.clock.advance(30 * time.Second)
	state := nextProc()
	for index := range state.network {
		if state.network[index].name == "eno1" {
			state.network[index].rx = 10 // eno1 의 수신 카운터만 초기화(드라이버 재적재)
		}
	}
	harness.writeProc(state)
	host := hostOf(t, harness.collect())
	requireNumber(t, "eno1 수신이 줄면 그 몫만 빠진다(wlo1 만)", host["net_rx_bps"], 200, 0.5)
	requireNumber(t, "송신은 그대로", host["net_tx_bps"], 3400, 0.5)
	requireNumber(t, "디스크 입출력은 그대로", host["io_read_bps"], 11264, 0.5)

	harness.clock.advance(30 * time.Second)
	next := state
	advanceCPU(&next)
	for index := range next.network {
		switch next.network[index].name {
		case "eno1":
			next.network[index].rx += 30000
			next.network[index].tx += 90000
		case "wlo1":
			next.network[index].rx += 6000
			next.network[index].tx += 12000
		}
	}
	harness.writeProc(next)
	requireNumber(t, "초기화 다음 수집은 새 기준에서 정상", hostOf(t, harness.collect())["net_rx_bps"], 1200, 0.5)
}

func TestResourceDeviceAppearingOrVanishingDoesNotSkewOthers(t *testing.T) {
	harness := newResourceHarness(t, defaultResourceSettings())
	harness.collect()
	harness.clock.advance(30 * time.Second)
	state := nextProc()
	var kept []networkCounter
	for _, counter := range state.network {
		if counter.name != "wlo1" { // wlo1 이 사라졌다
			kept = append(kept, counter)
		}
	}
	state.network = append(kept, networkCounter{"eno2", 900000000, 900000000}) // eno2 가 새로 생겼다(큰 누적 값)
	state.disks = append(state.disks, diskCounter{8, 16, "sdb", 700000000, 700000000})
	harness.writeProc(state)
	host := hostOf(t, harness.collect())
	requireNumber(t, "새 장치는 첫 표본 — eno1 만", host["net_rx_bps"], 1000, 0.5)
	requireNumber(t, "새 장치는 첫 표본 — eno1 만", host["net_tx_bps"], 3000, 0.5)
	requireNumber(t, "새 디스크는 첫 표본", host["io_read_bps"], 11264, 0.5)

	harness.clock.advance(30 * time.Second)
	advanceCPU(&state)
	for index := range state.network {
		state.network[index].rx += 3000
		state.network[index].tx += 3000
	}
	harness.writeProc(state)
	requireNumber(t, "다음 수집에는 새 장치도 센다(eno1 + eno2)", hostOf(t, harness.collect())["net_rx_bps"], 200, 0.5)
}

func TestResourceMissingHostWhenProcUnreadable(t *testing.T) {
	harness := newResourceHarness(t, defaultResourceSettings())
	harness.collector.procRoot = harness.procRoot + "/없는-디렉터리"
	harness.docker.set(func(docker *fakeDocker) {
		docker.containers = []*fakeContainer{projectContainer("a1", "front-app-green", "front")}
	})
	entry := harness.collect()
	host := hostOf(t, entry)
	requireKeys(t, "읽지 못한 host 도 칸은 그대로", host, hostKeys...)
	for _, key := range []string{"cpu_pct", "load1", "mem_total", "mem_used", "net_rx_bps", "net_tx_bps", "io_read_bps", "io_write_bps"} {
		requireNull(t, "/proc 을 못 읽은 "+key, host[key])
	}
	requireMissing(t, entry, "/proc 을 못 읽음", missingHost)
	if containers := listOf(t, entry, "containers"); len(containers) != 1 {
		t.Fatalf("/proc 을 못 읽어도 컨테이너 갈래는 나가야 한다: %v", containers)
	}
}

func TestResourceMissingHostWhenDiskSpaceUnreadable(t *testing.T) {
	harness := newResourceHarness(t, defaultResourceSettings())
	harness.setStatfs(func(string, *syscall.Statfs_t) error { return errors.New("statfs 실패") })
	entry := harness.collect()
	host := hostOf(t, entry)
	requireNull(t, "disk_total", host["disk_total"])
	requireNull(t, "disk_used", host["disk_used"])
	requireNumber(t, "디스크 용량을 못 읽어도 메모리는 그대로", host["mem_total"], 1024000000, 0)
	requireNumber(t, "디스크 용량을 못 읽어도 부하는 그대로", host["load1"], 0.11, 1e-9)
	requireMissing(t, entry, "디스크 용량을 못 읽음", missingHost)
}

// advanceCPU — CPU 누적 값을 baseProc → nextProc 만큼 더 움직인다(같은 63.33%).
func advanceCPU(state *procState) {
	for index := range state.cpu {
		state.cpu[index] += nextProc().cpu[index] - baseProc().cpu[index]
	}
}

// 두 표본 사이에 CPU 누적 값이 하나도 움직이지 않았으면(0 으로 나눔) cpu_pct 는 null 이다 — NaN 이 JSON 을 깨지 않는다.
// 움직이지 않은 네트워크 · 디스크는 0 B/s 다(정의된 값).
func TestResourceCPUWithoutElapsedTicksIsNullAndIdleDevicesAreZero(t *testing.T) {
	harness := newResourceHarness(t, defaultResourceSettings())
	harness.collect()
	harness.clock.advance(30 * time.Second)
	host := hostOf(t, harness.collect()) // 받침 파일 그대로
	requireNull(t, "CPU 누적 값이 그대로인 cpu_pct", host["cpu_pct"])
	requireNumber(t, "움직이지 않은 수신", host["net_rx_bps"], 0, 0)
	requireNumber(t, "움직이지 않은 쓰기", host["io_write_bps"], 0, 0)
}
