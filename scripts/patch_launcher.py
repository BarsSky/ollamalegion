#!/usr/bin/env python3
"""Patch launcher.go: fix GPUMetrics field access."""
import os

LAUNCHER_FILE = os.path.join(os.path.dirname(__file__), '..', 'internal', 'agent', 'launcher.go')

with open(LAUNCHER_FILE, 'r', encoding='utf-8') as f:
    content = f.read()

old_buildProfile = '''// buildProfile строит профиль оборудования из метрик.
func (la *LaunchAnalyzer) buildProfile(metrics *types.BackendMetrics) HardwareProfile {
	p := HardwareProfile{
		OSType: runtime.GOOS,
	}

	// GPU
	if metrics != nil {
		p.GPUCount = len(metrics.GPU.GPUs)
		p.VRAMTotalMB = metrics.GPU.MemoryTotal
		for _, gpu := range metrics.GPU.GPUs {
			p.VRAMPerGPU = append(p.VRAMPerGPU, gpu.MemoryTotal)
		}

		// Compute Capability (если есть в метриках)
		if len(metrics.GPU.GPUs) > 0 {
			p.GPUComputeCap = metrics.GPU.GPUs[0].ComputeCapability
		}

		// Системная память
		p.RAMTotalMB = metrics.System.MemoryTotal
		if metrics.System.MemoryFree > 0 {
			p.RAMFreeMB = metrics.System.MemoryFree
		}
	}'''

new_buildProfile = '''// buildProfile строит профиль оборудования из метрик.
func (la *LaunchAnalyzer) buildProfile(metrics *types.BackendMetrics) HardwareProfile {
	p := HardwareProfile{
		OSType: runtime.GOOS,
	}

	// GPU
	if metrics != nil && metrics.GPU.MemoryTotal > 0 {
		p.GPUCount = 1
		// Оцениваем количество GPU по суммарной VRAM (грубая оценка)
		if metrics.GPU.MemoryTotal > 32768 {
			p.GPUCount = 2
		}
		if metrics.GPU.MemoryTotal > 65536 {
			p.GPUCount = 4
		}
		p.VRAMTotalMB = metrics.GPU.MemoryTotal
		p.VRAMPerGPU = append(p.VRAMPerGPU, metrics.GPU.MemoryTotal/uint64(p.GPUCount))

		// Compute Capability — для RTX 30xx+ это >= 8.0, для более старых < 8.0
		// Консервативно предполагаем 7.5 (поддержка Flash Attention требует >= 8.0)
		p.GPUComputeCap = 7.5

		// Системная память
		p.RAMTotalMB = metrics.System.MemoryTotal
		if metrics.System.MemoryFree > 0 {
			p.RAMFreeMB = metrics.System.MemoryFree
		}
	}'''

if old_buildProfile in content:
    content = content.replace(old_buildProfile, new_buildProfile)
    with open(LAUNCHER_FILE, 'w', encoding='utf-8') as f:
        f.write(content)
    print('PATCHED: launcher.go buildProfile fixed')
else:
    print('NOT FOUND: buildProfile not matched')
    # Print lines containing GPUs
    for i, line in enumerate(content.split('\n')):
        if 'GPUs' in line:
            print(f'{i+1}: {repr(line)}')
    print('\nFirst 100 chars after buildProfile:')
    idx = content.find('buildProfile строит')
    if idx >= 0:
        print(repr(content[idx:idx+300]))