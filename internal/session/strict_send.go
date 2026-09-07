package session

import (
	"encoding/json"
	"time"
)

// StrictSendIdle reads only hook state; never use pane-diff fallback or treat
// a missing timestamp as fresh (the display-oriented reader permits that).
func StrictSendIdle(instanceID, tool, expectedThread string) bool {
	data, err := readStatusFileNoFollow(hookStatusFilePath(instanceID))
	if err != nil {
		return false
	}
	var raw struct {
		Status     string `json:"status"`
		Thread     string `json:"session_id"`
		Timestamp  int64  `json:"ts"`
		Generation string `json:"hook_generation"`
	}
	if json.Unmarshal(data, &raw) != nil || raw.Timestamp <= 0 || raw.Thread != expectedThread || (raw.Status != "waiting" && raw.Status != "idle") {
		return false
	}
	generation, authority := hookGenerationForInstance(instanceID)
	if !hookGenerationRecordAccepted(raw.Generation, generation, authority) {
		return false
	}
	age := time.Since(time.Unix(raw.Timestamp, 0))
	return age >= 0 && age < hookFastPathFreshnessForTool(tool, raw.Status)
}
