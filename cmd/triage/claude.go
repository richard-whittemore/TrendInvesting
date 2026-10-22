package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/richard-whittemore/TrendInvesting/internal/procgroup"
)

// prompt is the fixed prompt of every session: built into the program, so
// nothing on the Mac mini can change what the session is told.
//
//go:embed prompt.md
var prompt string

// sessionTools are the only commands the session may run, by name, each a
// fixed command line: the tests, vet, gofmt's listing and git's read-only
// commands (the log to its last 100 commits), with no flag of the
// session's choosing, so no -exec, -toolexec, -vettool or -o runs or leaves
// another program. The session's wrapper (runTool) only names one to the
// job, which runs it (serveTools): the Go ones as the gate user, so no test
// the session wrote runs as the user holding the Claude token
// (docs/manual/alerts-and-dashboard.md, "What the session and the gate can
// and cannot do").
var sessionTools = map[string][]string{
	"go-test":    {"go", "test", "./..."},
	"go-vet":     {"go", "vet", "./..."},
	"gofmt":      {"gofmt", "-l", "."},
	"git-diff":   {"git", "-c", "core.fsmonitor=false", "--no-pager", "diff"},
	"git-status": {"git", "-c", "core.fsmonitor=false", "--no-pager", "status"},
	"git-log":    {"git", "-c", "core.fsmonitor=false", "--no-pager", "log", "-n", "100"},
	"git-show":   {"git", "-c", "core.fsmonitor=false", "--no-pager", "show"},
}

// toolCommand is the job's subcommand that runs one of sessionTools.
const toolCommand = "tool"

