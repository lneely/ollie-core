package store

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"

	"ollie/pkg/agent"
	"ollie/pkg/backend"
	"ollie/pkg/config"
	olog "ollie/pkg/log"
	"ollie/pkg/tools"
	"ollie/pkg/tools/execute"
)

const batchNewTemplate = "name=\ncwd=\nagent=\nbackend=\nmodel=\noutput=\nparallel=1\n---\nWrite your prompt here.\n"

// batchJob holds state for one ephemeral batch agent run.
type batchJob struct {
	mu        sync.RWMutex
	id        string
	spec      string // verbatim input to b/new
	prompt    string
	cwd       string
	agentName string
	backend   string
	model     string
	output    string
	state     string // "running" | "done" | "failed: ..."
	result    string
	usage     string
	ctxsz     string
	log       []byte
	logVers   uint32
	cancel    context.CancelFunc
	done      chan struct{} // closed when state reaches a terminal value
}

// appendLog appends data to the job's log and bumps the Qid version.
func (job *batchJob) appendLog(data []byte) {
	if len(data) == 0 {
		return
	}
	job.mu.Lock()
	job.log = append(job.log, data...)
	job.logVers++
	job.mu.Unlock()
}

func (job *batchJob) RunnableID() string { return job.id }

func (job *batchJob) Cancel() {
	job.mu.RLock()
	cancel := job.cancel
	job.mu.RUnlock()
	if cancel != nil {
		cancel()
	}
}

func (job *batchJob) Interrupt() { job.Cancel() }

func (job *batchJob) AppendLog(data []byte) { job.appendLog(data) }

func (job *batchJob) LogInfo() (int, uint32) {
	job.mu.RLock()
	defer job.mu.RUnlock()
	return len(job.log), job.logVers
}

// batchSpec is the parsed result of a b/new write.
type batchSpec struct {
	name      string
	cwd       string
	agentName string
	backend   string
	model     string
	output    string
	parallel  int
	prompt    string
}

// BatchStoreConfig holds the dependencies for a BatchStore.
type BatchStoreConfig struct {
	AgentsDir string
	Log       *olog.Logger
	Sink      *olog.Sink
	// NewCore, if non-nil, replaces the default backend.New + agent.NewAgentCore
	// path. It receives the job ID, agent name, and cwd, and returns a Core.
	NewCore func(jobID, agentName, cwd string) (agent.Core, error)
}

// BatchStore implements Store for batch job management.
type BatchStore struct {
	*storeConfig
	cfg  BatchStoreConfig
	mu   sync.RWMutex
	jobs map[string]*batchJob
}

func NewBatchStore(cfg BatchStoreConfig) *BatchStore {
	bs := &BatchStore{cfg: cfg, jobs: make(map[string]*batchJob)}
	bs.storeConfig = &storeConfig{
		StatFn:   bs.stat,
		ListFn:   bs.list,
		OpenFn:   bs.openEntry,
		DeleteFn: bs.del,
		CreateFn: func(string) error { return fmt.Errorf("create not supported for batch jobs") },
		RenameFn: func(string, string) error { return fmt.Errorf("rename not supported for batch jobs") },
	}
	return bs
}

// AddJob inserts a completed job into the store. This allows callers to
// populate the store without going through the full agent execution path.
func (s *BatchStore) AddJob(id, state, result, spec string) {
	done := make(chan struct{})
	close(done)
	s.mu.Lock()
	s.jobs[id] = &batchJob{
		id:     id,
		spec:   spec,
		state:  state,
		result: result,
		done:   done,
	}
	s.mu.Unlock()
}

func (s *BatchStore) list() ([]os.DirEntry, error) {
	entries := []os.DirEntry{
		FileEntry("new", 0666),
		FileEntry("idx", 0444),
	}
	s.mu.RLock()
	for id := range s.jobs {
		entries = append(entries, DirEntry(id, 0555))
	}
	s.mu.RUnlock()
	return entries, nil
}

func (s *BatchStore) stat(name string) (os.FileInfo, error) {
	switch name {
	case "new":
		return &SyntheticFileInfo{Name_: "new", Mode_: 0666, Size_: int64(len(batchNewTemplate))}, nil
	case "idx":
		return &SyntheticFileInfo{Name_: "idx", Mode_: 0444, Size_: int64(len(s.index()))}, nil
	}
	s.mu.RLock()
	_, ok := s.jobs[name]
	s.mu.RUnlock()
	if ok {
		return &SyntheticFileInfo{Name_: name, Mode_: 0555, IsDir_: true}, nil
	}
	return nil, fmt.Errorf("%s: not found", name)
}

