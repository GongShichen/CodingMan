package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/GongShichen/CodingMan/agent"
	"golang.org/x/term"
)

const (
	colorReset = "\033[0m"
	colorDim   = "\033[2m"
	colorBold  = "\033[1m"
	colorCyan  = "\033[36m"
	colorGreen = "\033[32m"
	colorRed   = "\033[31m"
	colorGray  = "\033[90m"

	defaultDebianSandboxImageURL = "https://cloud.debian.org/images/cloud/bookworm/latest/debian-12-genericcloud-arm64.raw"
	debianSandboxImageName       = "debian-12-genericcloud-arm64.raw"
)

type RuntimeConfig struct {
	Provider  string
	ModelName string
	APIKey    string
	BaseURL   string

	Context                 agent.ContextConfig
	MaxLLMTurns             int
	MaxToolCalls            int
	MaxParallelTools        int
	MaxToolErrors           int
	MaxAPIErrors            int
	MaxSubAgentDepth        int
	MaxSubAgents            int
	SessionMemoryThreshold  int
	SkillEvolutionThreshold int
	SkillEviction           agent.SkillEvictionConfig
	MaxSessionMemoryEntries int
	MaxSessionMemoryChars   int
	MaxCrossMemoryChars     int
	EnableToolBudget        bool
	ToolBudget              agent.ToolBudget
	Retry                   agent.RetryConfig
	PromptCache             agent.PromptCacheConfig
	Coordination            agent.CoordinationConfig
	Permission              agent.PermissionConfig
	Sandbox                 agent.SandboxConfig
	SandboxCheck            SandboxEnvironmentCheck
	Hooks                   *agent.HookManager
	MCP                     agent.MCPConfig
	LogPath                 string
}

type CLIOptions struct {
	NonInteractive bool
	Prompt         string
	PromptFile     string
	Cwd            string
	Permission     string
	MaxLLMTurns    int
	MaxToolCalls   int
}

func main() {
	options, err := parseCLIOptions(os.Args[1:])
	if err != nil {
		fatal("parse args", err)
	}
	launchDir, err := os.Getwd()
	if err != nil {
		fatal("get working directory", err)
	}
	projectRoot, err := findProjectRoot(".")
	if err != nil {
		fatal("find project root", err)
	}
	ensureVFKitDefaultPath()

	cfg, source, err := loadRuntimeConfig(projectRoot, launchDir)
	if err != nil {
		fatal("load config", err)
	}
	if err := applyCLIOptions(&cfg, options); err != nil {
		fatal("apply args", err)
	}
	if options.NonInteractive && cfg.SandboxCheck.Needed {
		fatal("sandbox environment check", fmt.Errorf("missing sandbox dependencies:\n%s\nrun CodingMan interactively to approve installation, or install them manually", cfg.SandboxCheck.Summary()))
	}
	var tui *tuiController
	if !options.NonInteractive {
		scanner := bufio.NewScanner(os.Stdin)
		scanner.Buffer(make([]byte, 1024), 1024*1024)
		tui = newTUIController(scanner)
		if err := tui.confirmAndInstallSandboxEnvironment(cfg.SandboxCheck); err != nil {
			fmt.Fprintf(os.Stderr, "%ssandbox environment: %v%s\n", colorRed, err, colorReset)
			if tui.confirmFullAutoAfterSandboxFailure(err) {
				enableFullAutoUnsandboxed(&cfg)
			}
		}
	}

	client, err := agent.CreateLLM(agent.LLMConfig{
		Provider: cfg.Provider,
		BaseURL:  cfg.BaseURL,
		APIKey:   cfg.APIKey,
	})
	if err != nil {
		fatal("create llm", err)
	}
	logger, err := agent.NewFileLogger(cfg.LogPath)
	if err != nil {
		fatal("create logger", err)
	}
	defer logger.Close()

	a := agent.NewAgent(agent.AgentConfig{
		LLM:                      client,
		Context:                  cfg.Context,
		Model:                    cfg.ModelName,
		MaxLLMTurns:              cfg.MaxLLMTurns,
		MaxToolCalls:             cfg.MaxToolCalls,
		MaxParallelToolCalls:     cfg.MaxParallelTools,
		MaxConsecutiveToolErrors: cfg.MaxToolErrors,
		MaxConsecutiveAPIErrors:  cfg.MaxAPIErrors,
		MaxConcurrentSubAgents:   cfg.MaxSubAgents,
		MaxSubAgentDepth:         cfg.MaxSubAgentDepth,
		SessionMemoryThreshold:   cfg.SessionMemoryThreshold,
		SkillEvolutionThreshold:  cfg.SkillEvolutionThreshold,
		SkillEviction:            cfg.SkillEviction,
		MaxSessionMemoryEntries:  cfg.MaxSessionMemoryEntries,
		MaxSessionMemoryChars:    cfg.MaxSessionMemoryChars,
		MaxCrossMemoryChars:      cfg.MaxCrossMemoryChars,
		EnableToolBudget:         cfg.EnableToolBudget,
		ToolBudget:               cfg.ToolBudget,
		RetryConfig:              cfg.Retry,
		Permission:               cfg.Permission,
		Sandbox:                  cfg.Sandbox,
		PromptCache:              cfg.PromptCache,
		Coordination:             cfg.Coordination,
		Hooks:                    cfg.Hooks,
		MCP:                      cfg.MCP,
		Logger:                   logger,
	})
	defer a.Close()

	if options.NonInteractive {
		if err := RunHeadless(a, options); err != nil {
			fatal("run headless", err)
		}
		return
	}
	RunTUIWithController(a, cfg, source, tui)
}

func parseCLIOptions(args []string) (CLIOptions, error) {
	var options CLIOptions
	flags := flag.NewFlagSet("codingman", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.BoolVar(&options.NonInteractive, "non-interactive", false, "run one prompt and exit")
	flags.StringVar(&options.Prompt, "prompt", "", "prompt text for non-interactive mode")
	flags.StringVar(&options.PromptFile, "prompt-file", "", "path to a prompt file for non-interactive mode")
	flags.StringVar(&options.Cwd, "cwd", "", "working directory for agent tools")
	flags.StringVar(&options.Permission, "permission", "", "permission mode: ask, allow-deny, full-auto")
	flags.IntVar(&options.MaxLLMTurns, "max-turns", 0, "maximum LLM turns")
	flags.IntVar(&options.MaxToolCalls, "max-tool-calls", 0, "maximum tool calls")
	if err := flags.Parse(args); err != nil {
		return CLIOptions{}, err
	}
	if flags.NArg() > 0 {
		return CLIOptions{}, fmt.Errorf("unexpected positional arguments: %s", strings.Join(flags.Args(), " "))
	}
	if !options.NonInteractive {
		if options.Prompt != "" || options.PromptFile != "" {
			options.NonInteractive = true
		}
		return options, nil
	}
	if strings.TrimSpace(options.Prompt) == "" && strings.TrimSpace(options.PromptFile) == "" {
		return CLIOptions{}, errors.New("--non-interactive requires --prompt or --prompt-file")
	}
	return options, nil
}

func applyCLIOptions(cfg *RuntimeConfig, options CLIOptions) error {
	if cfg == nil {
		return errors.New("runtime config is nil")
	}
	if strings.TrimSpace(options.Cwd) != "" {
		absCwd, err := filepath.Abs(expandHome(options.Cwd))
		if err != nil {
			return err
		}
		cfg.Context.Cwd = absCwd
		cfg.Context.ProjectRoot = findMemoryProjectRoot(absCwd)
	}
	if strings.TrimSpace(options.Permission) != "" {
		mode, err := agent.ParsePermissionMode(options.Permission)
		if err != nil {
			return err
		}
		cfg.Permission.Mode = mode
		if mode == agent.PermissionModeFullAuto {
			if options.NonInteractive && !boolValue(readProcessEnv(), "CONFIRM_FULL_AUTO_UNSANDBOXED", false) {
				return errors.New("full-auto disables the sandbox; set CONFIRM_FULL_AUTO_UNSANDBOXED=true to confirm this risk in non-interactive mode")
			}
			fmt.Fprintln(os.Stderr, colorRed+"warning:"+colorReset+" full-auto disables the sandbox; dangerous operations may run on the local host")
			cfg.Permission.AllowedTools = []string{"*"}
		}
	}
	if options.MaxLLMTurns > 0 {
		cfg.MaxLLMTurns = options.MaxLLMTurns
	}
	if options.MaxToolCalls > 0 {
		cfg.MaxToolCalls = options.MaxToolCalls
	}
	return nil
}

func RunHeadless(a *agent.Agent, options CLIOptions) error {
	a.SetEventSink(func(event agent.AgentEvent) {
		printAgentEvent(os.Stderr, event)
	})
	prompt := strings.TrimSpace(options.Prompt)
	if strings.TrimSpace(options.PromptFile) != "" {
		data, err := os.ReadFile(expandHome(options.PromptFile))
		if err != nil {
			return err
		}
		if prompt != "" {
			prompt += "\n\n"
		}
		prompt += string(data)
	}
	if strings.TrimSpace(prompt) == "" {
		return errors.New("prompt is empty")
	}
	promptText, blocks, err := buildPromptContent(prompt)
	if err != nil {
		return err
	}
	resp, err := a.RunToolLoop(context.Background(), promptText, blocks...)
	if resp.StopReason != "" {
		fmt.Fprintf(os.Stderr, "stop_reason=%s\n", resp.StopReason)
	}
	if strings.TrimSpace(resp.Content) != "" {
		fmt.Println(strings.TrimSpace(resp.Content))
	}
	return err
}

func RunTUI(a *agent.Agent, cfg RuntimeConfig, source string) {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1024), 1024*1024)
	tui := newTUIController(scanner)
	RunTUIWithController(a, cfg, source, tui)
}

