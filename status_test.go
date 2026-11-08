package main

import (
	"encoding/json"
	"testing"
)

func TestJSONStatusKeepsTheVersionedEnvelopeShape(t *testing.T) {
	envelope := statusEnvelope{
		SchemaVersion: "gitdcy.response.v1", Component: "katteke.gitdcy", Operation: "status", Outcome: "success",
		Data: statusData{Repos: []statusRepo{{
			ID: "github/example", Path: "/workspace/example", Branch: ptr("main"), TrackingBranch: ptr("origin/main"),
			Ahead: ptr(uint32(0)), Behind: ptr(uint32(0)), IncomingWipTrusted: true, Safety: "public-safe",
			IdentityState: "identity-unchecked", State: "clean",
		}}},
		CorrelationID: "test-correlation",
	}
	var value map[string]any
	if err := json.Unmarshal([]byte(jsonPretty(envelope)), &value); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]any{"schema_version": "gitdcy.response.v1", "component": "katteke.gitdcy", "operation": "status", "outcome": "success", "correlation_id": "test-correlation"} {
		if value[key] != want {
			t.Errorf("%s = %v", key, value[key])
		}
	}
	repos := value["data"].(map[string]any)["repos"].([]any)
	repo := repos[0].(map[string]any)
	if repo["identity_state"] != "identity-unchecked" || repo["identity_enforced"] != false || repo["identity_blocked"] != false {
		t.Errorf("%v", repo)
	}
	if _, ok := value["warnings"].([]any); !ok {
		t.Error("warnings is not an array")
	}
	if _, ok := value["error"]; !ok {
		t.Error("no error field")
	}
}
