package monitor

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/process"

	"github.com/nezhahq/agent/model"
	"github.com/nezhahq/agent/pkg/logger"
	"github.com/nezhahq/agent/pkg/monitor/conn"
	"github.com/nezhahq/agent/pkg/monitor/cpu"
	"github.com/nezhahq/agent/pkg/monitor/disk"
	"github.com/nezhahq/agent/pkg/monitor/gpu"
	"github.com/nezhahq/agent/pkg/monitor/load"
	"github.com/nezhahq/agent/pkg/monitor/nic"
	"github.com/nezhahq/agent/pkg/monitor/temperature"
)

var (
	Version string
	printf  = logger.Printf
)

var (
	hostInfoProbe      = host.Info
	virtualMemoryProbe = mem.VirtualMemory
	swapMemoryProbe    = mem.SwapMemory
	processIDsProbe    = process.Pids
	cpuHostProbe       = cpu.GetHost
	cpuStateProbe      = cpu.GetState
	diskHostProbe      = disk.GetHost
	diskStateProbe     = disk.GetState
	gpuHostProbe       = gpu.GetHost
	gpuStatProbe       = gpu.GetStat
	loadStateProbe     = load.GetState
	nicStateProbe      = nic.GetState
	connStateProbe     = conn.GetState
	temperatureProbe   = temperature.GetState
	temperatureUpdated = func() {}
)

var (
	netInSpeed, netOutSpeed, netInTransfer, netOutTransfer, lastUpdateNetStats uint64
	cachedBootTime                                                             time.Time
	temperatureStat                                                            []model.SensorTemperature
)

const (
	CPU = iota + 1
	GPU
	Load
	Temperatures
)

const (
	// maxDeviceDataFetchAttempts is how many consecutive probe failures a device
	// gets before it is backed off.
	maxDeviceDataFetchAttempts = 3
	// deviceProbeCooldown is how long a device is left alone once that budget is
	// spent. The backoff has to expire: a driver reload, or a GPU that comes back,
	// would otherwise stay invisible until the agent is restarted, because a spent
	// budget means the probe is never called again.
	deviceProbeCooldown = 10 * time.Minute
)

// probeState is one device's probe budget. attempts counts consecutive failures;
// retryAt is when a device whose budget is spent may be probed again.
type probeState struct {
	attempts uint8
	retryAt  time.Time
}

// mayProbe reports whether the probe should run now. A device inside its cooldown
// is skipped until that cooldown expires.
func (s probeState) mayProbe(now time.Time) bool {
	return s.attempts < maxDeviceDataFetchAttempts || !now.Before(s.retryAt)
}

// recordFailure spends one attempt and arms the cooldown once the budget is gone.
func (s probeState) recordFailure(now time.Time) probeState {
	if s.attempts < maxDeviceDataFetchAttempts {
		s.attempts++
	}
	if s.attempts >= maxDeviceDataFetchAttempts {
		s.retryAt = now.Add(deviceProbeCooldown)
	}
	return s
}

var hostDataFetchAttempts = map[uint8]probeState{
	CPU: {},
	GPU: {},
}

var statDataFetchAttempts = map[uint8]probeState{
	CPU:          {},
	GPU:          {},
	Load:         {},
	Temperatures: {},
}

// reportProbeError records a failed device probe. The logger honours the `debug`
// setting, so the stderr copy is what keeps a failing probe visible on hosts
// running with debug off -- otherwise the metric simply freezes, with nothing in
// the journal to explain it.
func reportProbeError(err error, typ uint8, attempts uint8) {
	msg := fmt.Sprintf("monitor error: %v, type: %d, attempt: %d", err, typ, attempts)
	printf("%s", msg)
	fmt.Fprintln(os.Stderr, msg)
}

var (
	updateTempStatus atomic.Bool
	hostLock         sync.Mutex
	stateLock        sync.Mutex
	metricLock       sync.RWMutex
	temperatureLock  sync.RWMutex
)

type hostStateFunc[T any] func(context.Context) (T, error)

// probeWithBackoff runs probe while holding lock, honouring the device's failure
// budget and cooldown. A device that keeps failing is not probed on every cycle,
// but the cooldown expires so it recovers on its own.
func probeWithBackoff[T any](
	lock *sync.Mutex,
	states map[uint8]probeState,
	ctx context.Context,
	typ uint8,
	probe hostStateFunc[T],
) T {
	var value T

	lock.Lock()
	defer lock.Unlock()

	now := time.Now()
	state := states[typ]
	if !state.mayProbe(now) {
		return value
	}
	if state.attempts >= maxDeviceDataFetchAttempts {
		state = probeState{} // the cooldown expired: hand out a fresh budget
	}

	result, err := probe(ctx)
	if err != nil {
		state = state.recordFailure(now)
		states[typ] = state
		reportProbeError(err, typ, state.attempts)
		return value
	}

	states[typ] = probeState{}
	return result
}

func tryHost[T any](ctx context.Context, typ uint8, probe hostStateFunc[T]) T {
	return probeWithBackoff(&hostLock, hostDataFetchAttempts, ctx, typ, probe)
}

func tryStat[T any](ctx context.Context, typ uint8, probe hostStateFunc[T]) T {
	return probeWithBackoff(&stateLock, statDataFetchAttempts, ctx, typ, probe)
}
