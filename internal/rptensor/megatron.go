package rptensor

import "fmt"

// ApplyMegatronPartition — convenience wrapper для Megatron-LM style partitioning.
//
// Применяет PartitionStrategy "megatron" к модели. Эквивалентно вызову
// model.Partition(MegatronPartition, model.WorldSize), но без явного
// указания worldSize (берётся из model.WorldSize, который должен быть
// установлен заранее через первый вызов Partition с явным worldSize).
//
// Контракт:
//   - hidden_size, intermediate_size, vocab_size должны делиться на worldSize.
//   - num_attention_heads должен делиться на worldSize (для column-split).
//
// Раскладка (Megatron-LM):
//
//   Q/K/V/O: column split → [hidden, hidden/worldSize] для каждого rank'а.
//   MLP_gate, MLP_up: row split → [hidden, ffn/worldSize].
//   MLP_down: column split → [ffn/worldSize, hidden].
//   LM_head: column split → [vocab/worldSize, hidden].
//   embed_tokens: replicated → [vocab, hidden] (без split).
//
// После каждого слоя требуется all-reduce. Это выполняется через
// AllReduceFunc в TensorParallelCoordinator.executeLayer.
//
// Использование:
//
//	model, _ := NewShardedModel("llama-3-8b", 4096, 14336, 32, 32, 128256)
//	model.WorldSize = 4  // явное указание.
//	if err := ApplyMegatronPartition(model); err != nil {
//	    return err
//	}
//
// Более простой вариант — model.Partition(MegatronPartition, 4).
//
// Альтернативный entry point: model.Partition(ColumnPartition, 4) или
// model.Partition(RowPartition, 4) для других стратегий.
func ApplyMegatronPartition(model *ShardedModel) error {
	if model == nil {
		return fmt.Errorf("rptensor: model is nil")
	}
	return model.Partition(MegatronPartition, model.WorldSize)
}
