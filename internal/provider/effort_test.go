package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/qizhaoTan/Reviewer-AI/internal/schema"
)

func TestReasoningEffortRequests(t *testing.T) {
	for _, protocol := range []string{"", "openai", "OpenAI", "anthropic", "Anthropic"} {
		for _, effort := range []string{"", "none", "minimal", "low", "medium", "high", "xhigh", "max", "HIGH", " high ", "invalid"} {
			t.Run(protocol+"/"+effort, func(t *testing.T) {
				isAnthropic := strings.EqualFold(protocol, "anthropic")
				invalid := effort == "HIGH" || effort == " high " || effort == "invalid" ||
					(isAnthropic && (effort == "none" || effort == "minimal"))
				requests := make(chan map[string]any, 2)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					requests <- body
					w.Header().Set("Content-Type", "application/json")
					if isAnthropic {
						fmt.Fprint(w, `{"id":"msg-test","type":"message","role":"assistant","model":"test","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
					} else {
						fmt.Fprint(w, `{"id":"chat-test","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
					}
				}))
				defer server.Close()
				p, err := New(Config{Provider: protocol, APIKey: "test", BaseURL: server.URL, Model: "test", MaxTokens: 4096, ReasoningEffort: effort})
				if invalid {
					if err == nil || !strings.Contains(err.Error(), "reasoning_effort") || !strings.Contains(err.Error(), "请使用") {
						t.Fatalf("expected actionable validation error, got %v", err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				// Repeated calls must retain the per-model setting.
				for range 2 {
					result, err := p.Generate(context.Background(), []schema.Message{{Role: schema.RoleUser, Content: "review"}}, nil)
					if err != nil {
						t.Fatal(err)
					}
					if result.Content != "ok" {
						t.Fatalf("unexpected response: %+v", result)
					}
					body := <-requests
					if _, ok := body["thinking"]; ok {
						t.Fatal("effort must not change the thinking switch")
					}
					key := "reasoning_effort"
					if isAnthropic {
						key = "output_config"
						if body["max_tokens"] != float64(4096) {
							t.Fatalf("max_tokens changed: %v", body)
						}
						if _, ok := body["reasoning_effort"]; ok {
							t.Fatal("OpenAI field leaked into Anthropic request")
						}
					} else if _, ok := body["output_config"]; ok {
						t.Fatal("Anthropic field leaked into OpenAI request")
					}
					value, present := body[key]
					if effort == "" {
						if present {
							t.Fatalf("unset effort must omit %s, got %v", key, value)
						}
					} else {
						if isAnthropic {
							obj, ok := value.(map[string]any)
							if !ok {
								t.Fatalf("missing output_config: %v", body)
							}
							value = obj["effort"]
						}
						if value != effort {
							t.Fatalf("effort = %v, want %q", value, effort)
						}
					}
				}
			})
		}
	}
}
