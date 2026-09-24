// Package chat provides a line-oriented frontend to the harness coordinator.
package chat

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/unreallabsai/unreal-agent/cmd/internal/agentrunner"
	"github.com/unreallabsai/unreal-agent/harness/llm"
)

const help = `Enter a complete line to send a message (also while work is running).
/help          Show commands
/status        Show session, model activity, and operation IDs/states
/sessions      List IDs, first prompts/topics, empty sessions, and * current
/new           Stop current work and open a fresh unsaved chat
/resume [ID]   Choose a saved session, or stop/join and resume the exact ID
/cancel ID     Cancel only this operation
/stop          Stop all current work; keep history; no automatic retry
/exit, /quit   Stop work and exit (EOF also exits)
Terminal: arrows/Ctrl-B/F move; Home/End or Ctrl-A/E; Backspace/Delete;
Ctrl-U/K clear before/after cursor; Ctrl-W deletes a word; Up/Down or Ctrl-P/N
recall this run’s input history. Ctrl-D on an empty draft exits.
Ctrl-C stops work, not the chat, retaining your draft. Paste folds into a ▣
block (exact text, including newlines/tabs); Enter submits, never paste itself.
Multiple content lines are user text, even when starting with a slash.
Single-line /commands (typed or pasted, with optional surrounding blank lines)
run only after Enter; copied IDs work.
Arrows cross a block; Backspace/Delete removes it whole.
History retains the blocks. Limit: 1 MiB per message, 4096 editable cells;
exceeding either rejects the whole draft explicitly, never sends a prefix.
Async output preserves the draft/cursor.
Colors require a capable TTY; NO_COLOR disables color. Pipes remain plain.
Command and diagnostic log paths are shown at startup and /status.
Startup and /new stay UNSAVED until the first actual user message.
/resume opens a recent-first chooser: Up/Down move, Enter resumes, Esc cancels.
Opening/canceling does not stop work; Ctrl-C still stops work, EOF exits.
Without a capable terminal, /resume lists IDs with exact-ID instructions.
Startup never auto-resumes; -session ID is explicit. /resume never
substitutes another ID. Tools run with your local permissions.
`

const systemPrompt = `You are an interactive coding assistant working in the local workspace.
Discussion, questions, and requests to explain code are NOT permission to edit files or run mutating commands.
Explicit requests to implement or fix authorize relevant edits and tests. Clarify ambiguous requests.
Tools execute with the local process permissions; there is no isolated sandbox and no enforced read-only mode.
Never expose credentials or private reasoning. Do not read credential files to answer the user.
This is an ongoing chat: an ordinary reply ends only your response, not the application.
After a user stop, do not retry interrupted work unless the next user request authorizes it.
`

type config struct {
	workspace, directory, session, provider, model, effort, baseURL string
	attempts                                                        int
	prompt                                                          string
}

