package store_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ollie/pkg/agent"
	"ollie/pkg/backend"
	olog "ollie/pkg/log"
	"ollie/pkg/store"
)

// --- stub agent.Core ---

type stubCore struct {
	state     string
	running   bool
	backend   string
	model     string
	agentName string
	cwd       string
	usage     string
	ctxsz     string
	models    string
	sysprompt string
	reply     string
	params    backend.GenerationParams
	closed    bool
}

func (c *stubCore) Submit(context.Context, string, agent.EventHandler)          {}
func (c *stubCore) Interrupt(error) bool                                        { return c.running }
func (c *stubCore) Inject(string)                                               {}
func (c *stubCore) Queue(string)                                                {}
func (c *stubCore) PopQueue() (string, bool)                                    { return "", false }
func (c *stubCore) IsRunning() bool                                             { return c.running }
func (c *stubCore) State() string                                               { return c.state }
func (c *stubCore) Reply() string                                               { return c.reply }
func (c *stubCore) AgentName() string                                           { return c.agentName }
func (c *stubCore) BackendName() string                                         { return c.backend }
func (c *stubCore) ModelName() string                                           { return c.model }
func (c *stubCore) CtxSz() string                                               { return c.ctxsz }
func (c *stubCore) Usage() string                                               { return c.usage }
func (c *stubCore) ListModels() string                                          { return c.models }
func (c *stubCore) CWD() string                                                 { return c.cwd }
func (c *stubCore) SetCWD(dir string) error                                     { c.cwd = dir; return nil }
func (c *stubCore) SetSessionID(string) error                                   { return nil }
func (c *stubCore) SystemPrompt() string                                        { return c.sysprompt }
func (c *stubCore) GenerationParams() backend.GenerationParams                  { return c.params }
func (c *stubCore) SetGenerationParams(p backend.GenerationParams) error        { c.params = p; return nil }
func (c *stubCore) SetEnv(string, string)                                       {}
func (c *stubCore) WaitChange(ctx context.Context, _, _ string) (string, bool)  { <-ctx.Done(); return "", false }
func (c *stubCore) Close()                                                      { c.closed = true }

// --- helpers ---

func testSink() *olog.Sink {
	return olog.NewSink(io.Discard, io.Discard, olog.LevelError)
}

func testSession(id string) *store.Session {
	ctx, cancel := context.WithCancel(context.Background())
	return store.NewSession(id, &stubCore{state: "idle", backend: "stub", model: "m", agentName: "default", cwd: "/tmp"}, ctx, cancel)
}

func seedSkill(t *testing.T, base, name string) {
	t.Helper()
	d := filepath.Join(base, name)
	os.MkdirAll(d, 0755)
	os.WriteFile(filepath.Join(d, "SKILL.md"), []byte("---\ndescription: test\n---\n"), 0644)
}

func newSessionStore(t *testing.T) *store.SessionStore {
	t.Helper()
	sink := testSink()
	return store.NewSessionStore(store.SessionStoreConfig{
		Log:      sink.NewLogger("test"),
		Sink:     sink,
		ReadFile: func(string) ([]byte, error) { return []byte("#!/bin/sh\n"), nil },
		MkdirAll: func(string, os.FileMode) error { return nil },
	})
}

func newBatchStore(t *testing.T) *store.BatchStore {
	t.Helper()
	sink := testSink()
	return store.NewBatchStore(store.BatchStoreConfig{
		Log:  sink.NewLogger("test"),
		Sink: sink,
	})
}

// storeRead is a test helper: Open + Read.
func storeRead(t *testing.T, s store.Store, name string) []byte {
	t.Helper()
	e, err := s.Open(name)
	if err != nil {
		t.Fatalf("Open(%q): %v", name, err)
	}
	data, err := e.Read()
	if err != nil {
		t.Fatalf("Read(%q): %v", name, err)
	}
	return data
}

// storeWrite is a test helper: Open + Write.
func storeWrite(t *testing.T, s store.Store, name string, data []byte) {
	t.Helper()
	e, err := s.Open(name)
	if err != nil {
		t.Fatalf("Open(%q): %v", name, err)
	}
	if err := e.Write(data); err != nil {
		t.Fatalf("Write(%q): %v", name, err)
	}
}

// --- contract checks ---

