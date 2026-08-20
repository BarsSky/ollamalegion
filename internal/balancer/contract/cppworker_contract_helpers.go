// Round 51.5 (2026-08-20): helper for live integration tests.
// Lives outside _test.go build tags so it can be used by both L1 (llama_stub)
// and L2 (integration) test files.

package contract

import "testing"

// getTestPayload — convenience wrapper around AllEndpoints[i].PayloadBuilder.
// Returns nil for endpoints without a payload builder.
func getTestPayload(t *testing.T, epName string) map[string]interface{} {
	t.Helper()
	for _, ep := range AllEndpoints {
		if ep.Name == epName {
			if ep.PayloadBuilder == nil {
				return nil
			}
			return ep.PayloadBuilder()
		}
	}
	t.Fatalf("getTestPayload: endpoint %q not found in AllEndpoints", epName)
	return nil
}
