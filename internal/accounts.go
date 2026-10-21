package main

import (
	"encoding/json"
	"fmt"
	"sync"
)

var accountLocks sync.Map

func lockAccount(name string) func() {
	value, _ := accountLocks.LoadOrStore(name, &sync.Mutex{})
	mu := value.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func findAccountByID(id string) (accountFile, error) {
	if id == "" {
		return accountFile{}, fmt.Errorf("account ID is required")
	}
	files, err := codeartsFiles()
	if err != nil {
		return accountFile{}, err
	}
	var found accountFile
	for _, f := range files {
		if f.ID == id {
			if found.AuthIndex != "" {
				return accountFile{}, fmt.Errorf("ambiguous account ID")
			}
			found = f
		}
	}
	if found.AuthIndex == "" {
		return accountFile{}, fmt.Errorf("account not found")
	}
	return found, nil
}

func saveRefreshedAccount(file accountFile, refreshed pluginAuthData) (json.RawMessage, error) {
	unlock := lockAccount(file.Name)
	defer unlock()
	_, latest, err := accountStorage(file)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(refreshed.StorageJSON, &fields); err != nil {
		return nil, err
	}
	changes := map[string]any{}
	for _, key := range []string{"cloud_dragon_token", "access_key_id", "secret_access_key", "refresh_token", "expiration", "updated_at", "user_id", "user_name", "domain_id"} {
		if value, ok := fields[key]; ok {
			changes[key] = value
		}
	}
	for _, key := range []string{"expired", "refresh_interval_seconds"} {
		if value, ok := refreshed.Metadata[key]; ok {
			changes[key] = value
		}
	}
	updated, err := mergeAccountFields(latest, changes)
	if err != nil {
		return nil, err
	}
	return hostAuthSave(map[string]any{"name": file.Name, "json": json.RawMessage(updated)})
}

func mergeAccountFields(storage []byte, changes map[string]any) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(storage, &fields); err != nil {
		return nil, err
	}
	if fields == nil {
		return nil, fmt.Errorf("invalid account document")
	}
	for key, value := range changes {
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		fields[key] = encoded
	}
	return json.Marshal(fields)
}