func RunTUIWithController(a *agent.Agent, cfg RuntimeConfig, source string, tui *tuiController) {
	if tui == nil {
		scanner := bufio.NewScanner(os.Stdin)
		scanner.Buffer(make([]byte, 1024), 1024*1024)
		tui = newTUIController(scanner)
	}
	session := newSessionController(a, cfg.Context.Cwd)
	if err := session.save(); err != nil {
		fmt.Fprintf(os.Stderr, "%ssave session: %v%s\n", colorRed, err, colorReset)
	}
	if permissions := a.Permission(); permissions != nil {
		permissions.SetAskFunc(tui.permissionPrompt)
	}
	a.SetEventSink(func(event agent.AgentEvent) {
		printAgentEvent(os.Stdout, event)
	})

	printHeader(cfg, source)
	printSessionHeader(session)

	for {
		fmt.Printf("%s>%s ", colorCyan, colorReset)
		if !tui.scanner.Scan() {
			if err := tui.scanner.Err(); err != nil {
				fmt.Fprintf(os.Stderr, "%sread input: %v%s\n", colorRed, err, colorReset)
			}
			return
		}

		prompt := strings.TrimSpace(tui.scanner.Text())
		if prompt == "" {
			continue
		}
		if prompt == "/exit" || prompt == "/quit" {
			fmt.Println(colorDim + "session ended" + colorReset)
			return
		}
		if prompt == "/clear" {
			a.Clear()
			if err := session.save(); err != nil {
				fmt.Fprintf(os.Stderr, "%ssave session: %v%s\n", colorRed, err, colorReset)
			}
			fmt.Println(colorDim + "conversation cleared" + colorReset)
			continue
		}
		if prompt == "/help" {
			printHelp()
			continue
		}
		if strings.HasPrefix(prompt, "/") {
			if handled := handleSlashCommand(a, tui, session, &cfg, prompt); handled {
				continue
			}
			fmt.Printf("%sunknown command:%s %s\n", colorRed, colorReset, prompt)
			fmt.Println(colorDim + "Type /help to list slash commands." + colorReset)
			continue
		}

		start := time.Now()
		if tui.planMode {
			resp, execute, err := tui.runPlan(a, prompt)
			if err != nil {
				fmt.Printf("%serror:%s %v\n", colorRed, colorReset, err)
				continue
			}
			if resp.Content != "" {
				fmt.Printf("\n%s%s%s\n", colorBold, strings.TrimSpace(resp.Content), colorReset)
			}
			if !execute {
				fmt.Printf("%splan skipped elapsed=%s%s\n\n", colorGray, time.Since(start).Round(time.Millisecond), colorReset)
				continue
			}
			fmt.Println(colorDim + "executing approved plan" + colorReset)
		}
		fmt.Printf("%srunning agent loop... press Esc to interrupt%s\n", colorGray, colorReset)
		resp, interrupted, err := tui.runAgent(a, prompt)
		if err != nil {
			fmt.Printf("%serror:%s %v\n", colorRed, colorReset, err)
			if !interrupted {
				if saveErr := session.save(); saveErr != nil {
					fmt.Fprintf(os.Stderr, "%ssave session: %v%s\n", colorRed, saveErr, colorReset)
				}
				continue
			}
		}

		if resp.Content != "" {
			fmt.Printf("\n%s%s%s\n", colorBold, strings.TrimSpace(resp.Content), colorReset)
		}
		if interrupted {
			fmt.Println(colorDim + "interrupted. Add more context, or leave empty to skip." + colorReset)
			fmt.Printf("%s+%s ", colorCyan, colorReset)
			if !tui.scanner.Scan() {
				if err := tui.scanner.Err(); err != nil {
					fmt.Fprintf(os.Stderr, "%sread input: %v%s\n", colorRed, err, colorReset)
				}
				return
			}
			followUp := strings.TrimSpace(tui.scanner.Text())
			if followUp != "" {
				resp, interrupted, err = tui.runAgent(a, followUp)
				if err != nil {
					fmt.Printf("%serror:%s %v\n", colorRed, colorReset, err)
					if saveErr := session.save(); saveErr != nil {
						fmt.Fprintf(os.Stderr, "%ssave session: %v%s\n", colorRed, saveErr, colorReset)
					}
					continue
				}
				if resp.Content != "" {
					fmt.Printf("\n%s%s%s\n", colorBold, strings.TrimSpace(resp.Content), colorReset)
				}
			}
		}
		if resp.StopReason != "" && resp.StopReason != "completed" {
			fmt.Printf("%sstop: %s%s\n", colorGray, resp.StopReason, colorReset)
		}
		a.ScheduleCrossSessionMemoryExtraction(agent.WithTraceID(context.Background(), agent.NewTraceID()))
		if err := session.save(); err != nil {
			fmt.Fprintf(os.Stderr, "%ssave session: %v%s\n", colorRed, err, colorReset)
		}
		fmt.Printf("%sinput=%d cached=%d cache_write=%d output=%d retry=%d elapsed=%s%s\n\n",
			colorGray,
			resp.InputTokens,
			resp.CachedInputTokens,
			resp.CacheCreationInputTokens,
			resp.OutputTokens,
			resp.RetryAttempts,
			time.Since(start).Round(time.Millisecond),
			colorReset,
		)
	}
}

type sessionController struct {
	agent     *agent.Agent
	store     *agent.SessionStore
	sessionID string
}

func newSessionController(a *agent.Agent, projectDir string) *sessionController {
	store, err := agent.NewSessionStore(projectDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%ssession disabled: %v%s\n", colorRed, err, colorReset)
		return &sessionController{agent: a, sessionID: agent.NewSessionID()}
	}
	return &sessionController{
		agent:     a,
		store:     store,
		sessionID: agent.NewSessionID(),
	}
}

func (session *sessionController) save() error {
	if session == nil || session.store == nil || session.agent == nil {
		return nil
	}
	return session.store.AppendSnapshot(session.agent.Snapshot(session.sessionID, session.store.ProjectDir()))
}

func (session *sessionController) resume(sessionID string) error {
	if session == nil || session.store == nil || session.agent == nil {
		return errors.New("session store is unavailable")
	}
	var snapshot agent.SessionSnapshot
	var err error
	if strings.TrimSpace(sessionID) == "" || sessionID == "latest" {
		snapshot, err = session.store.LoadLatest()
	} else {
		snapshot, err = session.store.Load(sessionID)
	}
	if err != nil {
		return err
	}
	session.agent.Restore(snapshot)
	session.sessionID = snapshot.SessionID
	return session.save()
}

func (session *sessionController) list() ([]agent.SessionInfo, error) {
	if session == nil || session.store == nil {
		return nil, errors.New("session store is unavailable")
	}
	return session.store.List()
}

func printSessionHeader(session *sessionController) {
	if session == nil || session.store == nil {
		return
	}
	fmt.Printf("%ssession:%s %s  %spath:%s %s\n\n",
		colorGray, colorReset, session.sessionID,
		colorGray, colorReset, session.store.Dir(),
	)
}

func printHeader(cfg RuntimeConfig, source string) {
	fmt.Println(colorCyan + colorBold + "CodingMan" + colorReset)
	fmt.Println(colorDim + "Agent TUI. Type /help for commands, /exit to quit." + colorReset)
	fmt.Printf("%sprovider:%s %s  %smodel:%s %s  %sconfig:%s %s\n\n",
		colorGray, colorReset, cfg.Provider,
		colorGray, colorReset, cfg.ModelName,
		colorGray, colorReset, source,
	)
}

func printHelp() {
	fmt.Println(colorDim + "Slash commands:" + colorReset)
	fmt.Println("  /help                         show this help")
	fmt.Println("  /clear                        clear conversation history")
	fmt.Println("  /cache                        show prompt cache status")
	fmt.Println("  /cache on                     enable prompt cache")
	fmt.Println("  /cache off                    disable prompt cache")
	fmt.Println("  /plan                         show plan mode status")
	fmt.Println("  /plan on                      plan before execution")
	fmt.Println("  /plan off                     execute directly")
	fmt.Println("  /skill                        show loaded and active skills")
	fmt.Println("  /skill use <name>             activate a skill and its allow_tools")
	fmt.Println("  /skill clear                  clear active skill")
	fmt.Println("  /sessions                     list saved sessions for this directory")
	fmt.Println("  /resume [session_id|latest]   restore a saved session")
	fmt.Println("  /system <path>                load system prompt from file")
	fmt.Println("  /permission                   show permission mode and policy")
	fmt.Println("  /permission ask               ask before tool calls")
	fmt.Println("  /permission allow-deny        use tool allow/deny policy")
	fmt.Println("  /permission full-auto         allow all tool calls")
	fmt.Println("  /allow <tool>                 allow a tool in this session")
	fmt.Println("  /allow *                      allow all tools in this session")
	fmt.Println("  /deny <tool>                  deny a tool in this session")
	fmt.Println("  /permissions                  show permission mode and policy")
	fmt.Println("  /exit                         quit")
	fmt.Println()
}

