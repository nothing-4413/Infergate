package sse

import (
	"bytes"
	"encoding/json"
)

// Usage is the token accounting a provider reports for one request.
//
// The three counters are deliberately kept apart: prompt tokens drive input
// cost, completion tokens drive output cost and are the ones a budget policy
// can actually cut short mid-stream, and cached tokens are billed at a discount
// by most providers, so folding them into the prompt total would overstate
// spend.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	CachedTokens     int `json:"cached_tokens"`
}

// Empty reports whether no counters were observed.
func (u Usage) Empty() bool {
	return u.PromptTokens == 0 && u.CompletionTokens == 0 && u.TotalTokens == 0 && u.CachedTokens == 0
}

// merge folds non-zero counters from src into u, so a provider that reports
// counters across several frames still yields one coherent total.
func (u *Usage) merge(src Usage) {
	if src.PromptTokens != 0 {
		u.PromptTokens = src.PromptTokens
	}
	if src.CompletionTokens != 0 {
		u.CompletionTokens = src.CompletionTokens
	}
	if src.TotalTokens != 0 {
		u.TotalTokens = src.TotalTokens
	}
	if src.CachedTokens != 0 {
		u.CachedTokens = src.CachedTokens
	}
}

// ToolCall is one tool/function invocation extracted from a stream.
//
// OpenAI-style providers split a tool call across many frames: the first delta
// carries the id and function name, later deltas carry fragments of the JSON
// arguments. A relay that ignores this cannot report "the agent called
// search_web" and cannot enforce a per-tool budget, so the parsing lives here
// rather than in application code.
type ToolCall struct {
	Index int
	ID    string
	Name  string
	Args  []byte
}

// Observation is what Inspector learned from one frame.
type Observation struct {
	// Usage carries any token counters found in the frame.
	Usage Usage

	// Model is the model the provider says it served. Providers may resolve an
	// alias to a concrete snapshot name, so the response is the authoritative
	// source for pricing lookups.
	Model string

	// ToolCalls are the tool-call deltas present in this frame, keyed by index.
	ToolCalls map[int]*ToolCall

	// FinishReason is set once the provider signals why generation stopped.
	FinishReason string

	// Err is an upstream error delivered inside a 200 OK stream body. OpenAI
	// and its clones signal mid-stream failures this way rather than by status
	// code, so a relay that only checks status codes reports success on a
	// failed generation.
	Err *StreamError

	// JSON reports whether Data parsed as a JSON object.
	JSON bool
}

// StreamError is the error envelope carried inside a stream.
type StreamError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    any    `json:"code"`
	Param   any    `json:"param"`
}

func (e *StreamError) Error() string {
	if e == nil {
		return "<nil>"
	}
	switch {
	case e.Type != "" && e.Message != "":
		return e.Type + ": " + e.Message
	case e.Message != "":
		return e.Message
	default:
		return "upstream stream error"
	}
}

// Inspector accumulates observations across the frames of one response.
//
// It is exercised on the relay path, so it must never allocate for a frame it
// cannot parse and must never panic on malformed input: a gateway that dies on
// a truncated JSON delta is worse than one that reports an unaccounted stream.
type Inspector struct {
	usage        Usage
	model        string
	toolCalls    map[int]*ToolCall
	finishReason string
	streamErr    *StreamError
	frames       int
	jsonFrames   int
}

// NewInspector returns an Inspector ready for a single stream.
func NewInspector() *Inspector {
	return &Inspector{toolCalls: map[int]*ToolCall{}}
}