func (s *BatchStore) openEntry(name string) (StoreEntry, error) {
	notBlocking := func(context.Context, string) ([]byte, error) {
		return nil, fmt.Errorf("blocking read not supported")
	}
	switch name {
	case "new":
		return &EntryConfig{
			StatFn:  func() (os.FileInfo, error) { return &SyntheticFileInfo{Name_: "new", Mode_: 0666, Size_: int64(len(batchNewTemplate))}, nil },
			ReadFn:  func() ([]byte, error) { return []byte(batchNewTemplate), nil },
			WriteFn: func(data []byte) error { return s.handleNewBatch(strings.TrimSpace(string(data))) },
			BlockingReadFn: notBlocking,
		}, nil
	case "idx":
		return &EntryConfig{
			StatFn:         func() (os.FileInfo, error) { return &SyntheticFileInfo{Name_: "idx", Mode_: 0444, Size_: int64(len(s.index()))}, nil },
			ReadFn:         func() ([]byte, error) { return s.index(), nil },
			WriteFn:        func([]byte) error { return fmt.Errorf("idx: read-only") },
			BlockingReadFn: notBlocking,
		}, nil
	}
	return nil, fmt.Errorf("%s: not a readable file", name)
}

func (s *BatchStore) del(name string) error {
	s.mu.Lock()
	job, ok := s.jobs[name]
	if ok {
		delete(s.jobs, name)
	}
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("batch job not found: %s", name)
	}
	job.Cancel()
	s.cfg.Log.Info("removed batch job %s", name)
	return nil
}

