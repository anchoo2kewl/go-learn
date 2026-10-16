// Package agent runs one text-generation turn through a logged-in CLI (claude, codex or gemini), the
// way the llm-brain hub runs its agent turns: the prompt goes on stdin, the CLI streams JSON events,
// and the final answer and session id are read from them. The owner's subscriptions pay for it; no
// API keys are involved.
//
// Turns are pure text. Every runner switches tools off and loads no MCP servers, so text that came
// from a learner can never act on the machine.
package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Providers the hub offers, in its order.
var Providers = []string{"claude", "codex", "gemini"}

// Turn is one request.
type Turn struct {
	Provider string // claude | codex | gemini
	Model    string // optional; the CLI's default when empty
	Dir      string // working directory: an empty per-course scratch folder
	Prompt   string
	Resume   string // provider session to continue, if any
	Name     string // claude --name on a first turn
}

// Result is one finished turn.
type Result struct {
	Text      string
	SessionID string
	Provider  string
	Model     string
	Duration  time.Duration
}

// Runner runs turns. The hub can supply its own (its agent queue); CLI is the built-in one.
type Runner interface {
	Run(ctx context.Context, t Turn) (Result, error)
}

// ErrLimited marks a failure that looks like a subscription limit, so callers can fail over.
var ErrLimited = errors.New("agent: provider limit reached")

var (
	modelRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/\[\]-]{0,99}$`)
	sessionRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	nameRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,79}$`)
	limitRe   = regexp.MustCompile(`(?i)(rate.?limit|usage limit|quota|429|too many requests|limit reached|credit balance)`)
)

// CLI runs the real command-line tools found on PATH.
type CLI struct {
	Timeout time.Duration // per turn; default 6 minutes
}

func (c CLI) Run(ctx context.Context, t Turn) (Result, error) {
	if t.Model != "" && !modelRe.MatchString(t.Model) {
		return Result{}, fmt.Errorf("agent: bad model name %q", t.Model)
	}
	if t.Resume != "" && !sessionRe.MatchString(t.Resume) {
		return Result{}, fmt.Errorf("agent: bad session id %q", t.Resume)
	}
	if t.Name != "" && !nameRe.MatchString(t.Name) {
		return Result{}, fmt.Errorf("agent: bad session name %q", t.Name)
	}
	if err := os.MkdirAll(t.Dir, 0o755); err != nil {
		return Result{}, err
	}
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 6 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	start := time.Now()
	var (
		res Result
		err error
	)
	switch t.Provider {
	case "claude", "":
		res, err = runClaude(ctx, t)
	case "codex":
		res, err = runCodex(ctx, t)
	case "gemini":
		res, err = runGemini(ctx, t)
	default:
		return Result{}, fmt.Errorf("agent: unknown provider %q", t.Provider)
	}
	res.Duration = time.Since(start)
	if res.Provider == "" {
		res.Provider = t.Provider
	}
	if err != nil && limitRe.MatchString(err.Error()) {
		err = fmt.Errorf("%w: %v", ErrLimited, err)
	}
	return res, err
}

// claude -p --output-format stream-json --verbose, no tools and no MCP servers; the user's own
// settings (global CLAUDE.md) still load, so the teacher knows the owner.
func runClaude(ctx context.Context, t Turn) (Result, error) {
	args := []string{"-p", "--output-format", "stream-json", "--verbose", "--tools", "", "--strict-mcp-config"}
	if t.Resume != "" {
		args = append(args, "--resume", t.Resume)
	} else if t.Name != "" {
		args = append(args, "--name", t.Name)
	}
	if t.Model != "" {
		args = append(args, "--model", t.Model)
	}
	out, stderr, err := run(ctx, "claude", args, t.Dir, t.Prompt)
	res := Result{Provider: "claude", Model: t.Model}
	var final *struct {
		Subtype   string `json:"subtype"`
		IsError   bool   `json:"is_error"`
		Result    string `json:"result"`
		SessionID string `json:"session_id"`
	}
	eachEvent(out, func(ev map[string]json.RawMessage) {
		var typ string
		_ = json.Unmarshal(ev["type"], &typ)
		switch typ {
		case "system":
			var m string
			if json.Unmarshal(ev["model"], &m) == nil && m != "" {
				res.Model = m
			}
		case "result":
			raw, _ := json.Marshal(ev)
			final = new(struct {
				Subtype   string `json:"subtype"`
				IsError   bool   `json:"is_error"`
				Result    string `json:"result"`
				SessionID string `json:"session_id"`
			})
			_ = json.Unmarshal(raw, final)
		}
	})
	if final == nil {
		return res, fmt.Errorf("claude: no result event (%v): %s", err, tail(stderr))
	}
	res.SessionID = final.SessionID
	res.Text = final.Result
	if final.IsError || final.Subtype != "success" {
		return res, fmt.Errorf("claude: %s: %s", final.Subtype, tail(final.Result+" "+stderr))
	}
	return res, nil
}