// Observe folds one frame into the accumulated state and returns what this
// frame alone contributed.
func (in *Inspector) Observe(f *Frame) Observation {
	in.frames++
	obs := Observation{}
	if len(f.Data) == 0 || f.IsDone() {
		return obs
	}

	// OpenAI-compatible providers always send a JSON object. Some ship a bare
	// JSON fragment in non-conforming deployments; tolerate it rather than
	// dropping accounting on the floor.
	payload := f.Data
	if len(payload) > 0 && payload[0] != '{' {
		payload = append([]byte{'{'}, payload...)
	}

	var env struct {
		Model   string `json:"model"`
		Usage   *struct {
			PromptTokens        int `json:"prompt_tokens"`
			CompletionTokens    int `json:"completion_tokens"`
			TotalTokens         int `json:"total_tokens"`
			PromptTokensDetails *struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
		Error   *StreamError `json:"error"`
		Choices []struct {
			FinishReason *string `json:"finish_reason"`
			Delta        *struct {
				ToolCalls []struct {
					Index    *int   `json:"index"`
					ID       string `json:"id"`
					Function *struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		return obs
	}
	obs.JSON = true
	in.jsonFrames++

	if env.Model != "" {
		obs.Model = env.Model
		in.model = env.Model
	}
	if env.Usage != nil {
		u := Usage{
			PromptTokens:     env.Usage.PromptTokens,
			CompletionTokens: env.Usage.CompletionTokens,
			TotalTokens:      env.Usage.TotalTokens,
		}
		if env.Usage.PromptTokensDetails != nil {
			u.CachedTokens = env.Usage.PromptTokensDetails.CachedTokens
		}
		obs.Usage = u
		in.usage.merge(u)
	}
	if env.Error != nil {
		obs.Err = env.Error
		in.streamErr = env.Error
	}
	for _, ch := range env.Choices {
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			obs.FinishReason = *ch.FinishReason
			in.finishReason = *ch.FinishReason
		}
		if ch.Delta == nil {
			continue
		}
		for _, tc := range ch.Delta.ToolCalls {
			if obs.ToolCalls == nil {
				obs.ToolCalls = map[int]*ToolCall{}
			}
			idx := 0
			if tc.Index != nil {
				idx = *tc.Index
			}
			acc := in.toolCalls[idx]
			if acc == nil {
				acc = &ToolCall{Index: idx}
				in.toolCalls[idx] = acc
			}
			if tc.ID != "" {
				acc.ID = tc.ID
			}
			if tc.Function != nil {
				if tc.Function.Name != "" {
					acc.Name = tc.Function.Name
				}
				if tc.Function.Arguments != "" {
					acc.Args = append(acc.Args, tc.Function.Arguments...)
				}
			}
			cp := *acc
			cp.Args = bytes.Clone(acc.Args)
			obs.ToolCalls[idx] = &cp
		}
	}
	return obs
}

// Usage reports the accumulated token accounting.
func (in *Inspector) Usage() Usage { return in.usage }

// Model reports the model named by the provider, if any frame declared one.
func (in *Inspector) Model() string { return in.model }

// ToolCalls returns the accumulated tool calls indexed by their position.
func (in *Inspector) ToolCalls() map[int]*ToolCall { return in.toolCalls }

// FinishReason reports the terminal reason, if the stream reached one.
func (in *Inspector) FinishReason() string { return in.finishReason }

// Err reports an error envelope seen inside the stream.
func (in *Inspector) Err() *StreamError { return in.streamErr }

// Frames reports how many frames were observed, and how many parsed as JSON.
func (in *Inspector) Frames() (total, jsonFrames int) { return in.frames, in.jsonFrames }

// UsageFromResponse extracts token accounting from a non-streaming completion
// body. It returns false when the body is not a JSON object or carries no usage
// block, which is normal for e.g. /v1/models.
func UsageFromResponse(body []byte) (Usage, string, bool) {
	var env struct {
		Model string `json:"model"`
		Usage *struct {
			PromptTokens        int `json:"prompt_tokens"`
			CompletionTokens    int `json:"completion_tokens"`
			TotalTokens         int `json:"total_tokens"`
			PromptTokensDetails *struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &env); err != nil || env.Usage == nil {
		return Usage{}, "", false
	}
	u := Usage{
		PromptTokens:     env.Usage.PromptTokens,
		CompletionTokens: env.Usage.CompletionTokens,
		TotalTokens:      env.Usage.TotalTokens,
	}
	if env.Usage.PromptTokensDetails != nil {
		u.CachedTokens = env.Usage.PromptTokensDetails.CachedTokens
	}
	return u, env.Model, true
}
