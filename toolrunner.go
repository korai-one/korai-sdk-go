package korai

import (
	"context"
	"encoding/json"
)

// RunToolsOptions configures the agentic tool-use loop (RunTools).
type RunToolsOptions struct {
	// Messages is the conversation so far (at least one user turn).
	Messages []Message
	// Model is the alias or canonical id; defaults to "auto".
	Model string
	// System is prepended as a system message on the first turn only.
	System string
	// Temperature is forwarded to the model when non-zero.
	Temperature float64
	// MaxTurns caps model⇄tool round-trips. Defaults to 5.
	MaxTurns int
}

// ToolRunResult records the execution of one tool call inside the loop.
type ToolRunResult struct {
	ToolCallID string
	Name       string
	// Output is the JSON the tool returned, or {"error": "..."} if the
	// tool was missing or failed — the same bytes fed back to the model.
	Output json.RawMessage
}

// RunToolsResult is the outcome of RunTools.
type RunToolsResult struct {
	// Messages is the full conversation, including assistant tool-call
	// turns and the role="tool" result messages.
	Messages []Message
	// Final is the last assistant completion (the one with no further
	// tool calls, unless StoppedAtMaxTurns).
	Final *ChatResponse
	// Turns is the number of model turns taken.
	Turns int
	// StoppedAtMaxTurns is true if the loop hit MaxTurns while the model
	// still wanted to call tools.
	StoppedAtMaxTurns bool
	// ToolRuns is the flat list of every tool execution, in order.
	ToolRuns []ToolRunResult
}

// RunTools drives the agentic tool-use loop: it sends the conversation
// plus the registered tool schemas to the model, executes any tool calls
// the model returns locally via c.Tools, feeds the results back as
// role="tool" messages, and repeats until the model answers without
// requesting tools (or MaxTurns is reached).
//
// This is the hand-written ergonomic layer on top of the generated
// chat-completion core — the Go analogue of OpenAI's runTools. Tool
// execution happens in-process via the registry; a tool error (missing
// tool or Execute failure) is fed back to the model as {"error": ...}
// rather than aborting the loop.
//
// Example:
//
//	cli.Tools.Register(myAddTool)
//	res, err := cli.RunTools(ctx, korai.RunToolsOptions{
//	    Messages: []korai.Message{{Role: "user", Content: "what is 2+3?"}},
//	})
//	fmt.Println(res.Final.Choices[0].Message.Content)
func (c *Client) RunTools(ctx context.Context, opts RunToolsOptions) (*RunToolsResult, error) {
	maxTurns := opts.MaxTurns
	if maxTurns <= 0 {
		maxTurns = 5
	}

	schemas := c.Tools.ToOpenAISchemas()
	toolsAny := make([]any, len(schemas))
	for i, s := range schemas {
		toolsAny[i] = s
	}

	msgs := append([]Message{}, opts.Messages...)
	out := &RunToolsResult{}

	for turn := 0; turn < maxTurns; turn++ {
		req := ChatRequest{
			Messages:    msgs,
			Model:       opts.Model,
			Temperature: opts.Temperature,
			Tools:       toolsAny,
		}
		if turn == 0 && opts.System != "" {
			req.System = opts.System
		}

		resp, err := c.ChatComplete(ctx, req)
		if err != nil {
			return nil, err
		}
		out.Final = resp

		var content string
		var calls []ToolCall
		if len(resp.Choices) > 0 {
			content = resp.Choices[0].Message.Content
			calls = resp.Choices[0].Message.ToolCalls
		}
		msgs = append(msgs, Message{Role: "assistant", Content: content, ToolCalls: calls})

		// No tools requested → the model has answered.
		if len(calls) == 0 {
			out.Messages = msgs
			out.Turns = turn + 1
			return out, nil
		}

		// Execute each requested tool, feeding the result (or error) back.
		for _, call := range calls {
			inputRaw, _ := json.Marshal(call.Input)
			var outputRaw json.RawMessage
			if res, err := c.Tools.Invoke(ctx, call.Name, inputRaw); err != nil {
				outputRaw, _ = json.Marshal(map[string]string{"error": err.Error()})
			} else {
				outputRaw, _ = json.Marshal(res)
			}
			out.ToolRuns = append(out.ToolRuns, ToolRunResult{
				ToolCallID: call.ID,
				Name:       call.Name,
				Output:     outputRaw,
			})
			msgs = append(msgs, Message{
				Role:       "tool",
				Name:       call.Name,
				ToolCallID: call.ID,
				Content:    string(outputRaw),
			})
		}
	}

	out.Messages = msgs
	out.Turns = maxTurns
	out.StoppedAtMaxTurns = true
	return out, nil
}