func handleSlashCommand(a *agent.Agent, tui *tuiController, session *sessionController, cfg *RuntimeConfig, prompt string) bool {
	fields := strings.Fields(prompt)
	if len(fields) == 0 {
		return false
	}

	switch fields[0] {
	case "/sessions":
		return handleSessionsCommand(session)
	case "/resume":
		return handleResumeCommand(session, fields)
	case "/plan":
		return handlePlanCommand(tui, fields)
	case "/system":
		return handleSystemCommand(a, fields)
	case "/skill":
		return handleSkillCommand(a, fields)
	case "/permission":
		permissions := a.Permission()
		if permissions == nil {
			fmt.Println(colorRed + "permission manager is unavailable" + colorReset)
			return true
		}
		if len(fields) == 1 {
			printPermissionStatus(permissions)
			return true
		}
		mode, err := agent.ParsePermissionMode(fields[1])
		if err != nil {
			fmt.Printf("%serror:%s %v\n", colorRed, colorReset, err)
			return true
		}
		if mode == agent.PermissionModeFullAuto && !confirmFullAutoUnsandboxed(tui) {
			fmt.Println(colorGray + "permission mode unchanged" + colorReset)
			return true
		}
		if mode == agent.PermissionModeAsk && permissions.Mode() == agent.PermissionModeFullAuto {
			if !prepareSandboxForAskMode(a, tui, cfg) {
				fmt.Println(colorGray + "permission mode unchanged" + colorReset)
				return true
			}
		}
		if err := permissions.SetMode(mode); err != nil {
			fmt.Printf("%serror:%s %v\n", colorRed, colorReset, err)
			return true
		}
		if mode == agent.PermissionModeFullAuto {
			if err := a.StopSandbox(); err != nil {
				fmt.Printf("%swarning:%s stop sandbox: %v\n", colorRed, colorReset, err)
			}
		}
		fmt.Printf("%spermission mode:%s %s\n", colorGray, colorReset, mode)
		return true
	case "/cache":
		return handleCacheCommand(a, fields)
	case "/permissions":
		permissions := a.Permission()
		if permissions == nil {
			fmt.Println(colorRed + "permission manager is unavailable" + colorReset)
			return true
		}
		printPermissionStatus(permissions)
		return true
	case "/allow":
		if len(fields) != 2 {
			fmt.Println(colorRed + "usage: /allow <tool>" + colorReset)
			return true
		}
		if err := a.Permission().AllowTool(fields[1]); err != nil {
			fmt.Printf("%serror:%s %v\n", colorRed, colorReset, err)
			return true
		}
		fmt.Printf("%sallowed tool:%s %s\n", colorGray, colorReset, fields[1])
		return true
	case "/deny":
		if len(fields) != 2 {
			fmt.Println(colorRed + "usage: /deny <tool>" + colorReset)
			return true
		}
		if err := a.Permission().DenyTool(fields[1]); err != nil {
			fmt.Printf("%serror:%s %v\n", colorRed, colorReset, err)
			return true
		}
		fmt.Printf("%sdenied tool:%s %s\n", colorGray, colorReset, fields[1])
		return true
	default:
		return false
	}
}

func confirmFullAutoUnsandboxed(tui *tuiController) bool {
	fmt.Println(colorRed + "warning:" + colorReset + " full-auto disables the sandbox; dangerous operations may run on the local host.")
	fmt.Print(colorDim + "Type 1 to confirm, anything else to cancel > " + colorReset)
	selection, err := tui.readSelection(context.Background())
	if err != nil {
		fmt.Printf("%serror:%s %v\n", colorRed, colorReset, err)
		return false
	}
	return strings.TrimSpace(selection) == "1"
}

func prepareSandboxForAskMode(a *agent.Agent, tui *tuiController, cfg *RuntimeConfig) bool {
	if cfg == nil {
		fmt.Println(colorRed + "sandbox config is unavailable" + colorReset)
		return false
	}
	sandboxConfig := cfg.Sandbox
	sandboxConfig.Enabled = agent.SandboxEnabledAuto
	projectRoot := cfg.Context.ProjectRoot
	if strings.TrimSpace(projectRoot) == "" {
		projectRoot = cfg.Context.Cwd
	}
	check := checkSandboxEnvironment(projectRoot, sandboxConfig)
	if err := tui.confirmAndInstallSandboxEnvironment(check); err != nil {
		fmt.Fprintf(os.Stderr, "%ssandbox environment: %v%s\n", colorRed, err, colorReset)
		if tui.confirmFullAutoAfterSandboxFailure(err) {
			enableFullAutoUnsandboxed(cfg)
		}
		return false
	}
	fmt.Println(colorGray + "starting sandbox for ask mode..." + colorReset)
	if err := a.StartSandbox(context.Background(), sandboxConfig); err != nil {
		fmt.Fprintf(os.Stderr, "%ssandbox start: %v%s\n", colorRed, err, colorReset)
		if tui.confirmFullAutoAfterSandboxFailure(err) {
			enableFullAutoUnsandboxed(cfg)
		}
		return false
	}
	cfg.Sandbox = sandboxConfig
	fmt.Println(colorGreen + "sandbox ready; switching to ask mode" + colorReset)
	return true
}

func (tui *tuiController) confirmFullAutoAfterSandboxFailure(reason error) bool {
	fmt.Println(colorRed + "Sandbox is not ready." + colorReset)
	if reason != nil {
		fmt.Printf("Reason: %v\n", reason)
	}
	fmt.Println("Risk: switching to full-auto means bash, writes, curl, scripts, git mutations, and other dangerous operations may run directly on this Mac instead of inside the VM.")
	fmt.Print(colorDim + "Switch to full-auto without sandbox? Type 1 to confirm, anything else to stay in ask mode > " + colorReset)
	selection, err := tui.readSelection(context.Background())
	if err != nil {
		fmt.Printf("%serror:%s %v\n", colorRed, colorReset, err)
		return false
	}
	return strings.TrimSpace(selection) == "1"
}

func enableFullAutoUnsandboxed(cfg *RuntimeConfig) {
	if cfg == nil {
		return
	}
	cfg.Permission.Mode = agent.PermissionModeFullAuto
	cfg.Permission.AllowedTools = []string{"*"}
	cfg.Sandbox.Enabled = agent.SandboxEnabledFalse
	fmt.Println(colorRed + "warning:" + colorReset + " permission mode switched to full-auto; sandbox is disabled for this session.")
}

func (tui *tuiController) confirmAndInstallSandboxEnvironment(check SandboxEnvironmentCheck) error {
	if !check.Needed {
		return nil
	}
	fmt.Println(colorBold + "Sandbox environment check" + colorReset)
	fmt.Println("CodingMan uses the sandbox to run bash and file writes inside a macOS Apple VF VM in ask mode.")
	fmt.Println("The following required components are missing or incomplete:")
	fmt.Println(check.Summary())
	fmt.Print(colorDim + "Install/build these sandbox dependencies now? Type 1 to install, anything else to skip > " + colorReset)
	selection, err := tui.readSelection(context.Background())
	if err != nil {
		return err
	}
	if strings.TrimSpace(selection) != "1" {
		return errors.New("user skipped sandbox dependency installation")
	}
	total := len(check.Items)
	for i, item := range check.Items {
		fmt.Printf("%s[%d/%d] preparing:%s %s\n", colorGray, i+1, total, colorReset, item.Name)
		if strings.TrimSpace(item.Reason) != "" {
			fmt.Printf("%s      reason:%s %s\n", colorGray, colorReset, item.Reason)
		}
		if item.InstallFunc == nil {
			fmt.Printf("%s[%d/%d] skipped:%s %s has no installer\n", colorGray, i+1, total, colorReset, item.Name)
			continue
		}
		start := time.Now()
		fmt.Printf("%s[%d/%d] running:%s %s\n", colorGray, i+1, total, colorReset, item.Name)
		if err := item.InstallFunc(); err != nil {
			fmt.Printf("%s[%d/%d] failed:%s %s after %s\n", colorRed, i+1, total, colorReset, item.Name, time.Since(start).Round(time.Second))
			return fmt.Errorf("%s: %w", item.Name, err)
		}
		fmt.Printf("%s[%d/%d] done:%s %s in %s\n", colorGreen, i+1, total, colorReset, item.Name, time.Since(start).Round(time.Second))
	}
	fmt.Println(colorGreen + "sandbox environment ready" + colorReset)
	return nil
}