// Shutdown cancels all running jobs.
func (s *BatchStore) Shutdown() {
	s.mu.Lock()
	ids := make([]string, 0, len(s.jobs))
	for id := range s.jobs {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	for _, id := range ids {
		s.Delete(id) //nolint:errcheck
	}
}

// InterruptAll cancels all running jobs without removing them.
func (s *BatchStore) InterruptAll() {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, job := range s.jobs {
		job.Interrupt()
	}
}

// Job looks up a job by ID (nil if not found).
func (s *BatchStore) Job(id string) *batchJob {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.jobs[id]
}

// index returns the live b/idx content: id\tstate\tcwd\tagent per line.
func (s *BatchStore) index() []byte {
	var sb strings.Builder
	s.mu.RLock()
	defer s.mu.RUnlock()
	for id, job := range s.jobs {
		job.mu.RLock()
		fmt.Fprintf(&sb, "%s\t%s\t%s\t%s\n", id, job.state, job.cwd, job.agentName)
		job.mu.RUnlock()
	}
	return []byte(sb.String())
}

// handleNewBatch parses the spec, creates parallel batchJobs, and starts them.
func (s *BatchStore) handleNewBatch(input string) error {
	spec, err := ParseBatchSpec(input)
	if err != nil {
		return err
	}

	baseID := spec.name
	if baseID == "" {
		baseID = agent.NewSessionID()
	}

	for i := 0; i < spec.parallel; i++ {
		id := fmt.Sprintf("%s-%d", baseID, i)

		s.mu.Lock()
		if _, exists := s.jobs[id]; exists {
			s.mu.Unlock()
			return fmt.Errorf("batch job already exists: %s", id)
		}
		job := &batchJob{
			id:        id,
			spec:      input,
			prompt:    spec.prompt,
			cwd:       spec.cwd,
			agentName: spec.agentName,
			backend:   spec.backend,
			model:     spec.model,
			output:    spec.output,
			state:     "running",
			done:      make(chan struct{}),
		}
		s.jobs[id] = job
		s.mu.Unlock()

		ctx, cancel := context.WithCancel(context.Background())
		job.mu.Lock()
		job.cancel = cancel
		job.mu.Unlock()

		go s.runJob(ctx, job)
	}

	s.cfg.Log.Info("new batch base=%s parallel=%d", baseID, spec.parallel)
	return nil
}

// runJob executes one batch job and updates its state and result.
func (s *BatchStore) runJob(ctx context.Context, job *batchJob) {
	result, err := s.executeJob(ctx, job)
	job.mu.Lock()
	defer job.mu.Unlock()
	if err != nil {
		if ctx.Err() != nil {
			job.state = "failed: cancelled"
		} else {
			job.state = "failed: " + err.Error()
		}
		job.result = job.state
		s.cfg.Log.Info("batch job %s failed: %v", job.id, err)
		close(job.done)
		return
	}
	job.result = result
	job.state = "done"
	s.cfg.Log.Info("batch job %s done", job.id)
	close(job.done)
}

// executeJob creates an ephemeral agentCore, submits the prompt, and returns
// the assistant reply. The core is closed before returning.
func (s *BatchStore) executeJob(ctx context.Context, job *batchJob) (string, error) {
	job.mu.RLock()
	backendName := job.backend
	modelName := job.model
	agentName := job.agentName
	cwd := job.cwd
	prompt := job.prompt
	output := job.output
	jobID := job.id
	job.mu.RUnlock()

	var core agent.Core
	if s.cfg.NewCore != nil {
		var err error
		core, err = s.cfg.NewCore(jobID, agentName, cwd)
		if err != nil {
			return "", err
		}
	} else {
		be, err := backend.NewWithName(backendName)
		if err != nil {
			return "", fmt.Errorf("backend: %w", err)
		}
		if modelName != "" {
			be.SetModel(modelName)
		}

		cfg := LoadAgentConfig(s.cfg.AgentsDir, agentName, nil)

		newDisp := tools.NewDispatcherFunc(map[string]func() tools.Server{
			"execute": execute.Decl(cwd),
		})
		env := agent.BuildAgentEnv(cfg, newDisp(), cwd)

		core = agent.NewAgentCore(agent.AgentCoreConfig{
			Backend:       be,
			AgentName:     agentName,
			AgentsDir:     s.cfg.AgentsDir,
			SessionsDir:   "", // ephemeral: no persistence
			SessionID:     jobID,
			CWD:           cwd,
			Env:           env,
			NewDispatcher: newDisp,
			Log:           s.cfg.Sink.NewLogger("core"),
		})
	}
	defer core.Close()

	if output == "json" {
		prompt += "\n\nRespond with valid JSON only, no prose."
	}

	var replyBuf, errBuf strings.Builder
	assistantStarted := false
	core.Submit(ctx, prompt, func(ev agent.Event) {
		switch ev.Role {
		case "assistant":
			replyBuf.WriteString(ev.Content)
			if !assistantStarted {
				job.appendLog([]byte("assistant: "))
				assistantStarted = true
			}
		case "user", "call", "tool":
			if assistantStarted {
				job.appendLog([]byte("\n"))
				assistantStarted = false
			}
		case "error":
			errBuf.WriteString(ev.Content)
		}
		job.appendLog(FormatEvent(ev))
	})

	// Ensure log ends with newline (assistant content has no trailing \n).
	job.mu.Lock()
	if len(job.log) > 0 && job.log[len(job.log)-1] != '\n' {
		job.log = append(job.log, '\n')
		job.logVers++
	}
	job.mu.Unlock()

	usage := core.Usage()
	ctxsz := core.CtxSz()

	job.mu.Lock()
	job.usage = usage
	job.ctxsz = ctxsz
	job.mu.Unlock()

	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if errBuf.Len() > 0 {
		return "", fmt.Errorf("%s", strings.TrimSpace(errBuf.String()))
	}
	return replyBuf.String(), nil
}

// ParseBatchSpec splits input on the first \n---\n, parses key=value headers
// above it, and treats everything below as the prompt body.
func ParseBatchSpec(input string) (batchSpec, error) {
	spec := batchSpec{
		agentName: "default",
		parallel:  1,
	}

	const sep = "\n---\n"
	idx := strings.Index(input, sep)
	if idx < 0 {
		return spec, fmt.Errorf("missing --- delimiter between headers and prompt")
	}

	for _, line := range strings.Split(input[:idx], "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		switch k {
		case "name":
			spec.name = v
		case "cwd":
			spec.cwd = v
		case "agent":
			spec.agentName = v
		case "backend":
			spec.backend = v
		case "model":
			spec.model = v
		case "output":
			spec.output = v
		case "parallel":
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				spec.parallel = n
			}
		}
	}

	spec.prompt = strings.TrimSpace(input[idx+len(sep):])

	if spec.cwd == "" {
		return spec, fmt.Errorf("cwd is required")
	}
	if spec.prompt == "" {
		return spec, fmt.Errorf("prompt is required")
	}
	return spec, nil
}

// --- BatchJobStore ---

var batchJobFiles = []struct {
	name string
	mode os.FileMode
}{
	{"spec", 0444},
	{"state", 0444},
	{"statewait", 0444},
	{"result", 0444},
	{"usage", 0444},
	{"ctxsz", 0444},
	{"log", 0444},
}

// BatchJobStore provides Stat/List/Get for the files within a single batch job
// directory (/b/{id}/*). The file set is fixed.
type BatchJobStore struct {
	*storeConfig
	Runnable
	job *batchJob
}

