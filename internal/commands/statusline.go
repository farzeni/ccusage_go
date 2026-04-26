package commands

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sdpower/ccusage-go/internal/calculator"
	"github.com/sdpower/ccusage-go/internal/loader"
	"github.com/sdpower/ccusage-go/internal/pricing"
	"github.com/sdpower/ccusage-go/internal/types"
	"github.com/spf13/cobra"
)

// statuslineHookData is the JSON structure provided by Claude Code hooks via stdin.
type statuslineHookData struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Model          struct {
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
	} `json:"model"`
	Cost *struct {
		TotalCostUSD float64 `json:"total_cost_usd"`
	} `json:"cost"`
	ContextWindow *struct {
		InputTokens   int `json:"input_tokens"`
		OutputTokens  int `json:"output_tokens"`
		ContextWindow int `json:"context_window"`
	} `json:"context_window"`
}

// statuslineCache is persisted to /tmp for fast repeated calls.
type statuslineCache struct {
	TranscriptMtime int64  `json:"transcript_mtime"`
	Output          string `json:"output"`
}

const (
	statuslineLowThreshold    = 50
	statuslineMediumThreshold = 80
	burnRateLow               = 2000.0 // tokens/min
	burnRateMedium            = 5000.0 // tokens/min
)

func NewStatuslineCommand() *cobra.Command {
	var (
		visualBurnRate         string
		costSource             string
		contextLowThreshold    int
		contextMediumThreshold int
		dataPath               string
		useCache               bool
	)

	cmd := &cobra.Command{
		Use:   "statusline",
		Short: "Display compact status line for Claude Code hooks",
		Long: `Display a compact single-line status summary suitable for use as a Claude Code
status line hook. Reads hook JSON from stdin and outputs a formatted status line.

Example Claude Code configuration:
  {
    "statusLine": {
      "type": "command",
      "command": "ccusage statusline",
      "padding": 0
    }
  }`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// Validate threshold ordering
			if contextLowThreshold >= contextMediumThreshold {
				return fmt.Errorf("--context-low-threshold (%d) must be less than --context-medium-threshold (%d)",
					contextLowThreshold, contextMediumThreshold)
			}

			// Read stdin
			stdinBytes, err := io.ReadAll(os.Stdin)
			if err != nil {
				return fmt.Errorf("failed to read stdin: %w", err)
			}
			if len(stdinBytes) == 0 {
				return fmt.Errorf("no input provided on stdin")
			}

			var hookData statuslineHookData
			if err := json.Unmarshal(stdinBytes, &hookData); err != nil {
				return fmt.Errorf("invalid JSON input: %w", err)
			}

			// Try cache first
			if useCache && hookData.SessionID != "" && hookData.TranscriptPath != "" {
				if cached := readStatuslineCache(hookData.SessionID, hookData.TranscriptPath); cached != "" {
					fmt.Print(cached)
					return nil
				}
			}

			// Resolve data path
			if dataPath == "" {
				dataPath = getDefaultDataPath()
			}

			// Load usage data
			pricingService := pricing.NewService()
			calc := calculator.New(pricingService)
			dataLoader := loader.New()

			entries, err := dataLoader.LoadFromPath(cmd.Context(), dataPath)
			if err != nil {
				return fmt.Errorf("failed to load usage data: %w", err)
			}

			if len(entries) > 0 {
				entries, err = calc.CalculateCosts(cmd.Context(), entries)
				if err != nil {
					return fmt.Errorf("failed to calculate costs: %w", err)
				}
			}

			// Build output
			output := buildStatusline(hookData, entries, calc, statuslineOptions{
				visualBurnRate:         visualBurnRate,
				costSource:             costSource,
				contextLowThreshold:    contextLowThreshold,
				contextMediumThreshold: contextMediumThreshold,
			})

			// Write cache
			if useCache && hookData.SessionID != "" && hookData.TranscriptPath != "" {
				writeStatuslineCache(hookData.SessionID, hookData.TranscriptPath, output)
			}

			fmt.Print(output)
			return nil
		},
	}

	cmd.Flags().StringVar(&visualBurnRate, "visual-burn-rate", "off", "Burn rate visualization: off, emoji, text, emoji-text")
	cmd.Flags().StringVar(&costSource, "cost-source", "auto", "Session cost source: auto, cc, ccusage, both")
	cmd.Flags().IntVar(&contextLowThreshold, "context-low-threshold", statuslineLowThreshold, "Context % threshold for green (0-100)")
	cmd.Flags().IntVar(&contextMediumThreshold, "context-medium-threshold", statuslineMediumThreshold, "Context % threshold for yellow (0-100)")
	cmd.Flags().StringVar(&dataPath, "data-path", "", "Path to Claude data directory")
	cmd.Flags().BoolVar(&useCache, "cache", true, "Cache output to avoid recomputation when transcript unchanged")

	return cmd
}