func handleSkillCommand(a *agent.Agent, fields []string) bool {
	if len(fields) == 1 || strings.EqualFold(fields[1], "list") {
		printSkillStatus(a)
		return true
	}
	switch strings.ToLower(fields[1]) {
	case "use":
		if len(fields) != 3 {
			fmt.Println(colorRed + "usage: /skill use <name>" + colorReset)
			return true
		}
		if err := a.SetActiveSkill(fields[2]); err != nil {
			fmt.Printf("%serror:%s %v\n", colorRed, colorReset, err)
			return true
		}
		fmt.Printf("%sactive skill:%s %s\n", colorGray, colorReset, fields[2])
		return true
	case "clear", "off":
		a.ClearActiveSkill()
		fmt.Println(colorGray + "active skill cleared" + colorReset)
		return true
	default:
		fmt.Println(colorRed + "usage: /skill [list|use <name>|clear]" + colorReset)
		return true
	}
}

func printSkillStatus(a *agent.Agent) {
	skills := a.Skills()
	active, hasActive := a.ActiveSkill()
	if len(skills) == 0 {
		fmt.Println(colorDim + "no skills loaded" + colorReset)
		return
	}
	for _, skill := range skills {
		marker := " "
		if hasActive && skill.Name == active.Name {
			marker = "*"
		}
		allowTools := "all"
		if len(skill.AllowTools) > 0 {
			allowTools = strings.Join(skill.AllowTools, ",")
		}
		fmt.Printf("%s %s  %scontext:%s %s  %sallow:%s %s\n",
			marker,
			skill.Name,
			colorGray, colorReset, skill.Context,
			colorGray, colorReset, allowTools,
		)
	}
}

func printAgentEvent(w io.Writer, event agent.AgentEvent) {
	switch event.Type {
	case agent.AgentEventSkillSelected:
		if event.SkillSelected == nil {
			return
		}
		skill := event.SkillSelected.Skill
		if strings.TrimSpace(skill.Description) != "" {
			fmt.Fprintf(w, "\n%susing skill:%s %s - %s\n", colorGray, colorReset, skill.Name, skill.Description)
		} else {
			fmt.Fprintf(w, "\n%susing skill:%s %s\n", colorGray, colorReset, skill.Name)
		}
	case agent.AgentEventFileDiff:
		if event.FileDiff == nil || strings.TrimSpace(event.FileDiff.Diff) == "" {
			return
		}
		fmt.Fprintf(w, "\n%sfile diff:%s %s\n", colorGray, colorReset, event.FileDiff.Path)
		printColoredDiff(w, event.FileDiff.Diff)
	}
}

func printColoredDiff(w io.Writer, diff string) {
	for _, line := range strings.SplitAfter(diff, "\n") {
		if line == "" {
			continue
		}
		switch {
		case strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---"):
			fmt.Fprintf(w, "%s%s%s", colorGray, line, colorReset)
		case strings.HasPrefix(line, "+"):
			fmt.Fprintf(w, "%s%s%s", colorGreen, line, colorReset)
		case strings.HasPrefix(line, "-"):
			fmt.Fprintf(w, "%s%s%s", colorRed, line, colorReset)
		default:
			fmt.Fprint(w, line)
		}
	}
	if !strings.HasSuffix(diff, "\n") {
		fmt.Fprintln(w)
	}
}

func handlePlanCommand(tui *tuiController, fields []string) bool {
	if tui == nil {
		fmt.Println(colorRed + "plan mode is unavailable" + colorReset)
		return true
	}
	if len(fields) == 1 {
		printPlanStatus(tui.planMode)
		return true
	}
	switch strings.ToLower(fields[1]) {
	case "on", "enable", "enabled":
		tui.planMode = true
	case "off", "disable", "disabled":
		tui.planMode = false
	default:
		fmt.Println(colorRed + "usage: /plan [on|off]" + colorReset)
		return true
	}
	printPlanStatus(tui.planMode)
	return true
}

func handleSessionsCommand(session *sessionController) bool {
	sessions, err := session.list()
	if err != nil {
		fmt.Printf("%serror:%s %v\n", colorRed, colorReset, err)
		return true
	}
	if len(sessions) == 0 {
		fmt.Println(colorDim + "no saved sessions for this directory" + colorReset)
		return true
	}
	for _, info := range sessions {
		fmt.Printf("%s%s%s  %supdated:%s %s  %smessages:%s %d\n",
			colorBold, info.ID, colorReset,
			colorGray, colorReset, info.UpdatedAt.Local().Format("2006-01-02 15:04:05"),
			colorGray, colorReset, info.Messages,
		)
	}
	return true
}

func handleResumeCommand(session *sessionController, fields []string) bool {
	sessionID := "latest"
	if len(fields) > 1 {
		sessionID = fields[1]
	}
	if err := session.resume(sessionID); err != nil {
		fmt.Printf("%serror:%s %v\n", colorRed, colorReset, err)
		return true
	}
	fmt.Printf("%sresumed session:%s %s\n", colorGray, colorReset, session.sessionID)
	return true
}

func printPlanStatus(enabled bool) {
	state := "off"
	if enabled {
		state = "on"
	}
	fmt.Printf("%splan mode:%s %s\n", colorGray, colorReset, state)
}

