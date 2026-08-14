// Package store persists loom entities as plain files under a data dir:
//
//	data/agents/<name>.md          agent defs (frontmatter + system prompt body)
//	data/workflows/<id>.json       workflow defs
//	data/runs/<id>/run.json        run state (plan, node states, events)
//	data/runs/<id>/nodes/<node>.md full node outputs
//	data/runs/<id>/workspace/      working dir handed to executor agents
package store

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"loom/internal/model"
)

type Store struct {
	dir string
	mu  sync.Mutex
}

func New(dir string) (*Store, error) {
	for _, sub := range []string{"agents", "workflows", "runs"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return nil, err
		}
	}
	s := &Store{dir: dir}
	if err := s.migrateAgents(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) Dir() string { return s.dir }

var nameRe = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

func safeName(name string) error {
	if name == "" || !nameRe.MatchString(name) {
		return fmt.Errorf("invalid name %q: only [a-zA-Z0-9._-] allowed", name)
	}
	return nil
}

func newID(prefix string) string {
	b := make([]byte, 4)
	rand.Read(b)
	return fmt.Sprintf("%s_%s%s", prefix, time.Now().Format("20060102t150405"), hex.EncodeToString(b))
}

// NewTaskID mints an id for a dynamic-mode task. Task ids double as filenames
// for their transcripts, so they go through the same generator as runs.
func NewTaskID() string { return newID("task") }

// ---- agents ----
// Each agent owns a directory:
//
//	agents/<name>/agent.md              definition (frontmatter + system prompt)
//	agents/<name>/home/                 the agent's own persistent workspace (ACP session cwd)
//	agents/<name>/home/AGENTS.md        generated from the definition on save
//	agents/<name>/home/.claude/skills/  the agent's private skills

func (s *Store) agentDir(name string) string { return filepath.Join(s.dir, "agents", name) }

// AgentHome returns the absolute path of the agent's own workspace.
func (s *Store) AgentHome(name string) string {
	abs, err := filepath.Abs(filepath.Join(s.agentDir(name), "home"))
	if err != nil {
		return filepath.Join(s.agentDir(name), "home")
	}
	return abs
}

func (s *Store) SaveAgent(a *model.Agent) error {
	if err := safeName(a.Name); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	home := filepath.Join(s.agentDir(a.Name), "home")
	if err := os.MkdirAll(filepath.Join(home, ".claude", "skills"), 0o755); err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(s.agentDir(a.Name), "agent.md"), []byte(marshalAgent(a))); err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(home, "AGENTS.md"), []byte(agentsMD(a)))
}

func (s *Store) LoadAgent(name string) (*model.Agent, error) {
	if err := safeName(name); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(s.agentDir(name), "agent.md"))
	if err != nil {
		return nil, err
	}
	a := unmarshalAgent(name, string(data))
	a.Skills, _ = s.ListSkills(name)
	return a, nil
}

func (s *Store) ListAgents() ([]*model.Agent, error) {
	entries, err := os.ReadDir(filepath.Join(s.dir, "agents"))
	if err != nil {
		return nil, err
	}
	var out []*model.Agent
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		a, err := s.LoadAgent(e.Name())
		if err != nil {
			continue
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *Store) DeleteAgent(name string) error {
	if err := safeName(name); err != nil {
		return err
	}
	return os.RemoveAll(s.agentDir(name))
}

// migrateAgents converts legacy flat agents/<name>.md files into the
// per-agent directory layout.
func (s *Store) migrateAgents() error {
	dir := filepath.Join(s.dir, "agents")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".md")
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		if err := s.SaveAgent(unmarshalAgent(name, string(data))); err != nil {
			return err
		}
		os.Remove(filepath.Join(dir, e.Name()))
	}
	return nil
}

// ---- per-agent skills ----

func (s *Store) skillsDir(agent string) string {
	return filepath.Join(s.agentDir(agent), "home", ".claude", "skills")
}