// sessionToolNames is sessionTools' names, in order.
func sessionToolNames() []string {
	names := make([]string, 0, len(sessionTools))
	for n := range sessionTools {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// allowedTools are the only tools the session may use without asking, and
// in a headless session nothing can be asked (--permission-mode dontAsk,
// --permission-prompts none): reading the worktree, changing files in it
// (file tools are confined to the working folder by --restricted), and the
// exact invocations of the job's wrapper, tool, one per sessionTools name.
// The sandbox (sessionSettings) is the boundary; the list keeps the session
// to what it needs.
func allowedTools(tool string) []string {
	out := []string{"Read", "Grep", "Glob", "Edit", "Write"}
	for _, n := range sessionToolNames() {
		out = append(out, "Bash("+tool+" "+toolCommand+" "+n+")")
	}
	return out
}

// disallowedTools are denied outright, whatever any other rule allows: the
// web, every way to change git's history or reach GitHub, the server or
// another host, and every MCP server's tools.
var disallowedTools = []string{
	"WebFetch", "WebSearch",
	"Bash(git push *)", "Bash(git commit *)", "Bash(git reset *)", "Bash(git checkout *)", "Bash(git remote *)", "Bash(git config *)",
	"Bash(gh *)", "Bash(ssh *)", "Bash(scp *)", "Bash(sftp *)", "Bash(rsync *)", "Bash(curl *)", "Bash(wget *)", "Bash(nc *)",
	"Bash(tailscale *)", "Bash(security *)", "Bash(sudo *)", "mcp__*",
}

// analysisSchema is the answer's shape, checked by Claude Code itself
// (--json-schema) and by the job (analysis.Kind).
const analysisSchema = `{"type":"object","additionalProperties":false,` +
	`"required":["kind","what_happened","rule","owner_action","fix_title","fix_summary"],` +
	`"properties":{"kind":{"type":"string","enum":["code-defect","data-problem","owner-action"]},` +
	`"what_happened":{"type":"string"},"rule":{"type":"string"},"owner_action":{"type":"string"},` +
	`"fix_title":{"type":"string"},"fix_summary":{"type":"string"}}}`

// maxAnswer bounds what the session's output is read to.
const maxAnswer = 1 << 20

// errInterrupted is a session the job's own stop ended (a SIGTERM, or
// launchd's bootout): not a failed analysis, so the issue is not spent on it.
var errInterrupted = errors.New("the job was stopped during the session")

// untrustedBegin and untrustedEnd mark where the alert's and the journal's
// text begin and end in the prompt: everything between them is data.
const (
	untrustedBegin = "<<<TRIAGE-DATA-BEGIN>>>"
	untrustedEnd   = "<<<TRIAGE-DATA-END>>>"
)

// sessionSettings is the session's own settings (--settings): Claude Code's
// sandbox around every command the session runs, required
// (failIfUnavailable), with no command let out of it, no network but the
// Unix sockets in sockets (the job's, which its wrapper asks), reads under
// /Users denied but for allowRead (the worktrees and the job's program),
// the token folders (denyRead) denied again, writes allowed only in the
// worktree and allowWrite, and the Claude token unset for every command
// (sandbox.credentials); and file reads kept to the working folder.
func sessionSettings(denyRead, allowRead, allowWrite, sockets []string) string {
	type network struct {
		AllowedDomains   []string `json:"allowedDomains"`
		AllowUnixSockets []string `json:"allowUnixSockets"`
	}
	type filesystem struct {
		DenyRead   []string `json:"denyRead"`
		AllowRead  []string `json:"allowRead"`
		AllowWrite []string `json:"allowWrite"`
	}
	type envVar struct {
		Name string `json:"name"`
		Mode string `json:"mode"`
	}
	type credentials struct {
		EnvVars []envVar `json:"envVars"`
	}
	type sandbox struct {
		Enabled                  bool        `json:"enabled"`
		FailIfUnavailable        bool        `json:"failIfUnavailable"`
		AllowUnsandboxedCommands bool        `json:"allowUnsandboxedCommands"`
		AutoAllowBashIfSandboxed bool        `json:"autoAllowBashIfSandboxed"`
		Network                  network     `json:"network"`
		Filesystem               filesystem  `json:"filesystem"`
		Credentials              credentials `json:"credentials"`
	}
	type permissions struct {
		BlockReads bool `json:"blockReadsOutsideWorkingDirectories"`
	}
	raw, _ := json.Marshal(struct {
		Sandbox     sandbox     `json:"sandbox"`
		Permissions permissions `json:"permissions"`
	}{
		Sandbox: sandbox{Enabled: true, FailIfUnavailable: true, Network: network{AllowedDomains: []string{}, AllowUnixSockets: append([]string{}, sockets...)},
			Filesystem: filesystem{DenyRead: append([]string{"/Users"}, denyRead...), AllowRead: append([]string{}, allowRead...),
				AllowWrite: append([]string{}, allowWrite...)},
			Credentials: credentials{EnvVars: []envVar{{Name: "CLAUDE_CODE_OAUTH_TOKEN", Mode: "deny"}}}},
		Permissions: permissions{BlockReads: true},
	}) // a struct of strings and bools always encodes
	return string(raw)
}

// data is text from the alert or the journal, between the marks, with any
// mark inside it broken, so it cannot close its own.
func data(s string) string {
	s = strings.ReplaceAll(s, untrustedBegin, "<<< TRIAGE-DATA-BEGIN >>>")
	s = strings.ReplaceAll(s, untrustedEnd, "<<< TRIAGE-DATA-END >>>")
	return untrustedBegin + "\n" + s + "\n" + untrustedEnd
}

// claudeCLI is one headless Claude Code session (`claude -p`) per analysis.
type claudeCLI struct {
	path     string
	model    string
	timeout  time.Duration
	maxTurns int
	// env is the session's whole environment (sessionEnv); settings its
	// settings (sessionSettings).
	env      []string
	settings string
	// tool is the job's own program, the wrapper every command of the
	// session runs through (runTool); socket where the wrapper reaches the
	// job, and tools the job's side of the commands (serveTools).
	tool   string
	socket string
	tools  toolHost
}

// claudeArgs is the session's fixed command line.
func (c claudeCLI) claudeArgs() []string {
	args := []string{
		"-p", "--restricted", "--tools", "Read,Grep,Glob,Edit,Write,Bash",
		"--permission-mode", "dontAsk", "--permission-prompts", "none",
		"--strict-mcp-config", "--disable-slash-commands", "--no-session-persistence",
		"--output-format", "json", "--json-schema", analysisSchema,
		"--max-turns", strconv.Itoa(c.maxTurns),
	}
	if c.model != "" {
		args = append(args, "--model", c.model)
	}
	if c.settings != "" {
		args = append(args, "--settings", c.settings)
	}
	args = append(append(args, "--allowedTools"), allowedTools(c.tool)...)
	return append(append(args, "--disallowedTools"), disallowedTools...)
}

// Analyse runs the session in the request's worktree, handing it the prompt,
// the issue and the slice on its input, and reads its structured answer.
func (c claudeCLI) Analyse(parent context.Context, req analysisRequest) (got analysis, err error) {
	ctx, cancel := context.WithTimeout(parent, c.timeout)
	defer cancel()
	env := c.env
	if c.tools != nil {
		run, done, openErr := c.tools.OpenTools(ctx, req.Dir)
		if openErr != nil {
			return analysis{}, openErr
		}
		// A gate failure one of the session's commands met is the gate's,
		// whatever the session answered: the issue is tried again later. It
		// runs after the commands stop being served (deferred below).
		defer func() {
			if gateErr := done(); gateErr != nil && !errors.Is(err, errInterrupted) {
				got, err = analysis{}, gateErr
			}
		}()
		stop, serveErr := serveTools(ctx, c.socket, run)
		if serveErr != nil {
			return analysis{}, fmt.Errorf("%w: the session's commands cannot be served: %w", errGateUnavailable, serveErr)
		}
		defer stop()
		env = withEnv(env, toolsSocketEnv, c.socket)
	}
	//nolint:gosec // G204: the program is the operator's -claude flag and its arguments are the fixed list claudeArgs builds.
	cmd := exec.CommandContext(ctx, c.path, c.claudeArgs()...)
	cmd.Dir = req.Dir
	cmd.Env = env
	// Its own process group, killed whole at its time or the job's stop, so
	// no command it started outlives it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return procgroup.Kill(cmd.Process.Pid) }
	cmd.WaitDelay = 5 * time.Second
	cmd.Stdin = strings.NewReader(prompt + c.commands() +
		"\n\n## The alert issue\n\n" + data("Title: "+req.Issue.Title+"\n\n"+req.Issue.Body) +
		"\n\n## The journal slice\n\n" + data(req.Slice) + "\n")
	var out bytes.Buffer
	cmd.Stdout = &limitedBuffer{buf: &out, left: maxAnswer}
	if err := cmd.Run(); err != nil {
		if parent.Err() != nil {
			return analysis{}, errInterrupted
		}
		if ctx.Err() != nil {
			return analysis{}, fmt.Errorf("the session ran past its %s", c.timeout)
		}
		return analysis{}, errors.New("the session exited with an error")
	}
	var result struct {
		Subtype          string          `json:"subtype"`
		IsError          bool            `json:"is_error"`
		StructuredOutput json.RawMessage `json:"structured_output"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		return analysis{}, errors.New("the session's answer was not understood")
	}
	if result.IsError {
		return analysis{}, fmt.Errorf("the session ended with %s", result.Subtype)
	}
	if len(result.StructuredOutput) == 0 {
		return analysis{}, errors.New("the session gave no structured answer")
	}
	var a analysis
	if err := json.Unmarshal(result.StructuredOutput, &a); err != nil {
		return analysis{}, errors.New("the session's answer was not the shape asked for")
	}
	return a, nil
}

// commands is the prompt's list of the session's exact command lines.
func (c claudeCLI) commands() string {
	var b strings.Builder
	b.WriteString("\n\n## Your commands\n\nRun each exactly as written, nothing else:\n\n")
	for _, n := range sessionToolNames() {
		fmt.Fprintf(&b, "- `%s %s %s`: `%s`\n", c.tool, toolCommand, n, strings.Join(sessionTools[n], " "))
	}
	return b.String()
}

// limitedBuffer keeps at most left bytes and drops the rest, so a runaway
// session cannot fill the job's memory; an answer cut short fails to parse.
type limitedBuffer struct {
	buf  *bytes.Buffer
	left int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	n := min(len(p), l.left)
	l.buf.Write(p[:n])
	l.left -= n
	return len(p), nil
}

// keptNames are the only variables of the job's environment a session or
// the job's own git and go commands are handed: what they need to run. No
// GitHub token, Anthropic API key or helper, SSH agent or cloud credential
// is among them, nor any Go setting that could run another program
// (GOFLAGS) or reach the network (GOPROXY): the job sets those itself.
var keptNames = []string{
	"PATH", "HOME", "USER", "LOGNAME", "SHELL", "LANG", "LC_ALL", "LC_CTYPE", "TMPDIR",
	"GOPATH", "GOCACHE", "GOMODCACHE",
}

// kept is environ's variables named in names.
func kept(environ, names []string) []string {
	var out []string
	for _, e := range environ {
		name, _, ok := strings.Cut(e, "=")
		if ok && slices.Contains(names, name) {
			out = append(out, e)
		}
	}
	return out
}

// sessionEnv is a session's whole environment: the kept variables, git cut
// off from every credential helper (gitEnv), gh pointed at a configuration
// that does not exist, so it is not logged in, Go offline and read-only
// (the job filled the module cache), Claude Code's scrub of its commands'
// environment, and Claude Code's own OAuth token when one is given, so the
// session bills the subscription and never an API key.
func sessionEnv(environ []string, oauthToken string) []string {
	env := append(gitEnv(kept(environ, keptNames)), "GH_CONFIG_DIR=/var/empty/ti-triage-no-gh",
		"GOFLAGS=-mod=readonly", "GOPROXY=off", "GOTOOLCHAIN=local", "GOWORK=off", "CLAUDE_CODE_SUBPROCESS_ENV_SCRUB=1")
	if oauthToken != "" {
		env = append(env, "CLAUDE_CODE_OAUTH_TOKEN="+oauthToken)
	}
	return env
}

// withEnv is env with name set to value, replacing any value it had.
func withEnv(env []string, name, value string) []string {
	out := slices.DeleteFunc(slices.Clone(env), func(e string) bool { return strings.HasPrefix(e, name+"=") })
	return append(out, name+"="+value)
}