func handleSystemCommand(a *agent.Agent, fields []string) bool {
	if len(fields) != 2 {
		fmt.Println(colorRed + "usage: /system <system_prompt_path>" + colorReset)
		return true
	}
	path := expandHome(fields[1])
	if !filepath.IsAbs(path) {
		abs, err := filepath.Abs(path)
		if err == nil {
			path = abs
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		fmt.Printf("%serror:%s %v\n", colorRed, colorReset, err)
		return true
	}
	if info.IsDir() {
		fmt.Printf("%serror:%s system prompt path is a directory: %s\n", colorRed, colorReset, path)
		return true
	}
	content, err := os.ReadFile(path)
	if err != nil {
		fmt.Printf("%serror:%s %v\n", colorRed, colorReset, err)
		return true
	}
	if strings.TrimSpace(string(content)) == "" {
		fmt.Println(colorRed + "error: system prompt file is empty" + colorReset)
		return true
	}
	if err := a.SetBaseSystemPrompt(string(content)); err != nil {
		fmt.Printf("%serror:%s %v\n", colorRed, colorReset, err)
		return true
	}
	fmt.Printf("%ssystem prompt loaded:%s %s\n", colorGray, colorReset, path)
	return true
}

func handleCacheCommand(a *agent.Agent, fields []string) bool {
	if len(fields) == 1 {
		printCacheStatus(a.PromptCache())
		return true
	}
	cache := a.PromptCache()
	switch strings.ToLower(fields[1]) {
	case "on", "enable", "enabled":
		cache.Enabled = true
	case "off", "disable", "disabled":
		cache.Enabled = false
	default:
		fmt.Println(colorRed + "usage: /cache [on|off]" + colorReset)
		return true
	}
	a.SetPromptCache(cache)
	printCacheStatus(a.PromptCache())
	return true
}

func printCacheStatus(cache agent.PromptCacheConfig) {
	state := "off"
	if cache.Enabled {
		state = "on"
	}
	key := cache.Key
	if key == "" {
		key = "auto"
	}
	fmt.Printf("%sprompt cache:%s %s  %skey:%s %s  %sretention:%s %s  %sttl:%s %s\n",
		colorGray, colorReset, state,
		colorGray, colorReset, key,
		colorGray, colorReset, cache.Retention,
		colorGray, colorReset, cache.TTL,
	)
}

func printPermissionStatus(permissions *agent.PermissionManager) {
	snapshot := permissions.Snapshot()
	fmt.Printf("%spermission mode:%s %s\n", colorGray, colorReset, snapshot.Mode)
	fmt.Printf("%sallowed tools:%s %s\n", colorGray, colorReset, strings.Join(snapshot.AllowedTools, ", "))
	fmt.Printf("%sallowed commands:%s %s\n", colorGray, colorReset, strings.Join(snapshot.AllowedCommands, ", "))
	fmt.Printf("%sdenied tools:%s %s\n", colorGray, colorReset, strings.Join(snapshot.DeniedTools, ", "))
}

type tuiController struct {
	scanner      *bufio.Scanner
	selectionReq chan selectionRequest
	planMode     bool
}

type selectionRequest struct {
	response chan string
}

type agentRunResult struct {
	resp        agent.LLMResponse
	err         error
	interrupted bool
}

func newTUIController(scanner *bufio.Scanner) *tuiController {
	return &tuiController{
		scanner:      scanner,
		selectionReq: make(chan selectionRequest),
	}
}

func (tui *tuiController) runPlan(a *agent.Agent, prompt string) (agent.LLMResponse, bool, error) {
	promptText, blocks, err := buildPromptContent(prompt)
	if err != nil {
		return agent.LLMResponse{}, false, err
	}
	printAttachedImages(blocks)
	fmt.Printf("%splanning...%s\n", colorGray, colorReset)
	resp, err := a.Plan(context.Background(), promptText, blocks...)
	if err != nil {
		return resp, false, err
	}
	fmt.Println(colorDim + "Choose:" + colorReset)
	fmt.Println("  1. Execute this plan")
	fmt.Println("  2. Skip")
	fmt.Print(colorDim + "Select option [1-2] > " + colorReset)
	selection, err := tui.readSelection(context.Background())
	if err != nil {
		return resp, false, err
	}
	return resp, selection == "1", nil
}

func (tui *tuiController) runAgent(a *agent.Agent, prompt string) (agent.LLMResponse, bool, error) {
	promptText, blocks, err := buildPromptContent(prompt)
	if err != nil {
		return agent.LLMResponse{}, false, err
	}
	printAttachedImages(blocks)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan agentRunResult, 1)
	go func() {
		resp, err := a.RunToolLoop(ctx, promptText, blocks...)
		done <- agentRunResult{resp: resp, err: err}
	}()

	if !term.IsTerminal(int(os.Stdin.Fd())) {
		result := <-done
		return result.resp, false, result.err
	}

	spinnerStop := startSpinner("running")
	defer spinnerStop()

	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		result := <-done
		return result.resp, false, result.err
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	if err := syscall.SetNonblock(int(os.Stdin.Fd()), true); err != nil {
		result := <-done
		return result.resp, false, result.err
	}
	defer syscall.SetNonblock(int(os.Stdin.Fd()), false)

	var pending *selectionRequest
	var interrupted bool
	buf := make([]byte, 1)
	for {
		select {
		case result := <-done:
			result.interrupted = interrupted
			return result.resp, result.interrupted, result.err
		case req := <-tui.selectionReq:
			pending = &req
		default:
		}

		n, readErr := os.Stdin.Read(buf)
		if n > 0 {
			key := buf[0]
			if pending != nil && key >= '1' && key <= '4' {
				pending.response <- string(key)
				pending = nil
				continue
			}
			if key == 27 {
				interrupted = true
				cancel()
			}
		}
		if readErr != nil && !errors.Is(readErr, syscall.EAGAIN) && !errors.Is(readErr, syscall.EWOULDBLOCK) && !errors.Is(readErr, io.EOF) {
			cancel()
			result := <-done
			return result.resp, interrupted, readErr
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (tui *tuiController) permissionPrompt(ctx context.Context, request agent.PermissionRequest) (agent.PermissionDecision, string, error) {
	fmt.Printf("\n%sTool permission request%s\n", colorBold, colorReset)
	fmt.Printf("%stool:%s %s\n", colorGray, colorReset, request.ToolName)
	if request.ToolUseID != "" {
		fmt.Printf("%sid:%s %s\n", colorGray, colorReset, request.ToolUseID)
	}
	fmt.Printf("%sinput:%s\n%s\n", colorGray, colorReset, request.InputJSON())
	if fallback, _ := request.ToolInput["sandbox_fallback"].(bool); fallback {
		fmt.Println(colorRed + "Sandbox is unavailable. Allow this operation to run locally once?" + colorReset)
		fmt.Println(colorDim + "Choose:" + colorReset)
		fmt.Println("  1. Yes, allow local fallback once")
		fmt.Println("  2. No, deny")
		fmt.Print(colorDim + "Select option [1-2] > " + colorReset)
		selection, err := tui.readSelection(ctx)
		if err != nil {
			return agent.PermissionDecisionDeny, "", err
		}
		if selection == "1" {
			return agent.PermissionDecisionAllow, "", nil
		}
		return agent.PermissionDecisionDeny, "denied by user", nil
	}
	fmt.Println(colorDim + "Choose:" + colorReset)
	fmt.Println("  1. Yes, allow once")
	fmt.Println("  2. No, deny once")
	if request.ToolName == "bash" {
		fmt.Println("  3. Always allow this command")
	} else {
		fmt.Println("  3. Always allow this tool")
	}
	fmt.Println("  4. Always deny this tool")
	fmt.Print(colorDim + "Select option [1-4] > " + colorReset)

	selection, err := tui.readSelection(ctx)
	if err != nil {
		return agent.PermissionDecisionDeny, "", err
	}
	switch selection {
	case "1":
		return agent.PermissionDecisionAllow, "", nil
	case "2":
		return agent.PermissionDecisionDeny, "denied by user", nil
	case "3":
		return agent.PermissionDecisionAllowRule, request.AllowRuleValue(), nil
	case "4":
		return agent.PermissionDecisionDenyTool, "denied by user", nil
	default:
		return agent.PermissionDecisionDeny, "invalid permission response", nil
	}
}

func (tui *tuiController) readSelection(ctx context.Context) (string, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		if !tui.scanner.Scan() {
			if err := tui.scanner.Err(); err != nil {
				return "", err
			}
			return "", errors.New("input closed")
		}
		return strings.TrimSpace(tui.scanner.Text()), nil
	}

	req := selectionRequest{response: make(chan string, 1)}
	select {
	case tui.selectionReq <- req:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	select {
	case value := <-req.response:
		fmt.Println(value)
		return value, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func startSpinner(status string) func() {
	done := make(chan struct{})
	go func() {
		frames := []string{"-", "\\", "|", "/"}
		ticker := time.NewTicker(120 * time.Millisecond)
		defer ticker.Stop()
		i := 0
		for {
			select {
			case <-done:
				fmt.Printf("\r%s%s%s\r", colorGray, strings.Repeat(" ", len(status)+4), colorReset)
				return
			case <-ticker.C:
				fmt.Printf("\r%s%s %s%s", colorGray, frames[i%len(frames)], status, colorReset)
				i++
			}
		}
	}()
	return func() { close(done) }
}

var markdownImagePattern = regexp.MustCompile(`!\[[^\]]*]\(([^)]+)\)`)

func buildPromptContent(input string) (string, []agent.ContentBlock, error) {
	text := input
	refs := make([]string, 0)

	for _, match := range markdownImagePattern.FindAllStringSubmatch(input, -1) {
		if len(match) >= 2 {
			refs = append(refs, strings.TrimSpace(match[1]))
			text = strings.Replace(text, match[0], "", 1)
		}
	}

	for _, field := range strings.Fields(text) {
		ref := cleanImageRef(field)
		if ref == "" {
			continue
		}
		if strings.HasPrefix(ref, "@") {
			refs = append(refs, strings.TrimPrefix(ref, "@"))
			text = strings.Replace(text, field, "", 1)
			continue
		}
		if looksLikeImageRef(ref) {
			refs = append(refs, ref)
			text = strings.Replace(text, field, "", 1)
		}
	}

	blocks := make([]agent.ContentBlock, 0, len(refs))
	seen := map[string]struct{}{}
	for _, ref := range refs {
		ref = cleanImageRef(ref)
		if ref == "" {
			continue
		}
		if _, exists := seen[ref]; exists {
			continue
		}
		seen[ref] = struct{}{}
		block, err := imageBlockFromRef(ref)
		if err != nil {
			return "", nil, err
		}
		blocks = append(blocks, block)
	}

	return strings.TrimSpace(text), blocks, nil
}

func printAttachedImages(blocks []agent.ContentBlock) {
	count := 0
	for _, block := range blocks {
		if block.Type == agent.ContentTypeImage {
			count++
		}
	}
	if count > 0 {
		fmt.Printf("%sattached images:%s %d\n", colorGray, colorReset, count)
	}
}

func cleanImageRef(value string) string {
	value = strings.TrimSpace(value)
	value = strings.Trim(value, `"'`)
	value = strings.TrimRight(value, ".,;")
	if strings.HasPrefix(value, "file://") {
		parsed, err := url.Parse(value)
		if err == nil {
			if path, err := url.PathUnescape(parsed.Path); err == nil {
				value = path
			}
		}
	}
	return value
}

func looksLikeImageRef(value string) bool {
	if strings.HasPrefix(value, "@") {
		return true
	}
	if strings.HasPrefix(value, "data:image/") {
		return true
	}
	if strings.HasPrefix(value, "http://") || strings.HasPrefix(value, "https://") {
		parsed, err := url.Parse(value)
		if err != nil {
			return false
		}
		return supportedImageExt(strings.ToLower(filepath.Ext(parsed.Path)))
	}
	ext := strings.ToLower(filepath.Ext(value))
	return supportedImageExt(ext)
}

func imageBlockFromRef(ref string) (agent.ContentBlock, error) {
	if strings.HasPrefix(ref, "data:image/") {
		mediaType, data, ok := strings.Cut(strings.TrimPrefix(ref, "data:"), ";base64,")
		if !ok || mediaType == "" || data == "" {
			return agent.ContentBlock{}, fmt.Errorf("invalid image data URL")
		}
		return agent.ImageBase64Block(mediaType, data), nil
	}
	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		return agent.ImageURLBlock(ref), nil
	}

	path := expandHome(ref)
	if !filepath.IsAbs(path) {
		abs, err := filepath.Abs(path)
		if err == nil {
			path = abs
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		return agent.ContentBlock{}, fmt.Errorf("image not found: %s", ref)
	}
	if info.IsDir() {
		return agent.ContentBlock{}, fmt.Errorf("image path is a directory: %s", ref)
	}
	if !supportedImageExt(strings.ToLower(filepath.Ext(path))) {
		return agent.ContentBlock{}, fmt.Errorf("unsupported image type: %s", ref)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return agent.ContentBlock{}, err
	}
	mediaType := mime.TypeByExtension(strings.ToLower(filepath.Ext(path)))
	if mediaType == "" {
		mediaType = http.DetectContentType(data)
	}
	if !strings.HasPrefix(mediaType, "image/") {
		return agent.ContentBlock{}, fmt.Errorf("unsupported image media type %q: %s", mediaType, ref)
	}
	return agent.ImageBase64Block(mediaType, base64.StdEncoding.EncodeToString(data)), nil
}

func supportedImageExt(ext string) bool {
	switch ext {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp":
		return true
	default:
		return false
	}
}

func expandHome(path string) string {
	if path == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
	}
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(path, "~/"))
		}
	}
	return path
}

func loadRuntimeConfig(projectRoot string, launchDir string) (RuntimeConfig, string, error) {
	envPath := filepath.Join(projectRoot, ".env")
	values := map[string]string{}
	source := "environment"

	if _, err := os.Stat(envPath); err == nil {
		loaded, err := readDotEnv(envPath)
		if err != nil {
			return RuntimeConfig{}, "", err
		}
		values = loaded
		source = envPath
	} else if !os.IsNotExist(err) {
		return RuntimeConfig{}, "", err
	} else {
		values = readProcessEnv()
	}
	sandboxValues, err := ensureUserSandboxConfig()
	if err != nil {
		return RuntimeConfig{}, "", err
	}
	mergeSandboxConfigValues(values, sandboxValues)
	applySandboxEnvDefaults(sandboxValues)

	cfg := RuntimeConfig{
		Provider:  strings.TrimSpace(values["PROVIDER"]),
		ModelName: strings.TrimSpace(values["MODEL_NAME"]),
		APIKey:    strings.TrimSpace(values["API_KEY"]),
		BaseURL:   strings.TrimSpace(values["BASE_URL"]),
		Context:   agent.DefaultContextConfig(),
	}
	cfg.Context.Cwd = valueOrDefault(values["CWD"], launchDir)
	cfg.Context.ProjectRoot = valueOrDefault(values["PROJECT_ROOT"], findMemoryProjectRoot(cfg.Context.Cwd))
	cfg.Context.BaseSystem = values["BASE_SYSTEM"]
	cfg.Context.IncludeDate = boolValue(values, "INCLUDE_DATE", cfg.Context.IncludeDate)
	cfg.Context.LoadAgentsMD = boolValue(values, "LOAD_AGENTS_MD", cfg.Context.LoadAgentsMD)
	cfg.Context.LoadSkills = boolValue(values, "LOAD_SKILLS", cfg.Context.LoadSkills)
	cfg.Context.AutoCompact = boolValue(values, "AUTO_COMPACT", cfg.Context.AutoCompact)
	cfg.Context.CompactThreshold = intValue(values, "COMPACT_THRESHOLD", cfg.Context.CompactThreshold)
	cfg.Context.KeepRecentRounds = intValue(values, "KEEP_RECENT_ROUNDS", cfg.Context.KeepRecentRounds)
	cfg.Context.MaxAgentsMDBytes = intValue(values, "MAX_AGENTS_MD_BYTES", cfg.Context.MaxAgentsMDBytes)
	cfg.Context.ProgressiveMemoryMaxChars = intValue(values, "PROGRESSIVE_MEMORY_MAX_CHARS", cfg.Context.ProgressiveMemoryMaxChars)
	cfg.Context.ProgressiveSkillMaxChars = intValue(values, "PROGRESSIVE_SKILL_MAX_CHARS", cfg.Context.ProgressiveSkillMaxChars)

	cfg.MaxLLMTurns = intValue(values, "MAX_LLM_TURNS", 20)
	cfg.MaxToolCalls = intValue(values, "MAX_TOOL_CALLS", 50)
	cfg.MaxParallelTools = intValue(values, "MAX_PARALLEL_TOOL_CALLS", 4)
	cfg.MaxToolErrors = intValue(values, "MAX_CONSECUTIVE_TOOL_ERRORS", 3)
	cfg.MaxAPIErrors = intValue(values, "MAX_CONSECUTIVE_API_ERRORS", 3)
	cfg.MaxSubAgentDepth = intValue(values, "MAX_SUB_AGENT_DEPTH", 1)
	cfg.MaxSubAgents = intValue(values, "MAX_CONCURRENT_SUB_AGENTS", 4)
	cfg.SessionMemoryThreshold = intValue(values, "SESSION_MEMORY_TOOL_THRESHOLD", 10)
	cfg.SkillEvolutionThreshold = intValue(values, "SKILL_EVOLUTION_TOOL_THRESHOLD", 10)
	cfg.SkillEviction = agent.SkillEvictionConfig{
		Enabled:            boolValue(values, "SKILL_EVICTION_ENABLED", true),
		UnusedDays:         intValue(values, "SKILL_EVICTION_UNUSED_DAYS", 90),
		MinUses:            intValue(values, "SKILL_EVICTION_MIN_USES", 3),
		CheckIntervalHours: intValue(values, "SKILL_EVICTION_CHECK_INTERVAL_HOURS", 24),
	}
	cfg.MaxSessionMemoryEntries = intValue(values, "SESSION_MEMORY_MAX_ENTRIES", 8)
	cfg.MaxSessionMemoryChars = intValue(values, "SESSION_MEMORY_MAX_CHARS", 8000)
	cfg.MaxCrossMemoryChars = intValue(values, "CROSS_SESSION_MEMORY_MAX_CHARS", 12000)
	cfg.Coordination = agent.CoordinationConfig{
		SharedTempDir:     values["WORKER_SHARED_TEMP_DIR"],
		EnableGitWorktree: boolValue(values, "WORKER_GIT_WORKTREE", false),
		WorktreeBaseDir:   values["WORKER_WORKTREE_BASE_DIR"],
	}
	cfg.Sandbox = agent.SandboxConfig{
		Enabled:           valueOrDefault(values["SANDBOX_ENABLED"], agent.SandboxEnabledAuto),
		RootFS:            expandHome(values["SANDBOX_ROOTFS"]),
		VFKitPath:         valueOrDefault(values["SANDBOX_VFKIT"], "vfkit"),
		CPUs:              intValue(values, "SANDBOX_CPUS", 2),
		Memory:            valueOrDefault(values["SANDBOX_MEMORY"], "2048M"),
		KeepaliveInterval: durationValue(values, "SANDBOX_KEEPALIVE_INTERVAL", 30*time.Second),
		SocketPath:        expandHome(values["SANDBOX_SOCKET_PATH"]),
		Bootstrap:         valueOrDefault(values["SANDBOX_BOOTSTRAP"], agent.SandboxBootstrapAuto),
		MCPServerPath:     expandHome(values["SANDBOX_MCP_SERVER"]),
		EFIVariableStore:  expandHome(values["SANDBOX_EFI_VARIABLE_STORE"]),
	}
	cfg.SandboxCheck = checkSandboxEnvironment(projectRoot, cfg.Sandbox)
	hooks, err := loadHooksConfig(projectRoot)
	if err != nil {
		return RuntimeConfig{}, "", err
	}
	cfg.Hooks = hooks
	mcp, err := loadMCPConfig(projectRoot)
	if err != nil {
		return RuntimeConfig{}, "", err
	}
	cfg.MCP = mcp

	cfg.EnableToolBudget = boolValue(values, "ENABLE_TOOL_BUDGET", true)
	cfg.ToolBudget = agent.ToolBudget{
		MaxLen:  intValue(values, "TOOL_BUDGET_MAX_LEN", 10000),
		HeadLen: intValue(values, "TOOL_BUDGET_HEAD_LEN", 3000),
		TailLen: intValue(values, "TOOL_BUDGET_TAIL_LEN", 3000),
	}

	cfg.Retry = agent.RetryConfig{
		MaxRetries:   intValue(values, "RETRY_MAX_RETRIES", 3),
		InitialDelay: durationValue(values, "RETRY_INITIAL_DELAY", time.Second),
		MaxDelay:     durationValue(values, "RETRY_MAX_DELAY", 60*time.Second),
		Multiplier:   floatValue(values, "RETRY_MULTIPLIER", 2.0),
		Jitter:       floatValue(values, "RETRY_JITTER", 0.2),
	}
	cfg.PromptCache = agent.PromptCacheConfig{
		Enabled:   boolValue(values, "PROMPT_CACHE_ENABLED", true),
		Key:       values["PROMPT_CACHE_KEY"],
		Retention: valueOrDefault(values["PROMPT_CACHE_RETENTION"], agent.PromptCacheRetentionInMemory),
		TTL:       valueOrDefault(values["PROMPT_CACHE_TTL"], agent.PromptCacheTTL5m),
	}
	cfg.LogPath = valueOrDefault(values["LOG_PATH"], filepath.Join(projectRoot, ".codingman.log"))

	if err := validateRuntimeConfig(cfg); err != nil {
		return RuntimeConfig{}, "", err
	}
	return cfg, source, nil
}

func ensureUserSandboxConfig() (map[string]string, error) {
	defaults, err := defaultUserSandboxConfig()
	if err != nil {
		return nil, err
	}
	configPath, err := userSandboxConfigPath()
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
			return nil, err
		}
		if err := writeSandboxConfig(configPath, defaults); err != nil {
			return nil, err
		}
		return defaults, nil
	} else if err != nil {
		return nil, err
	}
	values, err := readDotEnv(configPath)
	if err != nil {
		return nil, err
	}
	changed := false
	if migrated, ok := migrateLegacySandboxRootFS(values, defaults); ok {
		values["SANDBOX_ROOTFS"] = migrated
		changed = true
	}
	for key, value := range defaults {
		if strings.TrimSpace(values[key]) == "" {
			values[key] = value
			changed = true
		}
	}
	if changed {
		if err := writeSandboxConfig(configPath, values); err != nil {
			return nil, err
		}
	}
	return values, nil
}

