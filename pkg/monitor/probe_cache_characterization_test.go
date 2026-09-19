package monitor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nezhahq/agent/model"
)

func TestTryHostStopsAfterMaximumFailedProbeAttempts(t *testing.T) {
	// Given
	originalAttempts := hostDataFetchAttempts[CPU]
	hostDataFetchAttempts[CPU] = probeState{}
	t.Cleanup(func() { hostDataFetchAttempts[CPU] = originalAttempts })
	calls := 0
	probeError := errors.New("host probe failed")
	probe := func(context.Context) ([]string, error) {
		calls++
		return nil, probeError
	}

	// When
	for range maxDeviceDataFetchAttempts + 1 {
		tryHost(context.Background(), CPU, probe)
	}

	// Then
	if calls != maxDeviceDataFetchAttempts {
		t.Fatalf("host probe calls = %d, want %d", calls, maxDeviceDataFetchAttempts)
	}
	if got := hostDataFetchAttempts[CPU].attempts; got != maxDeviceDataFetchAttempts {
		t.Fatalf("host failure cache = %d, want %d", got, maxDeviceDataFetchAttempts)
	}
}

func TestTryStatSuccessClearsFailedProbeAttempts(t *testing.T) {
	// Given
	stateLock.Lock()
	originalAttempts := statDataFetchAttempts[CPU]
	statDataFetchAttempts[CPU] = probeState{attempts: 2}
	stateLock.Unlock()
	t.Cleanup(func() {
		stateLock.Lock()
		statDataFetchAttempts[CPU] = originalAttempts
		stateLock.Unlock()
	})
	probe := func(context.Context) ([]float64, error) {
		return []float64{42.5}, nil
	}

	// When
	result := tryStat(context.Background(), CPU, probe)

	// Then
	if len(result) != 1 || result[0] != 42.5 {
		t.Fatalf("state probe result = %v, want [42.5]", result)
	}
	stateLock.Lock()
	attempts := statDataFetchAttempts[CPU]
	stateLock.Unlock()
	if attempts.attempts != 0 {
		t.Fatalf("state failure cache = %d, want reset to zero", attempts.attempts)
	}
}

// The budget above is a rate limit, not a verdict: a device that recovers must be
// picked up again without restarting the agent. This is the regression that let a
// broken NVIDIA driver freeze the GPU metric for days -- once the budget was spent
// the probe was never called again, so a repaired driver stayed invisible.
func TestTryHostProbesAgainOnceCooldownExpires(t *testing.T) {
	// Given a device whose budget is spent but whose cooldown has passed
	hostLock.Lock()
	original := hostDataFetchAttempts[CPU]
	hostDataFetchAttempts[CPU] = probeState{
		attempts: maxDeviceDataFetchAttempts,
		retryAt:  time.Now().Add(-time.Second),
	}
	hostLock.Unlock()
	t.Cleanup(func() {
		hostLock.Lock()
		hostDataFetchAttempts[CPU] = original
		hostLock.Unlock()
	})
	calls := 0
	probe := func(context.Context) ([]string, error) {
		calls++
		return []string{"recovered"}, nil
	}

	// When
	result := tryHost(context.Background(), CPU, probe)

	// Then the probe runs again and the recovered device is reported
	if calls != 1 {
		t.Fatalf("host probe calls = %d, want 1", calls)
	}
	if len(result) != 1 || result[0] != "recovered" {
		t.Fatalf("host probe result = %v, want [recovered]", result)
	}
	if got := hostDataFetchAttempts[CPU]; got.attempts != 0 || !got.retryAt.IsZero() {
		t.Fatalf("host failure cache = %+v, want cleared", got)
	}
}

