/**
 * setup-wizard-presets.js — Hardware preset definitions for setup-wizard.
 *
 * R58.3 (2026-09-03): Phase 1.3b. Hardcoded subset of config/hardware-presets/*.json
 * (cuda_arch + key tuning values) that the setup-wizard can apply directly
 * to the balancer tuning fields (vramMaxUsage, gpuMaxUsage, etc.).
 *
 * For CPPWORKER_* params (n_ctx, batch, gpu_layers, etc.) the user must run
 * `python scripts/apply-hardware-preset.py <name>` separately — the wizard
 * shows a hint with the recommended values.
 *
 * The full preset JSON files (config/hardware-presets/*.json) are the source
 * of truth. This JS module mirrors a small subset for the UI.
 *
 * Контракт:
 *   - window.HardwarePresets = { presets: { rtx30-8gb: {...}, ... }, order: [...] }
 *   - Загружается ДО setup-wizard.js (см. webui/index.html)
 */
(function() {
    'use strict';

    const presets = {
        // RTX 30xx 8GB (RTX 3050/3060/3070/3080 8GB, sm_86)
        'rtx30-8gb': {
            label: 'RTX 30xx 8GB',
            description: 'RTX 3050/3060/3070/3080 8GB. Aggressive RAM fallback. Q4_K_M or lower for >7B models.',
            recommendedModels: 'qwen2.5-3b, qwen2.5-7b, llama-3.1-8b, gemma-4-9b (4-bit), mistral-7b',
            cudaArch: '86',
            // Balancer tuning (applied to wizard fields)
            vramMaxUsage: 85,
            gpuMaxUsage: 90,
            cpuMaxUsage: 80,
            ramMaxUsage: 75,
            // Hint for cppworker params (показывается пользователю)
            cppworkerHint: {
                'CPPWORKER_CTX_SIZE': '32768',
                'CPPWORKER_BATCH_SIZE': '512',
                'CPPWORKER_GPU_LAYERS': '20',
                'CPPWORKER_KV_CACHE_TYPE': 'q4_0',
                'CPPWORKER_RAM_FALLBACK_MAX_N_CTX': '64000'
            }
        },
        // RTX 40xx 16-24GB (RTX 4080 16GB, RTX 4090 24GB, sm_89)
        'rtx40-24gb': {
            label: 'RTX 40xx 16-24GB',
            description: 'RTX 4080 16GB, RTX 4090 24GB. Full GPU offload для 7B-13B. 70B partial offload.',
            recommendedModels: 'qwen2.5-7b/14b, llama-3.1-8b/13b, gemma-4-27b (Q4), mixtral-8x7b (Q3)',
            cudaArch: '89',
            vramMaxUsage: 90,
            gpuMaxUsage: 90,
            cpuMaxUsage: 85,
            ramMaxUsage: 80,
            cppworkerHint: {
                'CPPWORKER_CTX_SIZE': '32768',
                'CPPWORKER_BATCH_SIZE': '512',
                'CPPWORKER_GPU_LAYERS': '99',
                'CPPWORKER_KV_CACHE_TYPE': 'f16',
                'CPPWORKER_RAM_FALLBACK_MAX_N_CTX': '128000'
            }
        },
        // NVIDIA A10/A10G 24GB (sm_86, same as RTX 3070, no rebuild needed)
        'a10-24gb': {
            label: 'A10 / A10G 24GB',
            description: 'Cloud GPU (AWS G5, Lambda, RunPod). Same arch as RTX 3070 (sm_86), but 3x VRAM.',
            recommendedModels: 'qwen2.5-7b/14b, llama-3.1-8b/13b, gemma-4-27b (Q4)',
            cudaArch: '86',
            vramMaxUsage: 80,
            gpuMaxUsage: 88,
            cpuMaxUsage: 80,
            ramMaxUsage: 75,
            cppworkerHint: {
                'CPPWORKER_CTX_SIZE': '32768',
                'CPPWORKER_BATCH_SIZE': '512',
                'CPPWORKER_GPU_LAYERS': '99',
                'CPPWORKER_KV_CACHE_TYPE': 'f16',
                'CPPWORKER_RAM_FALLBACK_MAX_N_CTX': '128000'
            }
        },
        // RTX 5090 32GB (Blackwell, sm_120, NEEDS REBUILD)
        'rtx50-32gb': {
            label: 'RTX 5090 32GB',
            description: 'RTX 5090. NEWEST arch (sm_120, Blackwell). Requires CUDA 12.8+ and image REBUILD.',
            recommendedModels: 'qwen2.5-7b through 32b, llama-3.1-8b through 70b (Q4), gemma-4-27b (F16), mixtral-8x22b (Q4)',
            cudaArch: '120',
            vramMaxUsage: 92,
            gpuMaxUsage: 92,
            cpuMaxUsage: 90,
            ramMaxUsage: 80,
            cppworkerHint: {
                'CPPWORKER_CTX_SIZE': '65536',
                'CPPWORKER_BATCH_SIZE': '1024',
                'CPPWORKER_GPU_LAYERS': '99',
                'CPPWORKER_KV_CACHE_TYPE': 'f16',
                'CPPWORKER_RAM_FALLBACK_MAX_N_CTX': '262144'
            }
        }
    };

    // Order for dropdown (newest/biggest first)
    const order = ['rtx50-32gb', 'rtx40-24gb', 'a10-24gb', 'rtx30-8gb'];

    window.HardwarePresets = {
        presets: presets,
        order: order,
        get: function(name) { return presets[name] || null; },
        list: function() {
            return order.map(function (key) {
                var p = presets[key];
                return { key: key, label: p.label, description: p.description };
            });
        }
    };
})();
