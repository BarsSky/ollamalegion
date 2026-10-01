// ggufplan — проверка раскладки KV-кэша по реальному GGUF-файлу через
// production-парсер (internal/cppbackend) и production-формулу (internal/memfit).
package main

import (
	"fmt"
	"os"

	"ollama-loadbalancer/internal/cppbackend"
	"ollama-loadbalancer/internal/memfit"
)

func main() {
	path := os.Args[1]
	h, err := cppbackend.ReadGGUFHeader(path)
	if err != nil {
		panic(err)
	}
	fmt.Printf("arch=%s nLayers=%d nEmbd=%d nHeads=%d nKvHeads=%d keyLength=%d ctx=%d\n",
		h.Architecture, h.NLayers, h.NEmbd, h.NHeads, h.NKvHeads, h.KeyLength, h.ContextLength)
	fmt.Printf("sharedKvLayers=%d slidingWindow=%d swaKeyLength=%d swaPatternLen=%d\n",
		h.SharedKVLayers, h.SlidingWindow, h.SWAKeyLength, len(h.SWAPattern))
	fmt.Printf("swaPattern=%s\n", h.SWAPattern)
	plan := h.KVPlan()
	fmt.Printf("PLAN globalLayers=%d globalHeadDim=%d swaLayers=%d swaHeadDim=%d swaWindow=%d exact=%v kvLayersTotal=%d\n",
		plan.GlobalLayers, plan.GlobalHeadDim, plan.SWALayers, plan.SWAHeadDim, plan.SWAWindow, plan.Exact, plan.KVLayers())
	kv, exact := h.KVLayers()
	fmt.Printf("KVLayers()=%d exact=%v\n", kv, exact)

	fi, _ := os.Stat(path)
	meta := &cppbackend.GGUFModelMeta{
		Filename:              path,
		SizeBytes:             fi.Size(),
		Architecture:          h.Architecture,
		NLayers:               h.NLayers,
		NEmbd:                 h.NEmbd,
		NHeads:                h.NHeads,
		NKvHeads:              h.NKvHeads,
		ContextLength:         h.ContextLength,
		KeyLength:             h.KeyLength,
		ValueLength:           h.ValueLength,
		NextNPredictLayers:    h.NextNPredictLayers,
		FullAttentionInterval: h.FullAttentionInterval,
		HasRecurrentLayersKey: h.HasRecurrentLayersKey,
		SharedKVLayers:        h.SharedKVLayers,
		SlidingWindow:         h.SlidingWindow,
		SWAKeyLength:          h.SWAKeyLength,
		SWAPattern:            h.SWAPattern,
	}
	spec := cppbackend.MemfitSpecFromMeta("gemma-4-E4B-it-Q4_K_M", meta)
	fmt.Printf("memfit spec: size=%.0f MiB kvLayers=%d kvSwaLayers=%d swaWindow=%d swaHeadDim=%d kvHeadDim=%d\n",
		float64(spec.SizeBytes)/1024/1024, spec.KVLayers, spec.KVSWALayers, spec.SWAWindow, spec.SWAHeadDim, spec.KVHeadDim)
	perToken := memfit.KVBytesPerToken(spec, memfit.KVQ4)
	for _, ctx := range []int{8192, 32768, 65536, 131072} {
		total := memfit.KVTotalBytes(spec, memfit.KVQ4, ctx)
		fmt.Printf("  ctx=%6d  KVTotal(q4_0)=%8.1f MiB   naive(kvPerToken*ctx)=%8.1f MiB\n",
			ctx, float64(total)/1024/1024, float64(perToken.Mul(int64(ctx)))/1024/1024)
	}
}