func NewBatchJobStore(job *batchJob) *BatchJobStore {
	js := &BatchJobStore{Runnable: job, job: job}
	notSupported := func(string) error { return fmt.Errorf("not supported") }
	js.storeConfig = &storeConfig{
		StatFn:   js.stat,
		ListFn:   js.list,
		OpenFn:   js.open,
		DeleteFn: notSupported,
		CreateFn: notSupported,
		RenameFn: func(string, string) error { return fmt.Errorf("not supported") },
	}
	return js
}

// OpenStore returns a RunnableStore for the given batch job ID.
func (s *BatchStore) OpenStore(id string) (RunnableStore, error) {
	job := s.Job(id)
	if job == nil {
		return nil, fmt.Errorf("batch job not found: %s", id)
	}
	return NewBatchJobStore(job), nil
}

func (js *BatchJobStore) list() ([]os.DirEntry, error) {
	entries := make([]os.DirEntry, len(batchJobFiles))
	for i, f := range batchJobFiles {
		entries[i] = FileEntry(f.name, f.mode)
	}
	return entries, nil
}

func (js *BatchJobStore) stat(name string) (os.FileInfo, error) {
	for _, f := range batchJobFiles {
		if f.name == name {
			var size int64
			if name == "log" {
				js.job.mu.RLock()
				size = int64(len(js.job.log))
				js.job.mu.RUnlock()
			} else {
				size = int64(len(js.content(name)))
			}
			return &SyntheticFileInfo{Name_: name, Mode_: f.mode, Size_: size}, nil
		}
	}
	return nil, fmt.Errorf("%s: not found", name)
}

func (js *BatchJobStore) open(name string) (StoreEntry, error) {
	for _, f := range batchJobFiles {
		if f.name == name {
			return js.entryFor(name, f.mode), nil
		}
	}
	return nil, fmt.Errorf("%s: not found", name)
}

func (js *BatchJobStore) entryFor(name string, mode os.FileMode) StoreEntry {
	return &EntryConfig{
		StatFn: func() (os.FileInfo, error) {
			var size int64
			if name == "log" {
				js.job.mu.RLock()
				size = int64(len(js.job.log))
				js.job.mu.RUnlock()
			} else {
				size = int64(len(js.content(name)))
			}
			return &SyntheticFileInfo{Name_: name, Mode_: mode, Size_: size}, nil
		},
		ReadFn: func() ([]byte, error) {
			if name == "log" {
				js.job.mu.RLock()
				data := make([]byte, len(js.job.log))
				copy(data, js.job.log)
				js.job.mu.RUnlock()
				return data, nil
			}
			return []byte(js.content(name)), nil
		},
		WriteFn: func([]byte) error { return fmt.Errorf("%s: read-only", name) },
		BlockingReadFn: func(ctx context.Context, base string) ([]byte, error) {
			if name != "statewait" {
				return nil, fmt.Errorf("%s: not a wait file", name)
			}
			select {
			case <-ctx.Done():
				return nil, nil
			case <-js.job.done:
			}
			js.job.mu.RLock()
			state := js.job.state
			js.job.mu.RUnlock()
			return []byte(state + "\n"), nil
		},
	}
}

func (js *BatchJobStore) content(name string) string {
	js.job.mu.RLock()
	defer js.job.mu.RUnlock()
	switch name {
	case "spec":
		return js.job.spec
	case "state":
		return js.job.state + "\n"
	case "result":
		return js.job.result
	case "usage":
		return js.job.usage + "\n"
	case "ctxsz":
		return js.job.ctxsz + "\n"
	}
	return ""
}

// LoadAgentConfig resolves and loads the config for a named agent.
// Returns nil (not an error) if the config file does not exist;
// BuildAgentEnv handles nil configs. open defaults to os.Open if nil.
func LoadAgentConfig(agentsDir, name string, open func(string) (*os.File, error)) *config.Config {
	if open == nil {
		open = os.Open
	}
	f, err := open(agentsDir + "/" + name + ".json")
	if err != nil {
		return nil
	}
	defer f.Close()
	cfg, _ := config.Load(f)
	return cfg
}

// FormatEvent converts an agent Event to bytes for appending to a chat log.
func FormatEvent(ev agent.Event) []byte {
	switch ev.Role {
	case "user":
		return []byte("user: " + ev.Content + "\n")
	case "assistant":
		return []byte(ev.Content)
	case "call":
		args := squashWhitespace(ev.Content)
		return []byte("-> " + ev.Name + "(" + args + ")\n")
	case "tool":
		return []byte(strings.TrimRight(ev.Content, "\n") + "\n")
	case "retry":
		return []byte("retrying in " + ev.Content + "s...\n")
	case "error":
		return []byte("error: " + ev.Content + "\n")
	case "stalled":
		return []byte("agent stalled\n")
	case "info":
		return []byte(ev.Content)
	default:
		return nil
	}
}

func squashWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