func migrateLegacySandboxRootFS(values map[string]string, defaults map[string]string) (string, bool) {
	current := strings.TrimSpace(values["SANDBOX_ROOTFS"])
	if current == "" {
		return "", false
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false
	}
	legacy := filepath.Join(home, ".codingman", "sandbox", "rootfs")
	if expandHome(current) != legacy {
		return "", false
	}
	if _, err := os.Stat(legacy); err == nil {
		return "", false
	}
	return defaults["SANDBOX_ROOTFS"], true
}

func userSandboxConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codingman", "sandbox", "config"), nil
}

func defaultUserSandboxConfig() (map[string]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return map[string]string{
		"SANDBOX_ENABLED":               agent.SandboxEnabledAuto,
		"SANDBOX_ROOTFS":                filepath.Join(home, ".codingman", "sandbox", "debian-12-slim-arm64.raw"),
		"SANDBOX_VFKIT":                 "vfkit",
		"SANDBOX_CPUS":                  "2",
		"SANDBOX_MEMORY":                "2048M",
		"SANDBOX_KEEPALIVE_INTERVAL":    "30s",
		"SANDBOX_SOCKET_PATH":           "",
		"SANDBOX_BOOTSTRAP":             agent.SandboxBootstrapAuto,
		"SANDBOX_MCP_SERVER":            filepath.Join(home, ".codingman", "sandbox", "mcp-server-linux-arm64"),
		"SANDBOX_EFI_VARIABLE_STORE":    filepath.Join(home, ".codingman", "sandbox", "efi-variable-store"),
		"SANDBOX_ROOTFS_SOURCE":         "",
		"DEBIAN_IMAGE_URLS":             defaultDebianSandboxImageURL + "|https://chuangtzu.ftp.acc.umu.se/images/cloud/bookworm/latest/debian-12-genericcloud-arm64.raw|https://saimei.ftp.acc.umu.se/images/cloud/bookworm/latest/debian-12-genericcloud-arm64.raw",
		"DEBIAN_SHA512_URLS":            "https://cloud.debian.org/images/cloud/bookworm/latest/SHA512SUMS|https://chuangtzu.ftp.acc.umu.se/images/cloud/bookworm/latest/SHA512SUMS|https://saimei.ftp.acc.umu.se/images/cloud/bookworm/latest/SHA512SUMS",
		"CONFIRM_FULL_AUTO_UNSANDBOXED": "false",
	}, nil
}

