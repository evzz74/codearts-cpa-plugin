package main

// The executor owns local accounting; host callbacks lack an ID for exact deduplication.
func handleUsageRecord(_ []byte) []byte {
	return okEnvelope(map[string]any{"ok": true, "skipped": true})
}
