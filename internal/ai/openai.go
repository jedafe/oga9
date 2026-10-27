// Ref: oga9-engineering/sections/22-ai.md — provider adapter. The LLM
// contract is an OpenAI-compatible /chat/completions endpoint so any
// self-hosted or hosted model works (Rule 29: deployment must keep
// working without the OGA9 control plane — the provider is operator-set).
package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/jedafe/oga9/internal/tenant"
)

// OpenAICompat adapts any OpenAI-compatible chat-completions endpoint
// (Ollama, vLLM, OpenAI, etc.) to the provider seams.
type OpenAICompat struct {
	url, key, model string
	client          *http.Client
}

func NewOpenAICompat(url, key, model string) *OpenAICompat {
	return &OpenAICompat{url: url, key: key, model: model,
		client: &http.Client{Timeout: 60 * time.Second}}
}

type chatMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponse struct {
	Choices []struct {
		Message chatMsg `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

func (o *OpenAICompat) call(ctx context.Context, system, user string) (string, int, int, error) {
	body, _ := json.Marshal(map[string]any{
		"model":    o.model,
		"messages": []chatMsg{{Role: "system", Content: system}, {Role: "user", Content: user}},
	})
	req, err := http.NewRequestWithContext(ctx, "POST", o.url, bytes.NewReader(body))
	if err != nil {
		return "", 0, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if o.key != "" {
		req.Header.Set("Authorization", "Bearer "+o.key)
	}
	resp, err := o.client.Do(req)
	if err != nil {
		return "", 0, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", 0, 0, err
	}
	if resp.StatusCode != 200 {
		return "", 0, 0, fmt.Errorf("ai provider %d: %s", resp.StatusCode, truncateForLog(string(raw)))
	}
	var out chatResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", 0, 0, err
	}
	if len(out.Choices) == 0 {
		return "", 0, 0, fmt.Errorf("ai provider returned no choices")
	}
	return out.Choices[0].Message.Content, out.Usage.PromptTokens, out.Usage.CompletionTokens, nil
}

// Chat — the provider seam for Chat(): persona sets the system prompt.
// Citations are empty until the context-assembly seam lands (§22 notes
// permission-scoped grounding rides the seam, not the provider).
func (o *OpenAICompat) Chat(ctx context.Context, oc *tenant.OrgCtx, persona, question string) (*Answer, error) {
	system := personaPrompt(persona)
	body, tin, tout, err := o.call(ctx, system, question)
	if err != nil {
		return nil, err
	}
	return &Answer{Body: body, Citations: []Citation{}, TokensIn: tin, TokensOut: tout}, nil
}

// Draft — the provider seam for Draft(): returns the generation draft
// as raw JSON (the provider returns text; callers keep it verbatim).
func (o *OpenAICompat) Draft(ctx context.Context, oc *tenant.OrgCtx, kind, prompt string) (json.RawMessage, int, int, error) {
	system := fmt.Sprintf("You draft %s for an organization. Return only the draft body.", kind)
	body, tin, tout, err := o.call(ctx, system, prompt)
	if err != nil {
		return nil, 0, 0, err
	}
	raw, _ := json.Marshal(map[string]string{"title": kind, "body": body})
	return raw, tin, tout, nil
}

func personaPrompt(p string) string {
	switch p {
	case "secretary":
		return "You are the organization's secretary assistant — minutes, notices, records. Answer concisely."
	case "treasurer":
		return "You are the organization's treasurer assistant — dues, levies, reports. Answer concisely and never reveal figures outside the asker's permission scope."
	case "historian":
		return "You are the organization's historian — past terms, precedents, constitutional context. Answer concisely."
	default:
		return "You are the organization's member assistant. Answer concisely."
	}
}

func truncateForLog(s string) string {
	if len(s) > 200 {
		return s[:200]
	}
	return s
}