type statuslineOptions struct {
	visualBurnRate         string
	costSource             string
	contextLowThreshold    int
	contextMediumThreshold int
}

func buildStatusline(hookData statuslineHookData, entries []types.UsageEntry, calc *calculator.Calculator, opts statuslineOptions) string {
	now := time.Now()
	todayKey := now.Format("2006-01-02")

	// --- Today's total cost ---
	var todayCost float64
	for _, e := range entries {
		key := e.DateKey
		if key == "" {
			key = e.Timestamp.Format("2006-01-02")
		}
		if key == todayKey {
			todayCost += e.Cost
		}
	}

	// --- Session cost ---
	var ccCost *float64
	if hookData.Cost != nil {
		v := hookData.Cost.TotalCostUSD
		ccCost = &v
	}

	var ccusageCost float64
	for _, e := range entries {
		if e.SessionID == hookData.SessionID {
			ccusageCost += e.Cost
		}
	}

	sessionCostStr := formatSessionCost(opts.costSource, ccCost, ccusageCost)

	// --- Active block ---
	var blockStr string
	var burnRateStr string

	if len(entries) > 0 {
		blocks := calc.IdentifySessionBlocks(entries, calculator.DefaultSessionDurationHours)
		var activeBlock *types.SessionBlock
		for i := range blocks {
			if blocks[i].IsActive {
				activeBlock = &blocks[i]
				break
			}
		}

		if activeBlock != nil {
			remaining := activeBlock.EndTime.Sub(now)
			blockStr = fmt.Sprintf("$%.2f block (%s)", activeBlock.CostUSD, formatRemaining(remaining))

			if br := calculator.CalculateBurnRate(*activeBlock); br != nil {
				burnRateStr = formatBurnRate(br.CostPerHour, br.TokensPerMinuteForIndicator, opts.visualBurnRate)
			}
		} else {
			blockStr = "No active block"
		}
	} else {
		blockStr = "No active block"
	}

	// --- Context usage ---
	contextStr := formatContextUsage(hookData, opts.contextLowThreshold, opts.contextMediumThreshold)

	// --- Assemble line ---
	modelName := hookData.Model.DisplayName
	if modelName == "" {
		modelName = hookData.Model.ID
	}

	parts := []string{
		modelName,
		contextStr,
		"$" + fmt.Sprintf("%.2f", todayCost) + " today / " + sessionCostStr + " session / " + blockStr,
	}
	if burnRateStr != "" {
		parts = append(parts, burnRateStr)
	}

	var sb strings.Builder
	for i, p := range parts {
		if i > 0 {
			sb.WriteString(" | ")
		}
		sb.WriteString(p)
	}
	sb.WriteByte('\n')

	return sb.String()
}

func formatSessionCost(costSource string, ccCost *float64, ccusageCost float64) string {
	switch costSource {
	case "cc":
		if ccCost != nil {
			return fmt.Sprintf("$%.2f", *ccCost)
		}
		return "N/A"
	case "ccusage":
		return fmt.Sprintf("$%.2f", ccusageCost)
	case "both":
		ccStr := "N/A"
		if ccCost != nil {
			ccStr = fmt.Sprintf("$%.2f", *ccCost)
		}
		return fmt.Sprintf("(%s cc / $%.2f ccusage)", ccStr, ccusageCost)
	default: // auto
		if ccCost != nil {
			return fmt.Sprintf("$%.2f", *ccCost)
		}
		return fmt.Sprintf("$%.2f", ccusageCost)
	}
}

func formatRemaining(d time.Duration) string {
	if d <= 0 {
		return "0m left"
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh %dm left", h, m)
	}
	return fmt.Sprintf("%dm left", m)
}

func formatBurnRate(costPerHour, tokensPerMinute float64, mode string) string {
	base := fmt.Sprintf("$%.2f/hr", costPerHour)

	var indicator, color string
	switch {
	case tokensPerMinute < burnRateLow:
		indicator = "normal"
		color = "\033[32m"
	case tokensPerMinute < burnRateMedium:
		indicator = "moderate"
		color = "\033[33m"
	default:
		indicator = "high"
		color = "\033[31m"
	}

	coloredBase := color + base + "\033[0m"

	switch mode {
	case "text", "emoji", "emoji-text":
		return coloredBase + " (" + indicator + ")"
	default: // off
		return coloredBase
	}
}

const progressBarWidth = 12