func (s *Store) ListSkills(agent string) ([]string, error) {
	entries, err := os.ReadDir(s.skillsDir(agent))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			if _, err := os.Stat(filepath.Join(s.skillsDir(agent), e.Name(), "SKILL.md")); err == nil {
				out = append(out, e.Name())
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

func (s *Store) ReadSkill(agent, skill string) (string, error) {
	if err := safeName(agent); err != nil {
		return "", err
	}
	if err := safeName(skill); err != nil {
		return "", err
	}
	data, err := os.ReadFile(filepath.Join(s.skillsDir(agent), skill, "SKILL.md"))
	return string(data), err
}

func (s *Store) WriteSkill(agent, skill, content string) error {
	if err := safeName(agent); err != nil {
		return err
	}
	if err := safeName(skill); err != nil {
		return err
	}
	dir := filepath.Join(s.skillsDir(agent), skill)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(dir, "SKILL.md"), []byte(content))
}

func (s *Store) DeleteSkill(agent, skill string) error {
	if err := safeName(agent); err != nil {
		return err
	}
	if err := safeName(skill); err != nil {
		return err
	}
	return os.RemoveAll(filepath.Join(s.skillsDir(agent), skill))
}

// ---- workflows ----

func (s *Store) SaveWorkflow(w *model.Workflow) error {
	if w.ID == "" {
		w.ID = newID("wf")
		w.CreatedAt = time.Now()
	}
	if err := safeName(w.ID); err != nil {
		return err
	}
	w.UpdatedAt = time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	return writeJSONAtomic(filepath.Join(s.dir, "workflows", w.ID+".json"), w)
}

func (s *Store) LoadWorkflow(id string) (*model.Workflow, error) {
	if err := safeName(id); err != nil {
		return nil, err
	}
	var w model.Workflow
	if err := readJSON(filepath.Join(s.dir, "workflows", id+".json"), &w); err != nil {
		return nil, err
	}
	return &w, nil
}

func (s *Store) ListWorkflows() ([]*model.Workflow, error) {
	entries, err := os.ReadDir(filepath.Join(s.dir, "workflows"))
	if err != nil {
		return nil, err
	}
	var out []*model.Workflow
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		var w model.Workflow
		if err := readJSON(filepath.Join(s.dir, "workflows", e.Name()), &w); err != nil {
			continue
		}
		out = append(out, &w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *Store) DeleteWorkflow(id string) error {
	if err := safeName(id); err != nil {
		return err
	}
	return os.Remove(filepath.Join(s.dir, "workflows", id+".json"))
}

// ---- runs ----

func (s *Store) NewRun(wf *model.Workflow, goal, backend string) (*model.Run, error) {
	r := &model.Run{
		ID:           newID("run"),
		WorkflowID:   wf.ID,
		WorkflowName: wf.Name,
		Goal:         goal,
		Backend:      backend,
		Mode:         wf.EffectiveMode(),
		Status:       model.RunPlanning,
		Nodes:        map[string]*model.NodeState{},
		CreatedAt:    time.Now(),
	}
	if r.Mode == model.ModeDynamic {
		r.Tasks = map[string]*model.Task{}
		// A dynamic run has nothing to plan: the coordinator starts immediately.
		r.Status = model.RunRunning
	}
	if err := os.MkdirAll(s.RunWorkspace(r.ID), 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(s.runDir(r.ID), "nodes"), 0o755); err != nil {
		return nil, err
	}
	return r, s.SaveRun(r)
}

func (s *Store) runDir(id string) string { return filepath.Join(s.dir, "runs", id) }

// RunWorkspace is the run's shared exchange directory (absolute): upstream
// artifacts land here and every node's deliverables must be written here.
func (s *Store) RunWorkspace(id string) string {
	p := filepath.Join(s.runDir(id), "workspace")
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

func (s *Store) NodeOutputPath(runID, nodeID string) string {
	return filepath.Join(s.runDir(runID), "nodes", nodeID+".md")
}

// SaveUpload stores one user-attached file (e.g. a pasted chat image) under
// the run's uploads directory. Uploads live beside run.json, not in the
// exchange directory — they are conversation input, not a deliverable, and
// the exchange directory can move when the run gets named.
func (s *Store) SaveUpload(runID, name string, data []byte) error {
	if err := safeName(runID); err != nil {
		return err
	}
	if err := safeName(name); err != nil {
		return err
	}
	dir := filepath.Join(s.runDir(runID), "uploads")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, name), data, 0o644)
}

// ReadUpload returns one stored upload's bytes.
func (s *Store) ReadUpload(runID, name string) ([]byte, error) {
	if err := safeName(runID); err != nil {
		return nil, err
	}
	if err := safeName(name); err != nil {
		return nil, err
	}
	return os.ReadFile(filepath.Join(s.runDir(runID), "uploads", name))
}

func (s *Store) SaveRun(r *model.Run) error {
	if err := safeName(r.ID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return writeJSONAtomic(filepath.Join(s.runDir(r.ID), "run.json"), r)
}

func (s *Store) LoadRun(id string) (*model.Run, error) {
	if err := safeName(id); err != nil {
		return nil, err
	}
	var r model.Run
	if err := readJSON(filepath.Join(s.runDir(id), "run.json"), &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// DeleteRun removes a run — its record, transcripts and workspace — for good.
// The cost ledger keeps its entries: spend happened whether or not the session
// is kept.
func (s *Store) DeleteRun(id string) error {
	if err := safeName(id); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := os.Stat(filepath.Join(s.runDir(id), "run.json")); err != nil {
		return fmt.Errorf("run %s not found", id)
	}
	return os.RemoveAll(s.runDir(id))
}

func (s *Store) ListRuns() ([]*model.Run, error) {
	entries, err := os.ReadDir(filepath.Join(s.dir, "runs"))
	if err != nil {
		return nil, err
	}
	var out []*model.Run
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		var r model.Run
		if err := readJSON(filepath.Join(s.runDir(e.Name()), "run.json"), &r); err != nil {
			continue
		}
		out = append(out, &r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// ---- amendments ----
// A coordinator's proposed revision of an agent's system prompt. Stored as
// data until a human decides; approval is the ONLY path that touches agent.md.

func (s *Store) amendmentPath(id string) string {
	return filepath.Join(s.dir, "amendments", id+".json")
}

// SaveAmendment persists a new proposal. IDs and timestamps are assigned here;
// status always starts pending regardless of what the caller set.
func (s *Store) SaveAmendment(a *model.Amendment) error {
	if a.ID == "" {
		a.ID = newID("amend")
	}
	if err := safeName(a.ID); err != nil {
		return err
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now()
	}
	a.Status = model.AmendmentPending
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Join(s.dir, "amendments"), 0o755); err != nil {
		return err
	}
	return writeJSONAtomic(s.amendmentPath(a.ID), a)
}

func (s *Store) LoadAmendment(id string) (*model.Amendment, error) {
	if err := safeName(id); err != nil {
		return nil, err
	}
	var a model.Amendment
	if err := readJSON(s.amendmentPath(id), &a); err != nil {
		return nil, err
	}
	return &a, nil
}

// ListAmendments returns every amendment, newest first.
func (s *Store) ListAmendments() ([]*model.Amendment, error) {
	entries, err := os.ReadDir(filepath.Join(s.dir, "amendments"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*model.Amendment
	for _, e := range entries {
		var a model.Amendment
		if err := readJSON(filepath.Join(s.dir, "amendments", e.Name()), &a); err != nil {
			continue
		}
		out = append(out, &a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// DecideAmendment settles a pending proposal. Approving applies the proposed
// prompt to the agent — through SaveAgent, so AGENTS.md regenerates like any
// human edit — but refuses a STALE proposal: one whose snapshot no longer
// matches the agent's current prompt was reasoned against text that no longer
// exists, and silently applying it would overwrite a human's newer edit.
func (s *Store) DecideAmendment(id string, approve bool) (*model.Amendment, error) {
	a, err := s.LoadAmendment(id)
	if err != nil {
		return nil, err
	}
	if a.Status != model.AmendmentPending {
		return nil, fmt.Errorf("amendment %s is already %s", id, a.Status)
	}
	if approve {
		agent, err := s.LoadAgent(a.Agent)
		if err != nil {
			return nil, fmt.Errorf("agent %q no longer exists: %w", a.Agent, err)
		}
		if agent.SystemPrompt != a.Current {
			return nil, fmt.Errorf("agent %q's prompt changed after this proposal was made; reject it and ask for a fresh one", a.Agent)
		}
		agent.SystemPrompt = a.Proposed
		if err := s.SaveAgent(agent); err != nil {
			return nil, err
		}
		a.Status = model.AmendmentApproved
	} else {
		a.Status = model.AmendmentRejected
	}
	a.DecidedAt = time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := writeJSONAtomic(s.amendmentPath(a.ID), a); err != nil {
		return nil, err
	}
	return a, nil
}

// ---- lessons ----
// Behavior rules distilled from run retrospectives, workflow-scoped. Stored as
// data until the user decides; only approved lessons are injected into future
// runs — proposal never equals effect (same posture as amendments).

func (s *Store) lessonPath(id string) string {
	return filepath.Join(s.dir, "lessons", id+".json")
}

// SaveLesson persists one lesson. IDs and timestamps are assigned here; an
// empty status starts pending (a coordinator proposal awaiting the user). A
// caller may preset StatusApproved for user-authored rules — the human writing
// the rule IS the approval. A proposal with Replaces must name approved rules
// of the same workflow; their texts are snapshotted here so approval can
// refuse a stale swap. Empty text is only legal as a retirement (Replaces
// non-empty).
func (s *Store) SaveLesson(l *model.Lesson) error {
	l.Text = strings.TrimSpace(l.Text)
	if l.Text == "" && len(l.Replaces) == 0 {
		return fmt.Errorf("lesson text is empty")
	}
	if l.WorkflowID == "" {
		return fmt.Errorf("lesson has no workflow")
	}
	l.ReplacedTexts = nil
	for _, id := range l.Replaces {
		target, err := s.LoadLesson(id)
		if err != nil {
			return fmt.Errorf("rule %s it claims to replace does not exist", id)
		}
		if target.WorkflowID != l.WorkflowID {
			return fmt.Errorf("rule %s belongs to another workflow", id)
		}
		if target.Status != model.AmendmentApproved {
			return fmt.Errorf("rule %s is %s — only approved standing rules can be replaced", id, target.Status)
		}
		l.ReplacedTexts = append(l.ReplacedTexts, target.Text)
	}
	if l.ID == "" {
		l.ID = newID("lesson")
	}
	if err := safeName(l.ID); err != nil {
		return err
	}
	if l.CreatedAt.IsZero() {
		l.CreatedAt = time.Now()
	}
	if l.Status == "" {
		l.Status = model.AmendmentPending
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Join(s.dir, "lessons"), 0o755); err != nil {
		return err
	}
	return writeJSONAtomic(s.lessonPath(l.ID), l)
}

func (s *Store) LoadLesson(id string) (*model.Lesson, error) {
	if err := safeName(id); err != nil {
		return nil, err
	}
	var l model.Lesson
	if err := readJSON(s.lessonPath(id), &l); err != nil {
		return nil, err
	}
	return &l, nil
}

// ListLessons returns every lesson, newest first.
func (s *Store) ListLessons() ([]*model.Lesson, error) {
	entries, err := os.ReadDir(filepath.Join(s.dir, "lessons"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*model.Lesson
	for _, e := range entries {
		var l model.Lesson
		if err := readJSON(filepath.Join(s.dir, "lessons", e.Name()), &l); err != nil {
			continue
		}
		out = append(out, &l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// DecideLesson settles a pending proposal: approve puts it into the injection
// set, reject archives it. Deciding twice is refused, same as amendments.
// Approving a superseding proposal swaps it for its targets — but refuses a
// STALE one: a target the user edited (or already replaced) since the
// proposal was made carries decisions the proposal never saw, and silently
// discarding them would let an agent overrule the human.
func (s *Store) DecideLesson(id string, approve bool) (*model.Lesson, error) {
	l, err := s.LoadLesson(id)
	if err != nil {
		return nil, err
	}
	if l.Status != model.AmendmentPending {
		return nil, fmt.Errorf("lesson %s is already %s", id, l.Status)
	}
	if approve {
		for i, tid := range l.Replaces {
			target, err := s.LoadLesson(tid)
			if err != nil {
				return nil, fmt.Errorf("rule %s it replaces no longer exists; reject this proposal and ask for a fresh one", tid)
			}
			if i < len(l.ReplacedTexts) && target.Text != l.ReplacedTexts[i] {
				return nil, fmt.Errorf("rule %s changed after this proposal was made; reject it and ask for a fresh one", tid)
			}
		}
		l.Status = model.AmendmentApproved
	} else {
		l.Status = model.AmendmentRejected
	}
	l.DecidedAt = time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := writeJSONAtomic(s.lessonPath(l.ID), l); err != nil {
		return nil, err
	}
	if approve {
		// The swap: superseded rules leave the set. Their texts live on in
		// this record's ReplacedTexts for the audit trail.
		for _, tid := range l.Replaces {
			os.Remove(s.lessonPath(tid))
		}
	}
	return l, nil
}

// UpdateLessonText rewrites a lesson's text in place — the human revising the
// record owns it; status and provenance stay as they are.
func (s *Store) UpdateLessonText(id, text string) (*model.Lesson, error) {
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("lesson text is empty")
	}
	l, err := s.LoadLesson(id)
	if err != nil {
		return nil, err
	}
	l.Text = text
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := writeJSONAtomic(s.lessonPath(l.ID), l); err != nil {
		return nil, err
	}
	return l, nil
}

// DeleteLesson removes a lesson entirely (an approved rule the user retires,
// or a rejected one they want gone from the archive).
func (s *Store) DeleteLesson(id string) error {
	if err := safeName(id); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(s.lessonPath(id)); os.IsNotExist(err) {
		return fmt.Errorf("lesson %s not found", id)
	} else if err != nil {
		return err
	}
	return nil
}

func (s *Store) WriteNodeOutput(runID, nodeID, content string) error {
	if err := safeName(runID); err != nil {
		return err
	}
	if err := safeName(nodeID); err != nil {
		return err
	}
	return writeFileAtomic(s.NodeOutputPath(runID, nodeID), []byte(content))
}

// ---- cost ledger ----
//
// data/costs.jsonl is the append-only source of truth for cost roll-ups. It
// exists so per-agent and per-workflow reporting doesn't require rescanning
// every run directory, and so history survives runs being deleted.

// CostEntry is one priced unit of work: a static node, a dynamic task, or a
// planner/coordinator call.
type CostEntry struct {
	Ts         time.Time        `json:"ts"`
	WorkflowID string           `json:"workflow_id"`
	RunID      string           `json:"run_id"`
	Unit       string           `json:"unit"` // node | task | planner | coordinator
	UnitID     string           `json:"unit_id"`
	Agent      string           `json:"agent"`
	Model      string           `json:"model"`
	Usage      model.TokenUsage `json:"usage"`
	CostUSD    float64          `json:"cost_usd"`
}

func (s *Store) costPath() string { return filepath.Join(s.dir, "costs.jsonl") }

// AppendCost records one priced unit. Zero-cost, zero-usage entries are
// dropped: mock runs would otherwise flood the ledger with noise.
func (s *Store) AppendCost(e CostEntry) error {
	if e.CostUSD == 0 && e.Usage.Empty() {
		return nil
	}
	if e.Ts.IsZero() {
		e.Ts = time.Now()
	}
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.OpenFile(s.costPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(data, '\n'))
	return err
}

// CostBucket is one row of a cost roll-up.
type CostBucket struct {
	Key     string           `json:"key"`
	Label   string           `json:"label,omitempty"`
	CostUSD float64          `json:"cost_usd"`
	Usage   model.TokenUsage `json:"usage"`
	Units   int              `json:"units"`
}

// CostSummary groups the ledger by workflow, agent and model in one pass.
type CostSummary struct {
	Workflows []CostBucket `json:"workflows"`
	Agents    []CostBucket `json:"agents"`
	Models    []CostBucket `json:"models"`
	TotalUSD  float64      `json:"total_usd"`
	Entries   int          `json:"entries"`
}

// CostSummaryData reads the ledger and aggregates it. A malformed line is
// skipped rather than failing the report — the ledger is an audit convenience,
// not a transactional store.
func (s *Store) CostSummaryData() (*CostSummary, error) {
	f, err := os.Open(s.costPath())
	if err != nil {
		if os.IsNotExist(err) {
			return &CostSummary{Workflows: []CostBucket{}, Agents: []CostBucket{}, Models: []CostBucket{}}, nil
		}
		return nil, err
	}
	defer f.Close()

	byWF, byAgent, byModel := map[string]*CostBucket{}, map[string]*CostBucket{}, map[string]*CostBucket{}
	add := func(m map[string]*CostBucket, key string, e CostEntry) {
		if key == "" {
			key = "(unknown)"
		}
		b := m[key]
		if b == nil {
			b = &CostBucket{Key: key}
			m[key] = b
		}
		b.CostUSD += e.CostUSD
		b.Usage.Add(e.Usage)
		b.Units++
	}
	sum := &CostSummary{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var e CostEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			continue
		}
		add(byWF, e.WorkflowID, e)
		add(byAgent, e.Agent, e)
		add(byModel, e.Model, e)
		sum.TotalUSD += e.CostUSD
		sum.Entries++
	}
	flatten := func(m map[string]*CostBucket) []CostBucket {
		out := make([]CostBucket, 0, len(m))
		for _, b := range m {
			out = append(out, *b)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].CostUSD > out[j].CostUSD })
		return out
	}
	sum.Workflows, sum.Agents, sum.Models = flatten(byWF), flatten(byAgent), flatten(byModel)
	return sum, nil
}

// ---- helpers ----

func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

func writeJSONAtomic(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, data)
}

func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
