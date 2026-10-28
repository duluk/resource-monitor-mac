package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const defaultAIModel = "gpt-6.1-terra"

const aiInstructions = `You are an expert macOS performance engineer. The user ran a diagnostic tool ("resmon") because their Mac feels slow: apps lag and keyboard input is sometimes delayed. You will get its report: raw measurements, then the tool's own rule-based diagnosis.

Write your own analysis for the user:
- Open with a one- or two-sentence bottom line: is the Mac resource-constrained, and by what?
- Explain the evidence using specific numbers from the report, and connect it to what the user feels (lag, delayed typing, slow app switching).
- Name the specific apps and processes responsible, and anything notable about them (e.g. a single process that's unexpectedly large).
- Say what looks fine, so they don't chase it.
- Give a prioritized, numbered list of concrete actions, most effective and cheapest first. Say whether more RAM is genuinely warranted.
- Point out anything the rule-based diagnosis missed, overstated or got wrong, and any limits of the data (e.g. not run as root, short sample).

Be direct and specific. Do not invent data that isn't in the report. Format for a terminal: plain text, short paragraphs, "-" bullets and numbered lists. No markdown headings, bold, or tables. Stay under about 450 words.`

// aiAnalyze sends the plain-text report to the OpenAI Responses API and returns the model's analysis.
func aiAnalyze(ctx context.Context, model, report, note string) (string, error) {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		return "", errors.New("OPENAI_API_KEY is not set")
	}
	base := strings.TrimRight(cmp.Or(os.Getenv("OPENAI_BASE_URL"), "https://api.openai.com/v1"), "/")

	input := "resmon report:\n\n" + report
	if note != "" {
		input = "What the user says about the problem: " + note + "\n\n" + input
	}
	body, err := json.Marshal(map[string]any{
		"model":        model,
		"instructions": aiInstructions,
		"input":        input,
	})
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/responses", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	var out struct {
		Status string
		Error  *struct{ Message string }
		Output []struct {
			Type    string
			Content []struct{ Type, Text string }
		}
		IncompleteDetails *struct{ Reason string } `json:"incomplete_details"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("OpenAI returned %s: %s", resp.Status, truncBytes(raw, 300))
	}
	if out.Error != nil && out.Error.Message != "" {
		return "", fmt.Errorf("OpenAI returned %s: %s", resp.Status, out.Error.Message)
	}
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("OpenAI returned %s: %s", resp.Status, truncBytes(raw, 300))
	}
	var text strings.Builder
	for _, item := range out.Output {
		if item.Type != "message" {
			continue
		}
		for _, c := range item.Content {
			if c.Type == "output_text" {
				text.WriteString(c.Text)
			}
		}
	}
	if text.Len() == 0 {
		if out.IncompleteDetails != nil {
			return "", fmt.Errorf("OpenAI response incomplete (%s) with no text", out.IncompleteDetails.Reason)
		}
		return "", fmt.Errorf("OpenAI response (status %q) contained no text", out.Status)
	}
	return strings.TrimSpace(text.String()), nil
}

func truncBytes(b []byte, n int) string {
	s := strings.TrimSpace(string(b))
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// printAI prints the model's text, re-wrapping long lines while keeping list
// items' hanging indent.
func printAI(w io.Writer, model, text string, st style) {
	fmt.Fprintln(w)
	fmt.Fprintln(w, st.bold(strings.Repeat("━", 36)+" AI ANALYSIS ("+model+") "+strings.Repeat("━", 36)))
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimLeft(line, " ")
		indent := len(line) - len(trimmed)
		hang := indent
		if i := strings.Index(trimmed, " "); i > 0 && i <= 4 && (trimmed[0] == '-' || trimmed[0] == '*' || (trimmed[0] >= '0' && trimmed[0] <= '9')) {
			hang += i + 1
		}
		wrapped := wrapText(trimmed, 96-indent)
		if len(wrapped) == 0 {
			fmt.Fprintln(w)
			continue
		}
		fmt.Fprintln(w, "  "+strings.Repeat(" ", indent)+wrapped[0])
		for _, l := range wrapText(strings.Join(wrapped[1:], " "), 96-hang) {
			fmt.Fprintln(w, "  "+strings.Repeat(" ", hang)+l)
		}
	}
	fmt.Fprintln(w, st.dim("  (AI-generated from the report above; double-check before acting on it.)"))
}
