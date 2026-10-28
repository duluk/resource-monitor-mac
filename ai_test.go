package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func mockOpenAI(t *testing.T, status int, reply string, check func(map[string]any)) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if check != nil {
			check(body)
		}
		w.WriteHeader(status)
		w.Write([]byte(reply))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("OPENAI_BASE_URL", srv.URL+"/v1/")
	t.Setenv("OPENAI_API_KEY", "test-key")
}

func TestAIAnalyzeExtractsMessageText(t *testing.T) {
	mockOpenAI(t, 200, `{"status":"completed","output":[
		{"type":"reasoning","summary":[]},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Bottom line: "},{"type":"output_text","text":"memory.\n"}]}]}`,
		func(body map[string]any) {
			if body["model"] != "some-model" {
				t.Errorf("model = %v", body["model"])
			}
			in, _ := body["input"].(string)
			if !strings.Contains(in, "REPORT TEXT") || !strings.Contains(in, "typing lags") {
				t.Errorf("input missing report or note: %q", in)
			}
			if ins, _ := body["instructions"].(string); ins == "" {
				t.Error("missing instructions")
			}
		})
	got, err := aiAnalyze(context.Background(), "some-model", "REPORT TEXT", "typing lags")
	if err != nil {
		t.Fatal(err)
	}
	if got != "Bottom line: memory." {
		t.Errorf("got %q", got)
	}
}

func TestAIAnalyzeReportsAPIError(t *testing.T) {
	mockOpenAI(t, 404, `{"error":{"message":"The model 'nope' does not exist","type":"invalid_request_error"}}`, nil)
	_, err := aiAnalyze(context.Background(), "nope", "r", "")
	if err == nil || !strings.Contains(err.Error(), "does not exist") || !strings.Contains(err.Error(), "404") {
		t.Fatalf("err = %v", err)
	}
}

func TestAIAnalyzeIncomplete(t *testing.T) {
	mockOpenAI(t, 200, `{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"reasoning"}]}`, nil)
	_, err := aiAnalyze(context.Background(), "m", "r", "")
	if err == nil || !strings.Contains(err.Error(), "max_output_tokens") {
		t.Fatalf("err = %v", err)
	}
}