// An expired cooldown hands out a whole fresh budget, and spending it re-arms the
// cooldown -- the limit must not decay into probing a dead device every cycle.
func TestTryStatRefillsBudgetAndRearmsCooldownAfterItExpires(t *testing.T) {
	// Given a device whose budget is spent and whose cooldown has passed
	stateLock.Lock()
	original := statDataFetchAttempts[GPU]
	statDataFetchAttempts[GPU] = probeState{
		attempts: maxDeviceDataFetchAttempts,
		retryAt:  time.Now().Add(-time.Second),
	}
	stateLock.Unlock()
	t.Cleanup(func() {
		stateLock.Lock()
		statDataFetchAttempts[GPU] = original
		stateLock.Unlock()
	})
	calls := 0
	probe := func(context.Context) ([]float64, error) {
		calls++
		return nil, errors.New("state probe failed")
	}

	// When a full budget's worth of failures follows the expiry
	for range maxDeviceDataFetchAttempts {
		tryStat(context.Background(), GPU, probe)
	}

	// Then
	stateLock.Lock()
	got := statDataFetchAttempts[GPU]
	stateLock.Unlock()
	if calls != maxDeviceDataFetchAttempts {
		t.Fatalf("state probe calls = %d, want %d", calls, maxDeviceDataFetchAttempts)
	}
	if got.attempts != maxDeviceDataFetchAttempts {
		t.Fatalf("state failure cache = %d, want %d", got.attempts, maxDeviceDataFetchAttempts)
	}
	if !got.retryAt.After(time.Now()) {
		t.Fatalf("state cooldown = %v, want a future deadline", got.retryAt)
	}
}

// Inside the cooldown the probe must not run at all: that is what keeps a broken
// device from spawning nvidia-smi -- and writing to stderr -- on every cycle.
func TestTryStatStaysQuietInsideCooldown(t *testing.T) {
	// Given a device whose budget is spent and whose cooldown still holds
	stateLock.Lock()
	original := statDataFetchAttempts[GPU]
	statDataFetchAttempts[GPU] = probeState{
		attempts: maxDeviceDataFetchAttempts,
		retryAt:  time.Now().Add(time.Minute),
	}
	stateLock.Unlock()
	t.Cleanup(func() {
		stateLock.Lock()
		statDataFetchAttempts[GPU] = original
		stateLock.Unlock()
	})
	calls := 0
	probe := func(context.Context) ([]float64, error) {
		calls++
		return []float64{1}, nil
	}

	// When
	result := tryStat(context.Background(), GPU, probe)

	// Then
	if calls != 0 {
		t.Fatalf("state probe calls = %d, want 0 while the cooldown holds", calls)
	}
	if result != nil {
		t.Fatalf("state probe result = %v, want the zero value", result)
	}
}

func TestTrackNetworkSpeedPreservesTransferAndSpeedFormula(t *testing.T) {
	// Given
	originalProbe := nicStateProbe
	originalNow := networkNow
	metricLock.Lock()
	originalNetInSpeed, originalNetOutSpeed := netInSpeed, netOutSpeed
	originalNetInTransfer, originalNetOutTransfer := netInTransfer, netOutTransfer
	originalLastUpdate := lastUpdateNetStats
	netInTransfer, netOutTransfer, lastUpdateNetStats = 1000, 2000, 100
	metricLock.Unlock()
	nicStateProbe = func(context.Context) ([]uint64, error) {
		return []uint64{1400, 2600}, nil
	}
	networkNow = func() time.Time { return time.Unix(104, 0) }
	t.Cleanup(func() {
		nicStateProbe = originalProbe
		networkNow = originalNow
		metricLock.Lock()
		netInSpeed, netOutSpeed = originalNetInSpeed, originalNetOutSpeed
		netInTransfer, netOutTransfer = originalNetInTransfer, originalNetOutTransfer
		lastUpdateNetStats = originalLastUpdate
		metricLock.Unlock()
	})

	// When
	TrackNetworkSpeed(&model.AgentConfig{})

	// Then
	metricLock.RLock()
	defer metricLock.RUnlock()
	if netInTransfer != 1400 || netOutTransfer != 2600 {
		t.Fatalf("network transfers = in:%d out:%d, want in:1400 out:2600", netInTransfer, netOutTransfer)
	}
	if netInSpeed != 100 || netOutSpeed != 150 {
		t.Fatalf("network speeds = in:%d out:%d, want delta/time in:100 out:150", netInSpeed, netOutSpeed)
	}
	if lastUpdateNetStats != 104 {
		t.Fatalf("last network update = %d, want 104", lastUpdateNetStats)
	}
}
