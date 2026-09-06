//go:build windows

package gpu

import (
	"context"

	"github.com/nezhahq/agent/pkg/monitor/gpu/vendor"
)

// nvidiaSMIWindows is where the NVIDIA driver installs the tool. Start() falls
// back to PATH when it is absent, so a non-standard install still resolves.
const nvidiaSMIWindows = `C:\Windows\System32\nvidia-smi.exe`

// detailedStat covers NVIDIA on Windows. The generic path here is PDH
// performance counters, which expose engine utilization but no frame buffer,
// so a machine with nvidia-smi gets its memory figures from that instead.
// Anything else -- AMD, Intel, no GPU at all -- reports false and falls back.
func detailedStat(_ context.Context) ([]vendor.GPUStat, bool, error) {
	smi := &vendor.NvidiaSMI{BinPath: nvidiaSMIWindows}
	if err := smi.Start(); err != nil {
		return nil, false, nil
	}
	stats, err := smi.GatherStat()
	return stats, true, err
}