func writeSandboxConfig(path string, values map[string]string) error {
	var builder strings.Builder
	builder.WriteString("# CodingMan sandbox config\n")
	for _, key := range sandboxConfigKeys() {
		builder.WriteString(key)
		builder.WriteString("=")
		builder.WriteString(values[key])
		builder.WriteString("\n")
	}
	return os.WriteFile(path, []byte(builder.String()), 0o600)
}

func sandboxConfigKeys() []string {
	return []string{
		"SANDBOX_ENABLED",
		"SANDBOX_ROOTFS",
		"SANDBOX_VFKIT",
		"SANDBOX_CPUS",
		"SANDBOX_MEMORY",
		"SANDBOX_KEEPALIVE_INTERVAL",
		"SANDBOX_SOCKET_PATH",
		"SANDBOX_BOOTSTRAP",
		"SANDBOX_MCP_SERVER",
		"SANDBOX_EFI_VARIABLE_STORE",
		"SANDBOX_ROOTFS_SOURCE",
		"DEBIAN_IMAGE_URLS",
		"DEBIAN_SHA512_URLS",
		"CONFIRM_FULL_AUTO_UNSANDBOXED",
	}
}

func mergeSandboxConfigValues(values map[string]string, sandboxValues map[string]string) {
	for _, key := range sandboxConfigKeys() {
		if strings.TrimSpace(values[key]) != "" {
			continue
		}
		if envValue := strings.TrimSpace(os.Getenv(key)); envValue != "" {
			values[key] = envValue
			continue
		}
		values[key] = sandboxValues[key]
	}
}

func applySandboxEnvDefaults(values map[string]string) {
	for _, key := range sandboxConfigKeys() {
		setDefaultEnv(key, values[key])
	}
}

func ensureVFKitDefaultPath() {
	if _, err := exec.LookPath("vfkit"); err == nil {
		return
	}
	for _, dir := range []string{"/opt/homebrew/bin", "/usr/local/bin"} {
		candidate := filepath.Join(dir, "vfkit")
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			pathValue := os.Getenv("PATH")
			if pathValue == "" {
				_ = os.Setenv("PATH", dir)
			} else {
				_ = os.Setenv("PATH", dir+string(os.PathListSeparator)+pathValue)
			}
			return
		}
	}
}

func setDefaultEnv(key string, value string) {
	if strings.TrimSpace(os.Getenv(key)) != "" {
		return
	}
	_ = os.Setenv(key, value)
}

type SandboxEnvironmentCheck struct {
	Needed bool
	Items  []SandboxInstallItem
}

type SandboxInstallItem struct {
	Name        string
	Reason      string
	InstallFunc func() error
}

func (check SandboxEnvironmentCheck) Summary() string {
	if !check.Needed {
		return "sandbox environment is ready"
	}
	lines := make([]string, 0, len(check.Items))
	for _, item := range check.Items {
		lines = append(lines, fmt.Sprintf("- %s: %s", item.Name, item.Reason))
	}
	return strings.Join(lines, "\n")
}

func checkSandboxEnvironment(projectRoot string, config agent.SandboxConfig) SandboxEnvironmentCheck {
	if !shouldBootstrapSandbox(config) || runtime.GOOS != "darwin" {
		return SandboxEnvironmentCheck{}
	}
	var items []SandboxInstallItem
	if _, err := exec.LookPath("brew"); err != nil {
		items = append(items, SandboxInstallItem{
			Name:   "Homebrew",
			Reason: "needed to install vfkit when it is missing",
			InstallFunc: func() error {
				return errors.New("Homebrew is required; install it from https://brew.sh and restart CodingMan")
			},
		})
	}
	if _, err := exec.LookPath(config.VFKitPath); err != nil {
		items = append(items, SandboxInstallItem{
			Name:   "vfkit",
			Reason: "required to start Apple Virtualization Framework VMs",
			InstallFunc: func() error {
				return runBrewInstall("vfkit")
			},
		})
	}
	mcpServer := config.MCPServerPath
	if strings.TrimSpace(mcpServer) == "" {
		values, err := defaultUserSandboxConfig()
		if err != nil {
			items = append(items, SandboxInstallItem{Name: "sandbox config", Reason: err.Error(), InstallFunc: func() error { return err }})
			return SandboxEnvironmentCheck{Needed: true, Items: items}
		}
		mcpServer = values["SANDBOX_MCP_SERVER"]
	}
	if _, err := os.Stat(mcpServer); err != nil {
		items = append(items, SandboxInstallItem{
			Name:   "sandbox MCP server",
			Reason: "required inside the VM to expose bash/file/grep MCP tools",
			InstallFunc: func() error {
				return buildSandboxMCPServer(projectRoot, mcpServer)
			},
		})
	}
	needsImageBuild := false
	if _, err := os.Stat(config.RootFS); err == nil {
		if cloudInitReady(config.RootFS) && sandboxImageSourceReady(config.RootFS) {
			return SandboxEnvironmentCheck{Needed: len(items) > 0, Items: items}
		}
		needsImageBuild = true
	} else if err != nil && !os.IsNotExist(err) {
		items = append(items, SandboxInstallItem{Name: "sandbox rootfs", Reason: err.Error(), InstallFunc: func() error { return err }})
		return SandboxEnvironmentCheck{Needed: true, Items: items}
	} else {
		needsImageBuild = true
	}
	if needsImageBuild {
		if _, err := exec.LookPath("aria2c"); err != nil {
			items = append(items, SandboxInstallItem{
				Name:   "aria2",
				Reason: "recommended downloader for resilient multi-connection Debian image downloads",
				InstallFunc: func() error {
					return runBrewInstall("aria2")
				},
			})
		}
		if _, err := os.Stat(config.RootFS); err == nil {
			items = append(items, SandboxInstallItem{
				Name:   "sandbox cloud-init/image metadata",
				Reason: "required to provision the Debian 12 slim VM and confirm the default genericcloud image source",
				InstallFunc: func() error {
					return buildSandboxRootFS(projectRoot, config.RootFS, mcpServer)
				},
			})
			return SandboxEnvironmentCheck{Needed: len(items) > 0, Items: items}
		}
	}
	items = append(items, SandboxInstallItem{
		Name:   "Debian 12 slim VM image",
		Reason: "required bootable arm64 raw disk for Apple VF sandbox execution",
		InstallFunc: func() error {
			return buildSandboxRootFS(projectRoot, config.RootFS, mcpServer)
		},
	})
	return SandboxEnvironmentCheck{Needed: len(items) > 0, Items: items}
}