// codex exec --json in a read-only sandbox with MCP servers cleared; the last message is written to a file.
func runCodex(ctx context.Context, t Turn) (Result, error) {
	last := filepath.Join(t.Dir, fmt.Sprintf(".codex-last-%d.txt", time.Now().UnixNano()))
	defer os.Remove(last)
	args := []string{"exec"}
	if t.Resume != "" {
		args = append(args, "resume", t.Resume, "--json", "--skip-git-repo-check", "-c", `sandbox_mode="read-only"`)
	} else {
		args = append(args, "--json", "--skip-git-repo-check", "-C", t.Dir, "--sandbox", "read-only")
	}
	args = append(args, "-c", "mcp_servers={}", "-o", last)
	if t.Model != "" {
		args = append(args, "-m", t.Model)
	}
	args = append(args, "-")
	out, stderr, err := run(ctx, "codex", args, t.Dir, t.Prompt)
	res := Result{Provider: "codex", Model: t.Model}
	var failure string
	eachEvent(out, func(ev map[string]json.RawMessage) {
		var typ string
		_ = json.Unmarshal(ev["type"], &typ)
		switch typ {
		case "thread.started":
			_ = json.Unmarshal(ev["thread_id"], &res.SessionID)
		case "turn.failed", "error":
			failure = string(ev["error"]) + string(ev["message"])
		}
	})
	if res.SessionID == "" {
		res.SessionID = t.Resume
	}
	b, rerr := os.ReadFile(last)
	res.Text = strings.TrimSpace(string(b))
	if failure != "" || (res.Text == "" && (err != nil || rerr != nil)) {
		return res, fmt.Errorf("codex: %s %v: %s", failure, err, tail(stderr))
	}
	return res, nil
}

// gemini -p -o stream-json; offered because the hub offers it.
func runGemini(ctx context.Context, t Turn) (Result, error) {
	args := []string{"-o", "stream-json", "--allowed-mcp-server-names", "none"}
	if t.Model != "" {
		args = append(args, "-m", t.Model)
	}
	if t.Resume != "" {
		args = append(args, "--resume", t.Resume)
	}
	args = append(args, "-p", "")
	out, stderr, err := run(ctx, "gemini", args, t.Dir, t.Prompt)
	res := Result{Provider: "gemini", Model: t.Model}
	var sb strings.Builder
	eachEvent(out, func(ev map[string]json.RawMessage) {
		var typ, role, content string
		_ = json.Unmarshal(ev["type"], &typ)
		switch typ {
		case "init":
			_ = json.Unmarshal(ev["session_id"], &res.SessionID)
		case "message":
			_ = json.Unmarshal(ev["role"], &role)
			_ = json.Unmarshal(ev["content"], &content)
			if role == "assistant" {
				sb.WriteString(content)
			}
		}
	})
	res.Text = strings.TrimSpace(sb.String())
	if res.Text == "" {
		return res, fmt.Errorf("gemini: no answer (%v): %s", err, tail(stderr))
	}
	return res, nil
}

func run(ctx context.Context, bin string, args []string, dir, stdin string) ([]byte, string, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	if ctx.Err() != nil {
		err = fmt.Errorf("%s timed out: %w", bin, ctx.Err())
	}
	return out.Bytes(), errb.String(), err
}

func eachEvent(out []byte, fn func(map[string]json.RawMessage)) {
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var ev map[string]json.RawMessage
		if json.Unmarshal(line, &ev) == nil {
			fn(ev)
		}
	}
}

func tail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 600 {
		s = "…" + s[len(s)-600:]
	}
	return s
}

// ExtractJSON pulls the first JSON object out of a reply that may carry prose or code fences.
func ExtractJSON(s string, v any) error {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "```"); i >= 0 {
		rest := s[i+3:]
		rest = strings.TrimPrefix(rest, "json")
		if j := strings.Index(rest, "```"); j >= 0 {
			s = rest[:j]
		}
	}
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end <= start {
		return errors.New("agent: no JSON object in reply")
	}
	return json.Unmarshal([]byte(s[start:end+1]), v)
}