func parse(args []string, getenv func(string) string, out io.Writer) (config, error) {
	c := config{}
	env := func(name, fallback string) string {
		if v := strings.TrimSpace(getenv(name)); v != "" {
			return v
		}
		return fallback
	}
	f := flag.NewFlagSet("unreal_chat", flag.ContinueOnError)
	f.SetOutput(out)
	f.StringVar(&c.provider, "provider", env("UNREAL_HARNESS_LLM_PROVIDER", "openai-codex"), "provider: openai-codex, openai, ollama, openrouter, fireworks")
	f.StringVar(&c.model, "model", env("UNREAL_HARNESS_LLM_MODEL", "gpt-6-astra"), "model name")
	f.StringVar(&c.effort, "reasoning-effort", env("UNREAL_HARNESS_LLM_REASONING_EFFORT", "xhigh"), "reasoning effort: low, medium, high, xhigh, max")
	f.StringVar(&c.baseURL, "base-url", getenv("UNREAL_HARNESS_LLM_BASE_URL"), "provider base URL override")
	// URLs may include credentials. Keep their environment value out of -h.
	f.Lookup("base-url").DefValue = ""
	attempts := env("UNREAL_HARNESS_LLM_MAX_ATTEMPTS", "1")
	f.Func("max-attempts", "bounded provider request attempts (default 1)", func(v string) error { attempts = v; return nil })
	f.StringVar(&c.directory, "session-directory", "", "session directory (default <workspace>/.harness/sessions; relative to workspace)")
	f.StringVar(&c.session, "session", "", "resume an existing session ID")
	f.Usage = func() {
		fmt.Fprintln(out, "Usage: unreal_chat [flags] [workspace]")
		f.PrintDefaults()
		fmt.Fprint(out, "\n"+help)
	}
	if err := f.Parse(args); err != nil {
		return c, err
	}
	if f.NArg() > 1 {
		return c, errors.New("expected at most one workspace argument (flags must precede workspace)")
	}
	if !llm.ReasoningEffort(c.effort).Valid() {
		return c, errors.New("invalid -reasoning-effort; use low, medium, high, xhigh, or max")
	}
	if strings.TrimSpace(c.model) == "" {
		return c, errors.New("model must not be empty")
	}
	emptySession := false
	f.Visit(func(v *flag.Flag) {
		if v.Name == "session" && strings.TrimSpace(c.session) == "" {
			emptySession = true
		}
	})
	if emptySession {
		return c, errors.New("session ID must not be empty")
	}
	var err error
	c.attempts, err = strconv.Atoi(attempts)
	if err != nil || c.attempts < 1 {
		return c, errors.New("max-attempts must be a positive integer")
	}
	workspace := "."
	if f.NArg() == 1 {
		workspace = f.Arg(0)
	}
	c.workspace, err = filepath.Abs(workspace)
	if err != nil {
		return c, err
	}
	info, err := os.Stat(c.workspace)
	if err != nil {
		return c, fmt.Errorf("workspace: %w", err)
	}
	if !info.IsDir() {
		return c, errors.New("workspace is not a directory")
	}
	if c.directory == "" {
		c.directory = ".harness/sessions"
	}
	if !filepath.IsAbs(c.directory) {
		c.directory = filepath.Join(c.workspace, c.directory)
	}
	c.prompt = systemPrompt + "\nWorkspace: " + c.workspace
	agents, err := os.ReadFile(filepath.Join(c.workspace, "AGENTS.md"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return c, fmt.Errorf("read workspace AGENTS.md: %w", err)
	}
	if err == nil {
		c.prompt += "\n\nWorkspace AGENTS.md:\n" + string(agents)
	}
	return c, nil
}

func (c config) client(providers []agentrunner.Provider, getenv func(string) string) (agentrunner.Client, error) {
	for _, p := range providers {
		if p.Name != c.provider {
			continue
		}
		key := ""
		if p.APIKeyEnvironment != "" {
			key = getenv("UNREAL_HARNESS_LLM_API_KEY")
			if strings.TrimSpace(key) == "" {
				key = getenv(p.APIKeyEnvironment)
			}
			if strings.TrimSpace(key) == "" {
				return nil, fmt.Errorf("set %s or UNREAL_HARNESS_LLM_API_KEY", p.APIKeyEnvironment)
			}
		}
		base := c.baseURL
		if base == "" {
			base = p.BaseURL
		}
		if p.NewClient == nil {
			return nil, errors.New("provider has no client factory")
		}
		client, err := p.NewClient(key, base, c.attempts, getenv)
		// Provider errors may contain response bodies, URLs, or header values.
		if err != nil {
			return nil, fmt.Errorf("cannot initialize %s: check configuration and missing/expired credentials; renew credentials externally (see cmd/unreal_chat/README.md): %w", c.provider, err)
		}
		return client, nil
	}
	return nil, fmt.Errorf("unknown provider %q; use openai-codex, openai, ollama, openrouter, or fireworks", c.provider)
}