func cloudInitReady(rootfs string) bool {
	dir := sandboxCloudInitDir(rootfs)
	for _, name := range []string{"user-data", "meta-data"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || info.IsDir() || info.Size() == 0 {
			return false
		}
	}
	return true
}

func sandboxImageSourceReady(rootfs string) bool {
	defaults, err := defaultUserSandboxConfig()
	if err != nil {
		return true
	}
	if expandHome(rootfs) != defaults["SANDBOX_ROOTFS"] {
		return true
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(rootfs), "image-url"))
	if err != nil {
		return false
	}
	imageURL := strings.TrimSpace(string(data))
	if imageURL == defaultDebianSandboxImageURL {
		return true
	}
	for _, configuredURL := range strings.Split(os.Getenv("DEBIAN_IMAGE_URLS"), "|") {
		if imageURL == strings.TrimSpace(configuredURL) {
			return true
		}
	}
	parsed, err := url.Parse(imageURL)
	if err != nil {
		return false
	}
	return strings.EqualFold(filepath.Base(parsed.Path), debianSandboxImageName)
}

func sandboxCloudInitDir(rootfs string) string {
	if strings.TrimSpace(rootfs) == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(rootfs), "cloud-init")
}

func shouldBootstrapSandbox(config agent.SandboxConfig) bool {
	if config.Enabled == agent.SandboxEnabledFalse {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(config.Bootstrap)) {
	case "", agent.SandboxBootstrapAuto, agent.SandboxBootstrapTrue:
		return true
	case agent.SandboxBootstrapFalse:
		return false
	default:
		return true
	}
}

func ensureHostCommand(command string, brewPackage string, brewArgs []string) error {
	if strings.TrimSpace(command) == "" {
		return errors.New("command name is required")
	}
	if _, err := exec.LookPath(command); err == nil {
		return nil
	}
	if len(brewArgs) == 0 {
		return fmt.Errorf("%s not found in PATH", command)
	}
	brew, err := exec.LookPath("brew")
	if err != nil {
		return fmt.Errorf("%s not found and Homebrew is unavailable", command)
	}
	cmd := exec.Command(brew, brewArgs...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if brewPackage != "" {
			return fmt.Errorf("install %s: %w", brewPackage, err)
		}
		return err
	}
	if _, err := exec.LookPath(command); err != nil {
		return fmt.Errorf("%s still not found after install", command)
	}
	return nil
}

func runBrewInstall(pkg string) error {
	brew, err := exec.LookPath("brew")
	if err != nil {
		return fmt.Errorf("Homebrew is unavailable: %w", err)
	}
	cmd := exec.Command(brew, "install", pkg)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func buildSandboxMCPServer(projectRoot string, output string) error {
	if strings.TrimSpace(output) == "" {
		return errors.New("SANDBOX_MCP_SERVER is required")
	}
	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		return err
	}
	cmd := exec.Command("go", "build", "-o", output, "./cmd/sandbox-mcp-server")
	cmd.Dir = projectRoot
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=arm64", "CGO_ENABLED=0")
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func buildSandboxRootFS(projectRoot string, rootfs string, mcpServer string) error {
	if strings.TrimSpace(rootfs) == "" {
		return errors.New("SANDBOX_ROOTFS is required")
	}
	script := filepath.Join(projectRoot, "sandbox", "build-rootfs.sh")
	if _, err := os.Stat(script); err != nil {
		return err
	}
	cmd := exec.Command(script)
	cmd.Dir = projectRoot
	cmd.Env = append(os.Environ(), "ROOTFS_IMAGE="+rootfs, "MCP_SERVER="+mcpServer)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func validateRuntimeConfig(cfg RuntimeConfig) error {
	missing := make([]string, 0)
	if cfg.Provider == "" {
		missing = append(missing, "PROVIDER")
	}
	if cfg.ModelName == "" {
		missing = append(missing, "MODEL_NAME")
	}
	if cfg.APIKey == "" {
		missing = append(missing, "API_KEY")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required config: %s", strings.Join(missing, ", "))
	}
	return nil
}

func loadMCPConfig(projectRoot string) (agent.MCPConfig, error) {
	var merged agent.MCPConfig
	paths := []string{}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".codingman", "settings.json"))
	}
	paths = append(paths, filepath.Join(projectRoot, "settings.json"))
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return agent.MCPConfig{}, err
		}
		servers, err := parseMCPServers(data)
		if err != nil {
			return agent.MCPConfig{}, fmt.Errorf("%s: %w", path, err)
		}
		merged.Servers = append(merged.Servers, servers...)
	}
	return merged, nil
}

func parseMCPServers(data []byte) ([]agent.MCPServerConfig, error) {
	var raw struct {
		MCPServers      json.RawMessage `json:"mcp_servers"`
		CamelMCPServers json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	payload := raw.MCPServers
	if len(payload) == 0 {
		payload = raw.CamelMCPServers
	}
	if len(payload) == 0 || string(payload) == "null" {
		return nil, nil
	}
	var list []agent.MCPServerConfig
	if err := json.Unmarshal(payload, &list); err == nil {
		return list, nil
	}
	var byName map[string]agent.MCPServerConfig
	if err := json.Unmarshal(payload, &byName); err != nil {
		return nil, err
	}
	for name, server := range byName {
		if server.Name == "" {
			server.Name = name
		}
		list = append(list, server)
	}
	return list, nil
}

func loadHooksConfig(projectRoot string) (*agent.HookManager, error) {
	var merged agent.HooksConfig
	paths := []string{}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".codingman", "settings.json"))
	}
	paths = append(paths, filepath.Join(projectRoot, "settings.json"))
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		var fileConfig struct {
			Hooks []agent.HookConfig `json:"hooks"`
		}
		if err := json.Unmarshal(data, &fileConfig); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		merged.Hooks = append(merged.Hooks, fileConfig.Hooks...)
	}
	return agent.NewHookManager(merged)
}

func readDotEnv(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	values := map[string]string{}
	scanner := bufio.NewScanner(file)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("%s:%d: expected KEY=VALUE", path, lineNumber)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		value = strings.Trim(value, `"'`)
		if key == "" {
			return nil, fmt.Errorf("%s:%d: empty key", path, lineNumber)
		}
		values[key] = value
	}
	return values, scanner.Err()
}

func readProcessEnv() map[string]string {
	values := map[string]string{}
	for _, item := range os.Environ() {
		key, value, ok := strings.Cut(item, "=")
		if ok {
			values[key] = value
		}
	}
	return values
}

func findProjectRoot(start string) (string, error) {
	current, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(current, "go.mod")); err == nil {
			return current, nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", errors.New("go.mod not found")
		}
		current = parent
	}
}

func findMemoryProjectRoot(start string) string {
	current, err := filepath.Abs(start)
	if err != nil {
		return start
	}
	for {
		if _, err := os.Stat(filepath.Join(current, ".git")); err == nil {
			return current
		}
		if _, err := os.Stat(filepath.Join(current, "go.mod")); err == nil {
			return current
		}
		if _, err := os.Stat(filepath.Join(current, ".codingman")); err == nil {
			return current
		}
		parent := filepath.Dir(current)
		if parent == current {
			return start
		}
		current = parent
	}
}

func valueOrDefault(value string, defaultValue string) string {
	if strings.TrimSpace(value) == "" {
		return defaultValue
	}
	return value
}

func intValue(values map[string]string, key string, defaultValue int) int {
	value := strings.TrimSpace(values[key])
	if value == "" {
		return defaultValue
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return defaultValue
	}
	return parsed
}

func floatValue(values map[string]string, key string, defaultValue float64) float64 {
	value := strings.TrimSpace(values[key])
	if value == "" {
		return defaultValue
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return defaultValue
	}
	return parsed
}

func boolValue(values map[string]string, key string, defaultValue bool) bool {
	value := strings.TrimSpace(values[key])
	if value == "" {
		return defaultValue
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return defaultValue
	}
	return parsed
}

func durationValue(values map[string]string, key string, defaultValue time.Duration) time.Duration {
	value := strings.TrimSpace(values[key])
	if value == "" {
		return defaultValue
	}
	parsed, err := time.ParseDuration(value)
	if err == nil {
		return parsed
	}
	seconds, err := strconv.Atoi(value)
	if err != nil {
		return defaultValue
	}
	return time.Duration(seconds) * time.Second
}

func fatal(scope string, err error) {
	fmt.Fprintf(os.Stderr, "%s%s:%s %v\n", colorRed, scope, colorReset, err)
	os.Exit(1)
}
