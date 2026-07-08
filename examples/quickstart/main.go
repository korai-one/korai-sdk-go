// Quickstart for the Korai Go SDK.
//
// Run:
//
//	cd korai-platform/packages/sdk-go/examples/quickstart
//	go run main.go --prompt "Explique-moi la TVA suisse en 3 phrases."
//
// The example demonstrates the four most common SDK calls:
//
//  1. Constructing a Client with WithAPIKey / WithBaseURL.
//  2. Listing the models advertised by the orchestrator's fleet.
//  3. Making a non-streaming chat completion.
//  4. Making a streaming chat completion and consuming the channel.
//  5. Registering a Go-native tool and producing the OpenAI / Anthropic schemas.
//
// The Korai API key is read from KORAI_API_KEY (preferred) or the
// --api-key flag.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	korai "github.com/korai-one/korai-sdk-go"
)

func main() {
	prompt := flag.String("prompt", "Bonjour, Korai !", "User prompt to send")
	model := flag.String("model", "auto", "Model alias or canonical ID")
	apiKey := flag.String("api-key", os.Getenv("KORAI_API_KEY"), "Korai Cloud API key")
	baseURL := flag.String("base-url", korai.DefaultBaseURL, "Korai Cloud base URL")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))

	cli := korai.New(
		korai.WithAPIKey(*apiKey),
		korai.WithBaseURL(*baseURL),
		korai.WithTimeout(60*time.Second),
	)

	// 1. Register a small tool just to demonstrate the schema export —
	// this isn't sent to the orchestrator unless you pass it on a
	// ChatRequest.
	cli.Tools.Register(korai.FuncTool{
		NameValue: "get_time",
		DescValue: "Return the current ISO-8601 UTC time",
		Schema:    map[string]any{"type": "object"},
		Fn: func(ctx context.Context, _ json.RawMessage) (*korai.ToolResult, error) {
			return &korai.ToolResult{Output: time.Now().UTC().Format(time.RFC3339)}, nil
		},
	})
	logger.Info("registered tools", "names", cli.Tools.Names())

	ctx := context.Background()

	// 2. List models. Some endpoints are open (no auth needed).
	if ids, err := cli.ListModels(ctx); err == nil {
		logger.Info("models advertised by orchestrator", "count", len(ids), "first", firstN(ids, 5))
	} else {
		logger.Warn("ListModels failed", "err", err)
	}

	// 3. Non-streaming completion.
	resp, err := cli.ChatComplete(ctx, korai.ChatRequest{
		Model:     *model,
		MaxTokens: 256,
		Messages: []korai.Message{
			{Role: "user", Content: *prompt},
		},
	})
	if err != nil {
		logger.Error("ChatComplete failed", "err", err)
	} else {
		fmt.Println("─── Non-streaming reply ───")
		if len(resp.Choices) > 0 {
			fmt.Println(resp.Choices[0].Message.Content)
		}
		fmt.Printf("usage: prompt=%d completion=%d total=%d\n",
			resp.Usage.PromptTokens, resp.Usage.CompletionTokens, resp.Usage.TotalTokens)
	}

	// 4. Streaming completion. The channel closes when the stream ends.
	fmt.Println("\n─── Streaming reply ───")
	streamCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	events, err := cli.ChatStream(streamCtx, korai.ChatRequest{
		Model:     *model,
		MaxTokens: 256,
		Messages: []korai.Message{
			{Role: "user", Content: *prompt + " (stream)"},
		},
	})
	if err != nil {
		logger.Error("ChatStream failed", "err", err)
	} else {
		for ev := range events {
			switch ev.Type {
			case "content":
				io.WriteString(os.Stdout, ev.Delta)
			case "error":
				logger.Error("stream error", "msg", ev.Error)
			}
		}
		fmt.Println()
	}
}

func firstN(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
