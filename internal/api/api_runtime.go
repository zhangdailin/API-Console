package api

import (
	"fmt"
	"net/http"
	"orchids-api/internal/util"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"orchids-api/internal/loadbalancer"
)

// CPU is sampled from cumulative Linux scheduler ticks, never since boot and
// never by sleeping in an HTTP handler. Memory is host scope, RSS is process scope.
//
// The sampler keeps the last reading and the value derived from it. A window
// shorter than hostCPUMinWindow is not measured: /proc/stat counts whole ticks,
// so a window of one or two ticks is decided by whether this very request
// happened to run in it — back-to-back calls reported 100% CPU on a host that was
// 95% idle. The previous value is reported instead, with the window it came from.
var hostCPU = struct {
	sync.Mutex
	total, idle uint64
	sampledAt   time.Time
	value       float64
	window      time.Duration
	hasValue    bool
}{}

// hostCPUMinWindow is the shortest window a rate may be computed over. One second
// is still coarse (100 ticks per core) and never a single request's worth of noise.
const hostCPUMinWindow = time.Second

// sampleHostCPU turns two /proc/stat readings into a busy ratio. ok is false when
// no rate can be reported yet; in that case any previously measured rate is
// returned unchanged so a caller cannot mistake a stale window for a fresh one.
func sampleHostCPU(raw string, now time.Time) (busy float64, window time.Duration, ok bool) {
	total, idle, err := parseCPUTicks(raw)
	if err != nil {
		return 0, 0, false
	}
	hostCPU.Lock()
	defer hostCPU.Unlock()
	if hostCPU.sampledAt.IsZero() || total <= hostCPU.total || idle < hostCPU.idle {
		// First reading, or the counters moved backwards (reboot, wrap). Start a
		// new window rather than dividing by a meaningless delta.
		hostCPU.total, hostCPU.idle, hostCPU.sampledAt = total, idle, now
		hostCPU.hasValue = false
		return 0, 0, false
	}
	elapsed := now.Sub(hostCPU.sampledAt)
	if elapsed < hostCPUMinWindow {
		if hostCPU.hasValue {
			return hostCPU.value, hostCPU.window, true
		}
		return 0, 0, false
	}
	deltaTotal := total - hostCPU.total
	deltaIdle := idle - hostCPU.idle
	if deltaTotal == 0 {
		return 0, 0, false
	}
	busy = 1 - float64(deltaIdle)/float64(deltaTotal)
	if busy < 0 {
		busy = 0
	}
	if busy > 1 {
		busy = 1
	}
	hostCPU.total, hostCPU.idle, hostCPU.sampledAt = total, idle, now
	hostCPU.value, hostCPU.window, hostCPU.hasValue = busy, elapsed, true
	return busy, elapsed, true
}

func parseCPUTicks(raw string) (total, idle uint64, err error) {
	line, _, _ := strings.Cut(raw, "\n")
	fields := strings.Fields(line)
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0, 0, fmt.Errorf("invalid /proc/stat")
	}
	for i, field := range fields[1:] {
		if i >= 8 {
			break
		}
		v, e := strconv.ParseUint(field, 10, 64)
		if e != nil {
			return 0, 0, e
		}
		total += v
		if i == 3 || i == 4 {
			idle += v
		}
	}
	return
}
func parseHostMemory(raw string) (used, total uint64) {
	values := map[string]uint64{}
	for line := range strings.SplitSeq(raw, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 {
			v, _ := strconv.ParseUint(f[1], 10, 64)
			values[strings.TrimSuffix(f[0], ":")] = v * 1024
		}
	}
	total = values["MemTotal"]
	available, ok := values["MemAvailable"]
	if !ok {
		available = values["MemFree"] + values["Buffers"] + values["Cached"]
	}
	if available < total {
		used = total - available
	}
	return
}
func runtimeMetric(label, value, detail string, available bool, percent float64) map[string]interface{} {
	status := "unknown"
	if available {
		status = "ok"
		if percent >= 90 {
			status = "critical"
		} else if percent >= 80 {
			status = "warn"
		}
	}
	return map[string]interface{}{"label": label, "value": value, "detail": detail, "available": available, "status": status}
}
func (a *API) HandleOpsRuntime(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet) {
		return
	}
	cpu := runtimeMetric("主机 CPU", "", "需要 Linux /proc 采集", false, 0)
	if raw, err := os.ReadFile("/proc/stat"); err == nil {
		if busy, window, ok := sampleHostCPU(string(raw), time.Now()); ok {
			cpu = runtimeMetric("主机 CPU", fmt.Sprintf("%.1f%%", busy*100),
				fmt.Sprintf("整机所有核心 · %.1fs 采样窗口", window.Seconds()), true, busy*100)
		} else {
			cpu = runtimeMetric("主机 CPU", "", "等待下一次采样", false, 0)
		}
	}
	memory := runtimeMetric("主机内存", "", "需要 Linux /proc 采集", false, 0)
	if raw, err := os.ReadFile("/proc/meminfo"); err == nil {
		used, total := parseHostMemory(string(raw))
		if total > 0 {
			percent := float64(used) / float64(total) * 100
			memory = runtimeMetric("主机内存", fmt.Sprintf("%.1f%%", percent), formatRuntimeBytes(used)+" / "+formatRuntimeBytes(total)+" · 已用/总量", true, percent)
		}
	}
	rss := runtimeMetric("进程内存 RSS", "", "需要 Linux /proc 采集", false, 0)
	if raw, err := os.ReadFile("/proc/self/statm"); err == nil {
		fields := strings.Fields(string(raw))
		if len(fields) >= 2 {
			pages, err := strconv.ParseUint(fields[1], 10, 64)
			if err == nil {
				rss = runtimeMetric("进程内存 RSS", formatRuntimeBytes(pages*uint64(os.Getpagesize())), "当前服务常驻物理内存", true, 0)
			}
		}
	}
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	refreshing := 0
	if a != nil && a.refreshConcurrency != nil {
		refreshing = a.refreshConcurrency()
	}
	metrics := []map[string]interface{}{cpu, memory, rss, runtimeMetric("Go 堆内存", formatRuntimeBytes(mem.Alloc), "当前已分配", true, 0), runtimeMetric("Goroutine", strconv.Itoa(runtime.NumGoroutine()), "当前协程数", true, 0), runtimeMetric("账号刷新", strconv.Itoa(refreshing), "正在刷新", true, 0)}
	util.WriteJSON(w, map[string]interface{}{"available": true, "metrics": metrics})
}
func formatRuntimeBytes(value uint64) string {
	if value >= 1<<30 {
		return fmt.Sprintf("%.2f GB", float64(value)/(1<<30))
	}
	return fmt.Sprintf("%.1f MB", float64(value)/(1<<20))
}

func (a *API) SetConnectionTracker(tracker loadbalancer.ConnTracker) { a.connTracker = tracker }
