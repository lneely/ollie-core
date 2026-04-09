package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"ollie/internal/backend"
	"ollie/internal/config"
	execpkg "ollie/internal/exec"
	"ollie/internal/tui"
	"ollie/pkg/agent"
)

func main() {
	sessionFlag := flag.String("session", "", "resume a session by ID")
	promptFlag := flag.String("prompt", "", "run a single prompt non-interactively and exit")
	flag.Parse()
	extraArgs := flag.Args()

	home, _ := os.UserHomeDir()
	agentsDir := home + "/.config/ollie/agents"
	sessionsDir := home + "/.config/ollie/sessions"
	if err := os.MkdirAll(sessionsDir, 0700); err != nil {
		fmt.Fprintln(os.Stderr, "sessions dir:", err)
		os.Exit(1)
	}

	be, err := backend.New()
	if err != nil {
		fmt.Fprintln(os.Stderr, "failed to create backend:", err)
		os.Exit(1)
	}

	if modelName := os.Getenv("OLLIE_MODEL"); modelName != "" {
		be.SetModel(modelName)
	}

	builtinExec := execpkg.New(
		home+"/.local/state/ollie",
		home+"/.cache/ollie/exec",
	)

	agentName := os.Getenv("OLLIE_AGENT")
	if agentName == "" {
		agentName = "default"
	}

	sessionID := newSessionID()
	var resumeMessages []backend.Message
	if *sessionFlag != "" {
		sessionPath := sessionsDir + "/" + *sessionFlag + ".json"
		data, readErr := os.ReadFile(sessionPath)
		if readErr != nil {
			fmt.Fprintln(os.Stderr, "--session:", readErr)
			os.Exit(1)
		}
		var ps agent.PersistedSession
		if jsonErr := json.Unmarshal(data, &ps); jsonErr != nil {
			fmt.Fprintln(os.Stderr, "--session: bad JSON:", jsonErr)
			os.Exit(1)
		}
		sessionID = ps.ID
		resumeMessages = ps.Messages
		if ps.Agent != "" && len(extraArgs) == 0 {
			agentName = ps.Agent
		}
	}
	if len(extraArgs) > 0 {
		agentName = extraArgs[0]
	}

	cfgPath := agentConfigPath(agentsDir, agentName)
	cfg, cfgErr := config.Load(cfgPath)
	env := agent.BuildAgentEnv(cfg, builtinExec)

	var initialSession *agent.Session
	if len(resumeMessages) > 0 {
		initialSession = agent.RestoreSession(resumeMessages, env.CtxOverhead)
	}

	agentCore := agent.NewAgentCore(agent.AgentCoreConfig{
		Backend:     be,
		AgentName:   agentName,
		AgentsDir:   agentsDir,
		SessionsDir: sessionsDir,
		SessionID:   sessionID,
		Session:     initialSession,
		Env:         env,
		BuiltinExec: builtinExec,
	})

	if cfgErr != nil {
		fmt.Fprintln(os.Stderr, "agent config:", cfgErr)
	}
	for _, msg := range env.Messages {
		fmt.Fprintln(os.Stderr, msg)
	}
	if len(resumeMessages) > 0 {
		fmt.Fprintf(os.Stderr, "session: %s (resumed)\n", sessionID)
	} else {
		fmt.Fprintf(os.Stderr, "session: %s\n", sessionID)
	}

	env.Hooks.Run(agent.HookAgentSpawn)

	if *promptFlag != "" {
		agentCore.Submit(context.Background(), *promptFlag, tui.MakeOutputFn(os.Stdout))
		return
	}

	tui.New(agentCore).Run(context.Background())
}


func newSessionID() string {
	b := make([]byte, 3)
	rand.Read(b) //nolint:errcheck
	return time.Now().Format("20060102-150405") + "-" + fmt.Sprintf("%06x", b)
}

func agentConfigPath(agentsDir, name string) string {
	p := agentsDir + "/" + name + ".json"
	if _, err := os.Stat(p); err == nil {
		return p
	}
	if name == "default" {
		home, _ := os.UserHomeDir()
		return home + "/.config/ollie/config.json"
	}
	return p
}