func checkReadableContract(t *testing.T, s store.Store, name string) {
	t.Helper()
	fi, err := s.Stat(name)
	if err != nil {
		t.Fatalf("Stat(%q): %v", name, err)
	}
	if fi.Name() != name {
		t.Errorf("Stat(%q).Name() = %q", name, fi.Name())
	}
	if _, err := s.Stat("__nonexistent__"); err == nil {
		t.Error("Stat(nonexistent) should error")
	}
	entries, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	found := false
	for _, e := range entries {
		if e.Name() == name {
			found = true
		}
	}
	if !found {
		t.Errorf("List() missing %q", name)
	}
	e, err := s.Open(name)
	if err != nil {
		t.Fatalf("Open(%q): %v", name, err)
	}
	if _, err := e.Read(); err != nil {
		t.Fatalf("Read(%q): %v", name, err)
	}
}

func checkReadWriteContract(t *testing.T, s store.Store, name string) {
	t.Helper()
	want := []byte("hello")
	e, err := s.Open(name)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := e.Write(want); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := e.Read()
	if err != nil {
		t.Fatalf("Read after Write: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("Read = %q; want %q", got, want)
	}
	want2 := []byte("world")
	if err := e.Write(want2); err != nil {
		t.Fatalf("Write overwrite: %v", err)
	}
	got2, err := e.Read()
	if err != nil {
		t.Fatalf("Read after overwrite: %v", err)
	}
	if string(got2) != string(want2) {
		t.Errorf("Read after overwrite = %q; want %q", got2, want2)
	}
	checkReadableContract(t, s, name)
}

func checkStoreContract(t *testing.T, s store.Store, name string) {
	t.Helper()
	if err := s.Create(name); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s.Stat(name); err != nil {
		t.Fatalf("Stat after Create: %v", err)
	}
	renamed := name + "-renamed"
	if err := s.Rename(name, renamed); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if _, err := s.Stat(name); err == nil {
		t.Error("Stat(old) should error after Rename")
	}
	if _, err := s.Stat(renamed); err != nil {
		t.Errorf("Stat(new) after Rename: %v", err)
	}
	s.Delete(renamed) //nolint:errcheck
	if err := s.Create(name); err != nil {
		t.Fatalf("Create after rename cleanup: %v", err)
	}
	if err := s.Delete(name); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Stat(name); err == nil {
		t.Error("Stat after Delete should error")
	}
	if err := s.Delete("__nonexistent__"); err == nil {
		t.Error("Delete(nonexistent) should error")
	}
	checkReadWriteContract(t, s, name)
}

// ===== FlatDir =====

func TestFlatDirContract(t *testing.T) {
	checkStoreContract(t, store.NewFlatDir(t.TempDir(), 0644), "test-file")
}

func TestFlatDirCreateMkdirError(t *testing.T) {
	fd := store.NewFlatDir("/nonexistent/path", 0644)
	if err := fd.Create("f"); err == nil {
		t.Error("Create should fail when dir doesn't exist")
	}
}

func TestFlatDirPutMkdirError(t *testing.T) {
	fd := store.NewFlatDir("/nonexistent/path", 0644)
	e, err := fd.Open("f")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := e.Write([]byte("x")); err == nil {
		t.Error("Write should fail when dir doesn't exist")
	}
}

// ===== SkillStore =====

func TestSkillStoreContract(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OLLIE_SKILLS_PATH", dir)
	seedSkill(t, dir, "test-skill")

	s := store.NewSkillStore()

	// Rename (needs seeded skill with valid front matter)
	seedSkill(t, dir, "rename-src")
	if err := s.Rename("rename-src.md", "rename-dst.md"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if _, err := s.Stat("rename-src.md"); err == nil {
		t.Error("Stat(old) should error after Rename")
	}
	if _, err := s.Stat("rename-dst.md"); err != nil {
		t.Errorf("Stat(new) after Rename: %v", err)
	}
	s.Delete("rename-dst.md") //nolint:errcheck

	// Delete
	seedSkill(t, dir, "del-test")
	if err := s.Delete("del-test.md"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Stat("del-test.md"); err == nil {
		t.Error("Stat after Delete should error")
	}
	if err := s.Delete("__nonexistent__.md"); err == nil {
		t.Error("Delete(nonexistent) should error")
	}

	// Write/Read round-trip (needs valid front matter)
	content := []byte("---\ndescription: rw test\n---\nbody\n")
	if err := s.Create("rw-skill.md"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	storeWrite(t, s, "rw-skill.md", content)
	got := storeRead(t, s, "rw-skill.md")
	if string(got) != string(content) {
		t.Errorf("Read = %q; want %q", got, content)
	}

	// Readable
	checkReadableContract(t, s, "test-skill.md")

	// idx
	idx := storeRead(t, s, "idx")
	if len(idx) == 0 {
		t.Error("idx should be non-empty with seeded skills")
	}

	// Rename nonexistent
	if err := s.Rename("__nope__.md", "x.md"); err == nil {
		t.Error("Rename(nonexistent) should error")
	}

	// Create calls through injected mkdirAll + writeFile
	var createdDir, createdFile string
	s2 := store.NewSkillStoreWith(store.SkillStoreConfig{
		Dirs:      []string{dir},
		MkdirAll:  func(p string, _ os.FileMode) error { createdDir = p; return nil },
		WriteFile: func(p string, _ []byte, _ os.FileMode) error { createdFile = p; return nil },
	})
	if err := s2.Create("newskill.md"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !strings.HasSuffix(createdDir, "newskill") {
		t.Errorf("mkdirAll path = %q; want suffix newskill", createdDir)
	}
	if !strings.HasSuffix(createdFile, "SKILL.md") {
		t.Errorf("writeFile path = %q; want suffix SKILL.md", createdFile)
	}

	// Create mkdirAll error
	s3 := store.NewSkillStoreWith(store.SkillStoreConfig{
		Dirs:     []string{dir},
		MkdirAll: func(string, os.FileMode) error { return fmt.Errorf("denied") },
	})
	if err := s3.Create("x.md"); err == nil {
		t.Error("Create should fail when mkdirAll errors")
	}
}

// ===== Session =====

func TestSessionAppendLog(t *testing.T) {
	sess := testSession("s1")
	defer sess.Cancel()

	sess.AppendLog([]byte("hello"))
	sess.AppendLog(nil) // no-op
	sess.AppendLog([]byte(" world"))

	l, v := sess.LogInfo()
	if l != 11 {
		t.Errorf("ChatInfo length = %d; want 11", l)
	}
	if v != 2 {
		t.Errorf("ChatInfo vers = %d; want 2", v)
	}
}

func TestSessionStoreFileMode(t *testing.T) {
	if m, ok := store.SessionStoreFileMode("new"); !ok || m != 0666 {
		t.Errorf("SessionStoreFileMode(new) = %o, %v", m, ok)
	}
	if _, ok := store.SessionStoreFileMode("bogus"); ok {
		t.Error("SessionStoreFileMode(bogus) should be false")
	}
}

// ===== SessionStore =====

func TestSessionStoreReadableContract(t *testing.T) {
	checkReadableContract(t, newSessionStore(t), "new")
}

func TestSessionStoreGetIdx(t *testing.T) {
	s := newSessionStore(t)
	sess := testSession("abc")
	defer sess.Cancel()
	s.AddSession(sess)

	data := storeRead(t, s, "idx")
	if !strings.Contains(string(data), "abc") {
		t.Errorf("idx = %q; want to contain abc", data)
	}
}

func TestSessionStoreGetScript(t *testing.T) {
	s := newSessionStore(t)
	data := storeRead(t, s, "ls")
	if string(data) != "#!/bin/sh\n" {
		t.Errorf("Read(ls) = %q", data)
	}
}

func TestSessionStoreStatSession(t *testing.T) {
	s := newSessionStore(t)
	sess := testSession("s1")
	defer sess.Cancel()
	s.AddSession(sess)

	fi, err := s.Stat("s1")
	if err != nil {
		t.Fatalf("Stat(s1): %v", err)
	}
	if !fi.IsDir() {
		t.Error("session stat should be dir")
	}
}

func TestSessionStoreListIncludesSessions(t *testing.T) {
	s := newSessionStore(t)
	sess := testSession("s1")
	defer sess.Cancel()
	s.AddSession(sess)

	entries, _ := s.List()
	found := false
	for _, e := range entries {
		if e.Name() == "s1" {
			found = true
		}
	}
	if !found {
		t.Error("List() missing session s1")
	}
}

func TestSessionStoreWriteNotWritable(t *testing.T) {
	s := newSessionStore(t)
	e, err := s.Open("idx")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := e.Write(nil); err == nil {
		t.Error("Write(idx) should error")
	}
}

func TestSessionStoreCreateErrors(t *testing.T) {
	s := newSessionStore(t)
	if err := s.Create("x"); err == nil {
		t.Error("Create should always error")
	}
}

func TestSessionStoreDeleteAndKill(t *testing.T) {
	s := newSessionStore(t)
	sess := testSession("s1")
	s.AddSession(sess)

	if err := s.Delete("s1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if s.Session("s1") != nil {
		t.Error("session should be gone after Delete")
	}
	core := sess.Core.(*stubCore)
	if !core.closed {
		t.Error("core should be closed after Delete")
	}
	if err := s.Delete("nope"); err == nil {
		t.Error("Delete(nonexistent) should error")
	}
}

func TestSessionStoreSession(t *testing.T) {
	s := newSessionStore(t)
	if s.Session("nope") != nil {
		t.Error("Session(nonexistent) should be nil")
	}
	sess := testSession("s1")
	defer sess.Cancel()
	s.AddSession(sess)
	if s.Session("s1") == nil {
		t.Error("Session(s1) should not be nil")
	}
}

func TestSessionStoreInterruptAll(t *testing.T) {
	s := newSessionStore(t)
	sess := testSession("s1")
	defer sess.Cancel()
	sess.Core.(*stubCore).running = true
	s.AddSession(sess)

	s.InterruptAll() // should not panic
}

func TestSessionStoreShutdown(t *testing.T) {
	s := newSessionStore(t)
	sess := testSession("s1")
	s.AddSession(sess)

	s.Shutdown()
	if s.Session("s1") != nil {
		t.Error("session should be gone after Shutdown")
	}
}

func TestSessionStoreRename(t *testing.T) {
	s := newSessionStore(t)
	sess := testSession("old")
	defer sess.Cancel()
	s.AddSession(sess)

	if err := s.Rename("old", "new"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if s.Session("old") != nil {
		t.Error("old session should be gone")
	}
	if s.Session("new") == nil {
		t.Error("new session should exist")
	}
}

func TestSessionStoreRenameErrors(t *testing.T) {
	s := newSessionStore(t)
	// nonexistent
	if err := s.Rename("nope", "x"); err == nil {
		t.Error("Rename(nonexistent) should error")
	}
	// duplicate
	s.AddSession(testSession("a"))
	s.AddSession(testSession("b"))
	if err := s.Rename("a", "b"); err == nil {
		t.Error("Rename to existing should error")
	}
	// running
	sess := testSession("r")
	sess.Core.(*stubCore).running = true
	s.AddSession(sess)
	if err := s.Rename("r", "r2"); err == nil {
		t.Error("Rename while running should error")
	}
}

// ===== SessionFileStore =====

func TestSessionFileStoreReadableContract(t *testing.T) {
	sess := testSession("s1")
	defer sess.Cancel()
	sink := testSink()
	sf := store.NewSessionFileStore(sess, sink.NewLogger("test"),
		func() {}, func(string) error { return nil }, func([]byte) error { return nil })
	checkReadableContract(t, sf, "state")
}

func TestSessionFileStoreList(t *testing.T) {
	sess := testSession("s1")
	defer sess.Cancel()
	sink := testSink()
	sf := store.NewSessionFileStore(sess, sink.NewLogger("test"),
		func() {}, func(string) error { return nil }, func([]byte) error { return nil })

	entries, err := sf.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != len(store.SessionFileList) {
		t.Errorf("List() returned %d entries; want %d", len(entries), len(store.SessionFileList))
	}
}

func TestSessionFileStoreStatChat(t *testing.T) {
	sess := testSession("s1")
	defer sess.Cancel()
	sess.AppendLog([]byte("hello"))
	sink := testSink()
	sf := store.NewSessionFileStore(sess, sink.NewLogger("test"),
		func() {}, func(string) error { return nil }, func([]byte) error { return nil })

	fi, err := sf.Stat("chat")
	if err != nil {
		t.Fatalf("Stat(chat): %v", err)
	}
	if fi.Size() != 5 {
		t.Errorf("Stat(chat).Size() = %d; want 5", fi.Size())
	}
}

func TestSessionFileStoreGetChat(t *testing.T) {
	sess := testSession("s1")
	defer sess.Cancel()
	sess.AppendLog([]byte("hello"))
	sink := testSink()
	sf := store.NewSessionFileStore(sess, sink.NewLogger("test"),
		func() {}, func(string) error { return nil }, func([]byte) error { return nil })

	data := storeRead(t, sf, "chat")
	if string(data) != "hello" {
		t.Errorf("Read(chat) = %q; want hello", data)
	}
}

func TestSessionFileStoreGetContent(t *testing.T) {
	sess := testSession("s1")
	defer sess.Cancel()
	sink := testSink()
	sf := store.NewSessionFileStore(sess, sink.NewLogger("test"),
		func() {}, func(string) error { return nil }, func([]byte) error { return nil })

	for _, name := range []string{"backend", "agent", "model", "state", "cwd", "offset", "params"} {
		if _, err := sf.Open(name); err != nil {
			t.Errorf("Get(%q): %v", name, err)
		}
	}
}

func TestSessionFileStorePutCwd(t *testing.T) {
	sess := testSession("s1")
	defer sess.Cancel()
	sink := testSink()
	sf := store.NewSessionFileStore(sess, sink.NewLogger("test"),
		func() {}, func(string) error { return nil }, func([]byte) error { return nil })

	storeWrite(t, sf, "cwd", []byte("/new/path"))
	core := sess.Core.(*stubCore)
	if core.cwd != "/new/path" {
		t.Errorf("cwd = %q; want /new/path", core.cwd)
	}
}

func TestSessionFileStorePutEmpty(t *testing.T) {
	sess := testSession("s1")
	defer sess.Cancel()
	sink := testSink()
	sf := store.NewSessionFileStore(sess, sink.NewLogger("test"),
		func() {}, func(string) error { return nil }, func([]byte) error { return nil })

	// Empty write is a no-op
	e, err := sf.Open("cwd")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := e.Write([]byte("")); err != nil {
		t.Fatalf("Write(cwd, empty): %v", err)
	}
}

// ===== FormatParams / ParseParams =====

func TestFormatParseParamsRoundTrip(t *testing.T) {
	temp := 0.7
	p := backend.GenerationParams{MaxTokens: 1024, Temperature: &temp}
	s := store.FormatParams(p)
	got, err := store.ParseParams(s, backend.GenerationParams{})
	if err != nil {
		t.Fatalf("ParseParams: %v", err)
	}
	if got.MaxTokens != 1024 {
		t.Errorf("MaxTokens = %d; want 1024", got.MaxTokens)
	}
	if got.Temperature == nil || *got.Temperature != 0.7 {
		t.Errorf("Temperature = %v; want 0.7", got.Temperature)
	}
}

func TestParseParamsErrors(t *testing.T) {
	if _, err := store.ParseParams("maxTokens=abc", backend.GenerationParams{}); err == nil {
		t.Error("expected error for invalid maxTokens")
	}
	if _, err := store.ParseParams("temperature=abc", backend.GenerationParams{}); err == nil {
		t.Error("expected error for invalid temperature")
	}
	if _, err := store.ParseParams("frequencyPenalty=abc", backend.GenerationParams{}); err == nil {
		t.Error("expected error for invalid frequencyPenalty")
	}
	if _, err := store.ParseParams("presencePenalty=abc", backend.GenerationParams{}); err == nil {
		t.Error("expected error for invalid presencePenalty")
	}
}

func TestParseParamsClearOptional(t *testing.T) {
	temp := 0.5
	p := backend.GenerationParams{Temperature: &temp}
	got, err := store.ParseParams("temperature=\nmaxTokens=", p)
	if err != nil {
		t.Fatalf("ParseParams: %v", err)
	}
	if got.Temperature != nil {
		t.Error("Temperature should be nil after clearing")
	}
}

// ===== BatchStore =====

func TestBatchStoreReadableContract(t *testing.T) {
	checkReadableContract(t, newBatchStore(t), "new")
}

func TestBatchStoreStatJob(t *testing.T) {
	s := newBatchStore(t)
	s.AddJob("j1", "done", "result", "spec")

	fi, err := s.Stat("j1")
	if err != nil {
		t.Fatalf("Stat(j1): %v", err)
	}
	if !fi.IsDir() {
		t.Error("job stat should be dir")
	}
}

func TestBatchStoreListIncludesJobs(t *testing.T) {
	s := newBatchStore(t)
	s.AddJob("j1", "done", "result", "spec")

	entries, _ := s.List()
	found := false
	for _, e := range entries {
		if e.Name() == "j1" {
			found = true
		}
	}
	if !found {
		t.Error("List() missing job j1")
	}
}

func TestBatchStoreGetIdx(t *testing.T) {
	s := newBatchStore(t)
	s.AddJob("j1", "done", "result", "spec")

	data := storeRead(t, s, "idx")
	if !strings.Contains(string(data), "j1") {
		t.Errorf("idx = %q; want to contain j1", data)
	}
}

func TestBatchStoreWriteNotWritable(t *testing.T) {
	s := newBatchStore(t)
	e, err := s.Open("idx")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := e.Write(nil); err == nil {
		t.Error("Write(idx) should error")
	}
}

func TestBatchStoreCreateErrors(t *testing.T) {
	s := newBatchStore(t)
	if err := s.Create("x"); err == nil {
		t.Error("Create should always error")
	}
}

func TestBatchStoreRenameErrors(t *testing.T) {
	s := newBatchStore(t)
	if err := s.Rename("a", "b"); err == nil {
		t.Error("Rename should always error")
	}
}

func TestBatchStoreDelete(t *testing.T) {
	s := newBatchStore(t)
	s.AddJob("j1", "done", "result", "spec")

	if err := s.Delete("j1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if s.Job("j1") != nil {
		t.Error("job should be gone after Delete")
	}
	if err := s.Delete("nope"); err == nil {
		t.Error("Delete(nonexistent) should error")
	}
}

func TestBatchStoreJob(t *testing.T) {
	s := newBatchStore(t)
	if s.Job("nope") != nil {
		t.Error("Job(nonexistent) should be nil")
	}
	s.AddJob("j1", "done", "result", "spec")
	if s.Job("j1") == nil {
		t.Error("Job(j1) should not be nil")
	}
}

func TestBatchStoreShutdown(t *testing.T) {
	s := newBatchStore(t)
	s.AddJob("j1", "done", "result", "spec")
	s.Shutdown()
	if s.Job("j1") != nil {
		t.Error("job should be gone after Shutdown")
	}
}

// ===== BatchJobStore =====

func TestBatchJobStoreReadableContract(t *testing.T) {
	s := newBatchStore(t)
	s.AddJob("j1", "done", "the result", "the spec")
	js, err := s.OpenStore("j1")
	if err != nil {
		t.Fatal("JobStore(j1) not found")
	}
	checkReadableContract(t, js, "state")
}

func TestBatchJobStoreContent(t *testing.T) {
	s := newBatchStore(t)
	s.AddJob("j1", "done", "the result", "the spec")
	js, _ := s.OpenStore("j1")

	for _, tc := range []struct {
		name, want string
	}{
		{"spec", "the spec"},
		{"state", "done\n"},
		{"result", "the result"},
	} {
		data := storeRead(t, js, tc.name)
		if string(data) != tc.want {
			t.Errorf("Read(%q) = %q; want %q", tc.name, data, tc.want)
		}
	}
}

func TestBatchJobStoreWait(t *testing.T) {
	s := newBatchStore(t)
	s.AddJob("j1", "done", "", "")
	js, _ := s.OpenStore("j1")

	// Job is already done, so BlockingRead returns immediately
	e, err := js.Open("statewait")
	if err != nil {
		t.Fatalf("Open(statewait): %v", err)
	}
	data, err := e.BlockingRead(context.Background(), "")
	if err != nil {
		t.Fatalf("BlockingRead: %v", err)
	}
	if !strings.Contains(string(data), "done") {
		t.Errorf("BlockingRead = %q; want to contain done", data)
	}

	// Non-wait file
	e2, err := js.Open("spec")
	if err != nil {
		t.Fatalf("Open(spec): %v", err)
	}
	if _, err := e2.BlockingRead(context.Background(), ""); err == nil {
		t.Error("BlockingRead(spec) should error")
	}
}

func TestBatchJobStoreLogInfo(t *testing.T) {
	s := newBatchStore(t)
	s.AddJob("j1", "done", "", "")
	js, _ := s.OpenStore("j1")

	l, v := js.LogInfo()
	if l != 0 || v != 0 {
		t.Errorf("LogInfo = (%d, %d); want (0, 0)", l, v)
	}
}

func TestBatchJobStoreNotFound(t *testing.T) {
	s := newBatchStore(t)
	if _, err := s.OpenStore("nope"); err == nil {
		t.Error("JobStore(nonexistent) should be false")
	}
}

// ===== ParseBatchSpec =====

func TestParseBatchSpec(t *testing.T) {
	input := "name=test\ncwd=/tmp\nagent=default\n---\ndo something"
	if _, err := store.ParseBatchSpec(input); err != nil {
		t.Fatalf("ParseBatchSpec: %v", err)
	}
}

func TestParseBatchSpecErrors(t *testing.T) {
	// missing delimiter
	if _, err := store.ParseBatchSpec("no delimiter"); err == nil {
		t.Error("expected error for missing ---")
	}
	// missing cwd
	if _, err := store.ParseBatchSpec("name=x\n---\nprompt"); err == nil {
		t.Error("expected error for missing cwd")
	}
	// missing prompt
	if _, err := store.ParseBatchSpec("cwd=/tmp\n---\n"); err == nil {
		t.Error("expected error for missing prompt")
	}
}

// ===== LoadAgentConfig =====

func TestLoadAgentConfig(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "test.json"), []byte(`{}`), 0644)

	cfg := store.LoadAgentConfig(dir, "test", nil)
	if cfg == nil {
		t.Error("LoadAgentConfig should return non-nil for existing file")
	}

	cfg = store.LoadAgentConfig(dir, "nonexistent", nil)
	if cfg != nil {
		t.Error("LoadAgentConfig should return nil for missing file")
	}
}

// ===== FormatEvent =====

func TestFormatEvent(t *testing.T) {
	for _, tc := range []struct {
		ev   agent.Event
		want string
	}{
		{agent.Event{Role: "user", Content: "hi"}, "user: hi\n"},
		{agent.Event{Role: "assistant", Content: "hello"}, "hello"},
		{agent.Event{Role: "error", Content: "oops"}, "error: oops\n"},
		{agent.Event{Role: "call", Name: "fn", Content: "args"}, "-> fn(args)\n"},
		{agent.Event{Role: "tool", Content: "result\n"}, "result\n"},
		{agent.Event{Role: "retry", Content: "5"}, "retrying in 5s...\n"},
		{agent.Event{Role: "stalled"}, "agent stalled\n"},
		{agent.Event{Role: "unknown"}, ""},
	} {
		got := string(store.FormatEvent(tc.ev))
		if got != tc.want {
			t.Errorf("FormatEvent(%q) = %q; want %q", tc.ev.Role, got, tc.want)
		}
	}
}

// ===== SyntheticFileInfo / SyntheticEntry / FileEntry / DirEntry =====

func TestSyntheticFileInfo(t *testing.T) {
	fi := &store.SyntheticFileInfo{Name_: "f", Mode_: 0644, Size_: 42, IsDir_: false}
	if fi.Name() != "f" || fi.Size() != 42 || fi.Mode() != 0644 || fi.IsDir() || fi.Sys() != nil {
		t.Error("SyntheticFileInfo field mismatch")
	}
	if !fi.ModTime().IsZero() {
		t.Error("ModTime should be zero")
	}
}

func TestFileEntryDirEntry(t *testing.T) {
	fe := store.FileEntry("f", 0644)
	if fe.Name() != "f" || fe.IsDir() || fe.Type() != 0 {
		t.Error("FileEntry mismatch")
	}
	de := store.DirEntry("d", 0755)
	if de.Name() != "d" || !de.IsDir() || de.Type() != os.ModeDir {
		t.Error("DirEntry mismatch")
	}
	// Info()
	info, err := de.Info()
	if err != nil || info.Name() != "d" || !info.IsDir() {
		t.Error("DirEntry.Info() mismatch")
	}
}