func formatContextUsage(hookData statuslineHookData, lowThreshold, mediumThreshold int) string {
	inputTokens, contextLimit := resolveContextTokens(hookData)
	if contextLimit <= 0 {
		return "N/A"
	}

	pct := float64(inputTokens) / float64(contextLimit)
	percentage := int(pct * 100)

	var color string
	switch {
	case percentage < lowThreshold:
		color = "\033[32m"
	case percentage < mediumThreshold:
		color = "\033[33m"
	default:
		color = "\033[31m"
	}

	filled := min(int(pct*progressBarWidth), progressBarWidth)
	bar := strings.Repeat("█", filled) + strings.Repeat("░", progressBarWidth-filled)

	return fmt.Sprintf("%s[%s] %d%%\033[0m", color, bar, percentage)
}

// resolveContextTokens returns (inputTokens, contextWindowLimit).
// Prefers hook data; falls back to reading the transcript file.
func resolveContextTokens(hookData statuslineHookData) (int, int) {
	if hookData.ContextWindow != nil && hookData.ContextWindow.ContextWindow > 0 {
		return hookData.ContextWindow.InputTokens, hookData.ContextWindow.ContextWindow
	}

	// Fallback: read last usage entry from transcript JSONL
	if hookData.TranscriptPath == "" {
		return 0, 0
	}

	inputTokens := readContextFromTranscript(hookData.TranscriptPath)
	if inputTokens <= 0 {
		return 0, 0
	}

	return inputTokens, modelContextWindow(hookData.Model.ID)
}

// transcriptUsage is the minimal structure we need from a transcript JSONL line.
type transcriptUsage struct {
	Message *struct {
		Usage *struct {
			InputTokens              int `json:"input_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
}

const transcriptReadChunk = 8192 // 8 KB per chunk, grows if needed

// readContextFromTranscript seeks from the end of the transcript JSONL and returns
// the total input tokens (input + cache_creation + cache_read) of the last assistant turn.
// Reads backwards in chunks to avoid loading the entire file.
func readContextFromTranscript(path string) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()

	size, err := f.Seek(0, io.SeekEnd)
	if err != nil || size == 0 {
		return 0
	}

	buf := make([]byte, 0, transcriptReadChunk)
	offset := size

	for offset > 0 {
		chunkSize := min(int64(transcriptReadChunk), offset)
		offset -= chunkSize

		chunk := make([]byte, chunkSize)
		if _, err := f.ReadAt(chunk, offset); err != nil {
			break
		}
		// Prepend new chunk to buffer
		buf = append(chunk, buf...)

		// Scan lines from the end of current buffer
		if tokens := scanLastUsageLine(buf); tokens > 0 {
			return tokens
		}

		// If we've read the whole file and found nothing, stop
		if offset == 0 {
			break
		}
	}
	return 0
}

// scanLastUsageLine parses lines from the end of buf and returns tokens from the
// last line that contains valid usage data, or 0 if none found.
func scanLastUsageLine(buf []byte) int {
	// Find line boundaries from end
	end := len(buf)
	for end > 0 {
		start := end - 1
		// Skip trailing newline
		for start >= 0 && buf[start] == '\n' {
			start--
		}
		if start < 0 {
			break
		}
		// Find start of this line
		lineEnd := start + 1
		for start > 0 && buf[start-1] != '\n' {
			start--
		}
		line := buf[start:lineEnd]
		end = start

		if len(line) == 0 {
			continue
		}
		var entry transcriptUsage
		if err := json.Unmarshal(line, &entry); err != nil {
			continue
		}
		if entry.Message == nil || entry.Message.Usage == nil {
			continue
		}
		u := entry.Message.Usage
		total := u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
		if total > 0 {
			return total
		}
	}
	return 0
}

// modelContextWindow returns the context window size for known Claude models.
// All recent Claude models use 200k; kept as a hook for future model-specific limits.
func modelContextWindow(_ string) int {
	return 200000
}

// --- Cache helpers ---

func statuslineCachePath(sessionID string) string {
	dir := filepath.Join(os.TempDir(), "ccusage-statusline")
	return filepath.Join(dir, sessionID+".cache")
}

func readStatuslineCache(sessionID, transcriptPath string) string {
	path := statuslineCachePath(sessionID)
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}

	var cache statuslineCache
	if err := json.Unmarshal(data, &cache); err != nil {
		return ""
	}

	info, err := os.Stat(transcriptPath)
	if err != nil {
		return ""
	}

	if info.ModTime().UnixMilli() == cache.TranscriptMtime {
		return cache.Output
	}
	return ""
}

func writeStatuslineCache(sessionID, transcriptPath, output string) {
	dir := filepath.Join(os.TempDir(), "ccusage-statusline")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}

	var mtime int64
	if info, err := os.Stat(transcriptPath); err == nil {
		mtime = info.ModTime().UnixMilli()
	}

	cache := statuslineCache{
		TranscriptMtime: mtime,
		Output:          output,
	}
	data, err := json.Marshal(cache)
	if err != nil {
		return
	}
	_ = os.WriteFile(statuslineCachePath(sessionID), data, 0o644)
}
