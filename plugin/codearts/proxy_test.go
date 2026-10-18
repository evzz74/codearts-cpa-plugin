package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
)

// valuesWith builds a query carrying at most one parameter.
func valuesWith(key, value string) url.Values {
	query := url.Values{}
	if value != "" {
		query.Set(key, value)
	}
	return query
}

// TestBootstrapCallProxyOnlyAllowsReads pins the security boundary of the
// keyless proxy: it must serve account listings and refuse anything that could
// return secrets or mutate host state.
func TestBootstrapCallProxyOnlyAllowsReads(t *testing.T) {
	oldList := hostAuthList
	defer func() { hostAuthList = oldList }()
	hostAuthList = func() (json.RawMessage, error) {
		return json.Marshal(map[string]any{"files": []map[string]any{{
			"auth_index": "idx-1",
			"name":       "codearts-account1.json",
		}}})
	}
	oldGet := hostAuthGet
	defer func() { hostAuthGet = oldGet }()
	hostAuthGet = func(string) (json.RawMessage, error) {
		return json.Marshal(map[string]any{
			"auth_index": "idx-1",
			"name":       "codearts-account1.json",
			"json":       base64.StdEncoding.EncodeToString([]byte(`{"type":"codearts","user_id":"account1","access_key_id":"ak","secret_access_key":"sk"}`)),
		})
	}

	for _, op := range []string{"accounts", "credits"} {
		status, value := responseBody(t, managementBootstrap("/call", valuesWith("op", op)))
		if status != http.StatusOK {
			t.Fatalf("%s status = %d (%v)", op, status, value)
		}
		if _, isList := value["results"].([]any); !isList {
			t.Fatalf("%s did not return a results list: %v", op, value)
		}
	}

	// Anything that would leak a credential or change state must be refused.
	for _, op := range []string{"credentials", "import", "enabled", "checkin", "keepalive", "", "../accounts"} {
		status, _ := responseBody(t, managementBootstrap("/call", valuesWith("op", op)))
		if status == http.StatusOK {
			t.Fatalf("proxy allowed %q", op)
		}
	}
}

// TestBootstrapCallProxyNeverReturnsSecrets asserts the response body carries no
// credential material even though the underlying store does.
func TestBootstrapCallProxyNeverReturnsSecrets(t *testing.T) {
	oldList, oldGet := hostAuthList, hostAuthGet
	defer func() { hostAuthList, hostAuthGet = oldList, oldGet }()
	hostAuthList = func() (json.RawMessage, error) {
		return json.Marshal(map[string]any{"files": []map[string]any{{"auth_index": "idx-1", "name": "codearts-account1.json"}}})
	}
	hostAuthGet = func(string) (json.RawMessage, error) {
		return json.Marshal(map[string]any{
			"auth_index": "idx-1",
			"name":       "codearts-account1.json",
			"json":       base64.StdEncoding.EncodeToString([]byte(`{"type":"codearts","user_id":"account1","access_key_id":"ak","secret_access_key":"super-secret","refresh_token":"tok"}`)),
		})
	}
	_, value := responseBody(t, managementBootstrap("/call", valuesWith("op", "accounts")))
	joined, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"super-secret", "refresh_token", "secret_access_key"} {
		if containsString(string(joined), forbidden) {
			t.Fatalf("proxy leaked %q: %s", forbidden, joined)
		}
	}
}

func containsString(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
