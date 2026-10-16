package learn

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/anchoo2kewl/go-learn/agent"
	"github.com/anchoo2kewl/go-learn/voice"
)

// Engine runs Audio Courses: it plans courses, keeps a buffer of voiced segments ahead of the
// listener, and answers what the owner types or says. Mount Handler() in the host app and call Run.
type Engine struct {
	st     *store
	log    zerolog.Logger
	opt    options
	wake   chan struct{}
	mu     sync.Mutex
	voices []voice.Voice
}

type options struct {
	identity     func(*http.Request) (string, bool)
	runner       agent.Runner
	speaker      voice.Speaker
	transcriber  voice.Transcriber
	dataDir      string
	ffmpeg       string
	bufferAhead  time.Duration
	agentWorkers int
	fallback     map[string]string
	logger       zerolog.Logger
	title        string
	researchDirs []string
}

// ResearchTools is what a research-enabled turn may use: reading and searching files, git history,
// and GitHub lookups through gh. Nothing that writes, deletes, or reaches the open web.
var ResearchTools = []string{
	"Read", "Grep", "Glob",
	"Bash(ls:*)", "Bash(wc:*)",
	"Bash(git log:*)", "Bash(git show:*)", "Bash(git diff:*)", "Bash(git grep:*)", "Bash(git blame:*)", "Bash(git ls-files:*)",
	"Bash(gh repo view:*)", "Bash(gh repo list:*)", "Bash(gh pr view:*)", "Bash(gh pr list:*)", "Bash(gh pr diff:*)",
	"Bash(gh issue view:*)", "Bash(gh issue list:*)", "Bash(gh search:*)", "Bash(gh release view:*)", "Bash(gh release list:*)",
}

// WithResearch lets courses that opt in read these folders and GitHub, read-only (see ResearchTools).
func WithResearch(dirs []string) Option { return func(o *options) { o.researchDirs = dirs } }

// Option configures an Engine.
type Option func(*options)

// WithIdentity tells the engine who is making a request.
func WithIdentity(f func(*http.Request) (string, bool)) Option {
	return func(o *options) { o.identity = f }
}

// WithRunner sets how turns run (default: the logged-in CLIs, agent.CLI).
func WithRunner(r agent.Runner) Option { return func(o *options) { o.runner = r } }

// WithVoice sets the speech sidecar client (speaker and transcriber).
func WithVoice(s voice.Speaker, t voice.Transcriber) Option {
	return func(o *options) { o.speaker, o.transcriber = s, t }
}

// WithDataDir sets where audio and scratch folders live.
func WithDataDir(dir string) Option { return func(o *options) { o.dataDir = dir } }

// WithBufferAhead sets how much voiced audio to keep ready ahead of the listener (default 45 minutes).
func WithBufferAhead(d time.Duration) Option { return func(o *options) { o.bufferAhead = d } }

// WithLogger sets the logger.
func WithLogger(l zerolog.Logger) Option { return func(o *options) { o.logger = l } }

// WithTitle sets the page title shown in the player (default "Learn").
func WithTitle(t string) Option { return func(o *options) { o.title = t } }

// New makes an Engine on db (SQLite dialect) and creates its learn_* tables.
func New(ctx context.Context, db *sql.DB, opts ...Option) (*Engine, error) {
	o := options{
		runner:       agent.CLI{},
		ffmpeg:       "ffmpeg",
		bufferAhead:  45 * time.Minute,
		agentWorkers: 2,
		fallback:     map[string]string{"claude": "codex", "codex": "claude", "gemini": "claude"},
		logger:       zerolog.Nop(),
		title:        "Learn",
		identity:     func(*http.Request) (string, bool) { return "", false },
	}
	for _, f := range opts {
		f(&o)
	}
	if o.dataDir == "" {
		return nil, errors.New("learn: WithDataDir is required")
	}
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		o.ffmpeg = p
	} else if _, err := os.Stat("/opt/homebrew/bin/ffmpeg"); err == nil {
		o.ffmpeg = "/opt/homebrew/bin/ffmpeg"
	}
	e := &Engine{st: &store{db: db}, log: o.logger, opt: o, wake: make(chan struct{}, 1)}
	if err := e.st.migrate(ctx); err != nil {
		return nil, err
	}
	return e, nil
}

func (e *Engine) poke() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// Run starts the buffer controller and the workers; it returns when ctx ends.
func (e *Engine) Run(ctx context.Context) {
	var wg sync.WaitGroup
	agentKinds := []string{"answer", "grade", "quiz", "calibrate", "plan", "extend", "script"}
	for i := 0; i < e.opt.agentWorkers; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); e.worker(ctx, agentKinds) }()
	}
	if e.opt.speaker != nil {
		wg.Add(1)
		go func() { defer wg.Done(); e.worker(ctx, []string{"voice"}) }()
	}
	wg.Add(1)
	go func() { defer wg.Done(); e.controller(ctx) }()
	wg.Wait()
}

func (e *Engine) worker(ctx context.Context, kinds []string) {
	health, _ := e.opt.speaker.(interface{ Healthy(context.Context) bool })
	for ctx.Err() == nil {
		// Voice jobs wait for the sidecar (it takes a few seconds to start) rather than burn attempts.
		if kinds[0] == "voice" && health != nil && !health.Healthy(ctx) {
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
			continue
		}
		j, err := e.st.claim(ctx, kinds)
		if err != nil {
			e.log.Error().Err(err).Msg("claim job")
		}
		if j == nil {
			select {
			case <-ctx.Done():
				return
			case <-e.wake:
			case <-time.After(2 * time.Second):
			}
			continue
		}
		log := e.log.With().Int64("job", j.ID).Str("kind", j.Kind).Str("course", j.CourseID).Int("attempt", j.Attempts).Logger()
		log.Info().Msg("job start")
		t0 := time.Now()
		err = e.do(ctx, j)
		retry := err != nil && j.Attempts < 3 && ctx.Err() == nil
		e.st.finishJob(context.WithoutCancel(ctx), j.ID, err, retry)
		if err != nil {
			log.Error().Err(err).Bool("retry", retry).Dur("took", time.Since(t0)).Msg("job failed")
			if !retry {
				e.jobGaveUp(context.WithoutCancel(ctx), j, err)
			} else {
				time.Sleep(5 * time.Second)
			}
		} else {
			log.Info().Dur("took", time.Since(t0)).Msg("job done")
		}
		e.poke()
	}
}

func (e *Engine) do(ctx context.Context, j *Job) error {
	switch j.Kind {
	case "calibrate":
		return e.doCalibrate(ctx, j)
	case "plan":
		return e.doPlan(ctx, j)
	case "extend":
		return e.doExtend(ctx, j)
	case "script":
		return e.doScript(ctx, j)
	case "voice":
		return e.doVoice(ctx, j)
	case "answer":
		return e.doAnswer(ctx, j)
	case "quiz":
		return e.doQuiz(ctx, j)
	case "grade":
		return e.doGrade(ctx, j)
	}
	return fmt.Errorf("unknown job kind %q", j.Kind)
}

// jobGaveUp records a final failure where the owner will see it.
func (e *Engine) jobGaveUp(ctx context.Context, j *Job, err error) {
	switch j.Kind {
	case "calibrate", "plan":
		_ = e.st.updateCourse(ctx, j.CourseID, map[string]any{"status": StatusError, "error": err.Error()})
	case "voice":
		var p struct {
			SegmentID string `json:"segment_id"`
		}
		_ = json.Unmarshal(j.Payload, &p)
		_ = e.st.setSegmentStatus(ctx, p.SegmentID, SegFailed)
	case "answer":
		var p struct {
			InteractionID string `json:"interaction_id"`
		}
		_ = json.Unmarshal(j.Payload, &p)
		_ = e.st.finishInteraction(ctx, p.InteractionID, "failed", "", "", "")
	case "quiz", "grade":
		var p struct {
			QuizID string `json:"quiz_id"`
		}
		_ = json.Unmarshal(j.Payload, &p)
		if q, err2 := e.st.quiz(ctx, p.QuizID); err2 == nil {
			q.Status, q.Error = "failed", err.Error()
			_ = e.st.updateQuiz(ctx, q)
		}
	case "script", "extend":
		_ = e.st.updateCourse(ctx, j.CourseID, map[string]any{"error": "generation is failing: " + err.Error()})
	}
}

// controller keeps each active course's buffer topped up.
func (e *Engine) controller(ctx context.Context) {
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	prune := time.NewTicker(6 * time.Hour)
	defer prune.Stop()
	for {
		e.topUp(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-prune.C:
			e.st.pruneJobs(ctx)
			e.pruneClips()
		}
	}
}

func (e *Engine) topUp(ctx context.Context) {
	courses, err := e.st.activeCourses(ctx)
	if err != nil {
		e.log.Error().Err(err).Msg("list active courses")
		return
	}
	for _, c := range courses {
		ord, updated := e.st.ownerPosition(ctx, c)
		last := c.CreatedAt
		if updated != "" {
			last = updated
		}
		if t, err := time.Parse(time.RFC3339Nano, last); err == nil && time.Since(t) > 7*24*time.Hour {
			continue // idle for a week: don't spend the subscription
		}
		ready, pending := e.st.secondsAhead(ctx, c.ID, ord)
		// Don't let scripts run far ahead of the voice: queued text is wasted if the learner steers.
		if pending >= 3*180 || ready+pending >= e.opt.bufferAhead.Seconds() || e.st.hasJob(ctx, c.ID, "script", "extend", "plan") {
			continue
		}
		if err := e.st.enqueue(ctx, c.ID, "script", 30, nil); err == nil {
			e.poke()
		}
	}
}

// ---- LLM turns ----

// turn runs prompt(resumed) on the course's provider, resuming session when given, and fails over to
// the fallback provider with a fresh session. It returns the result and whether the session is new.
func (e *Engine) turn(ctx context.Context, c *Course, name, sessionProv, session string, prompt func(resumed bool) string, out any) (agent.Result, error) {
	dir := filepath.Join(e.opt.dataDir, "scratch", c.ID)
	research := c.Research && len(e.opt.researchDirs) > 0 && !strings.HasSuffix(name, "-calibrate") && !strings.HasSuffix(name, "-pron")
	try := func(provider, model, resume string) (agent.Result, error) {
		t := agent.Turn{Provider: provider, Model: model, Dir: dir, Prompt: prompt(resume != ""), Resume: resume, Name: name}
		if research {
			t.AddDirs, t.AllowTools = e.opt.researchDirs, ResearchTools
			t.Prompt = researchNote(e.opt.researchDirs) + "\n\n" + t.Prompt
		}
		res, err := e.opt.runner.Run(ctx, t)
		if err != nil {
			return res, err
		}
		if err := agent.ExtractJSON(res.Text, out); err != nil {
			return res, fmt.Errorf("%s reply wasn't JSON: %w", provider, err)
		}
		return res, nil
	}
	resume := ""
	if sessionProv == c.Provider {
		resume = session
	}
	res, err := try(c.Provider, c.Model, resume)
	if err != nil && resume != "" {
		e.log.Warn().Err(err).Str("course", c.ID).Msg("resumed turn failed; trying a fresh session")
		res, err = try(c.Provider, c.Model, "")
	}
	if err != nil {
		fb := e.opt.fallback[c.Provider]
		if fb == "" || fb == c.Provider {
			return res, err
		}
		e.log.Warn().Err(err).Str("course", c.ID).Str("fallback", fb).Msg("turn failed; failing over")
		res2, err2 := try(fb, "", "")
		if err2 != nil {
			return res2, fmt.Errorf("%v; fallback %s: %w", err, fb, err2)
		}
		return res2, nil
	}
	return res, nil
}

func llmLabel(r agent.Result) string {
	if r.Model != "" {
		return r.Provider + "/" + r.Model
	}
	return r.Provider
}

// ---- jobs ----

func (e *Engine) doCalibrate(ctx context.Context, j *Job) error {
	c, err := e.st.course(ctx, j.CourseID)
	if err != nil {
		return err
	}
	var out struct {
		Title     string        `json:"title"`
		Questions []CalQuestion `json:"questions"`
	}
	if _, err := e.turn(ctx, c, "learn-"+c.ID+"-calibrate", "", "", func(bool) string { return calibratePrompt(c) }, &out); err != nil {
		return err
	}
	if len(out.Questions) == 0 {
		return errors.New("no calibration questions came back")
	}
	set := map[string]any{"status": StatusAwaiting, "calibration": Calibration{Questions: out.Questions}, "error": ""}
	if c.Title == "" && out.Title != "" {
		set["title"] = out.Title
	}
	return e.st.updateCourse(ctx, c.ID, set)
}

type planModule struct {
	Title    string `json:"title"`
	Summary  string `json:"summary"`
	Concepts []struct {
		Title      string  `json:"title"`
		Objectives string  `json:"objectives"`
		Mastery    float64 `json:"mastery"`
	} `json:"concepts"`
}

func (e *Engine) addModules(ctx context.Context, c *Course, mods []planModule, startOrd float64) error {
	for i, m := range mods {
		mo := startOrd + float64(i)*100
		mod := &Node{CourseID: c.ID, Kind: "module", Ord: mo, Title: m.Title, Summary: m.Summary}
		if err := e.st.insertNode(ctx, mod); err != nil {
			return err
		}
		for k, cc := range m.Concepts {
			n := &Node{CourseID: c.ID, ParentID: mod.ID, Kind: "concept", Ord: mo + float64(k+1), Title: cc.Title, Summary: cc.Objectives, Mastery: clamp01(cc.Mastery)}
			if err := e.st.insertNode(ctx, n); err != nil {
				return err
			}
		}
	}
	return nil
}

func (e *Engine) doPlan(ctx context.Context, j *Job) error {
	c, err := e.st.course(ctx, j.CourseID)
	if err != nil {
		return err
	}
	var out struct {
		Title   string          `json:"title"`
		Modules []planModule    `json:"modules"`
		Profile json.RawMessage `json:"profile"`
	}
	if _, err := e.turn(ctx, c, "learn-"+c.ID+"-plan", "", "", func(bool) string { return planPrompt(c) }, &out); err != nil {
		return err
	}
	if len(out.Modules) == 0 {
		return errors.New("the plan had no modules")
	}
	if nodes, _ := e.st.nodes(ctx, c.ID); len(nodes) > 0 {
		return e.st.updateCourse(ctx, c.ID, map[string]any{"status": StatusActive}) // a retry after a partial write
	}
	if err := e.addModules(ctx, c, out.Modules, 100); err != nil {
		return err
	}
	set := map[string]any{"status": StatusActive, "error": ""}
	if len(out.Profile) > 2 {
		set["profile"] = []byte(out.Profile)
	}
	if out.Title != "" {
		set["title"] = out.Title
	}
	if err := e.st.updateCourse(ctx, c.ID, set); err != nil {
		return err
	}
	return e.st.enqueue(ctx, c.ID, "script", 30, nil)
}

func (e *Engine) doExtend(ctx context.Context, j *Job) error {
	c, err := e.st.course(ctx, j.CourseID)
	if err != nil {
		return err
	}
	nodes, err := e.st.nodes(ctx, c.ID)
	if err != nil {
		return err
	}
	var done []string
	maxOrd := 0.0
	for _, n := range nodes {
		if n.Kind == "module" {
			done = append(done, n.Title+": "+n.Summary)
		}
		if n.Ord > maxOrd {
			maxOrd = n.Ord
		}
	}
	var out struct {
		Modules []planModule `json:"modules"`
	}
	if _, err := e.turn(ctx, c, "learn-"+c.ID+"-extend", "", "", func(bool) string { return extendPrompt(c, done) }, &out); err != nil {
		return err
	}
	if len(out.Modules) == 0 {
		return errors.New("the extension had no modules")
	}
	start := (float64(int(maxOrd/100)) + 1) * 100
	if err := e.addModules(ctx, c, out.Modules, start); err != nil {
		return err
	}
	return e.st.enqueue(ctx, c.ID, "script", 30, nil)
}

type nextOut struct {
	Move       string `json:"move"`
	Focus      string `json:"focus"`
	NewConcept struct {
		Title      string `json:"title"`
		Objectives string `json:"objectives"`
		Module     string `json:"module"`
	} `json:"new_concept"`
	Title          string            `json:"title"`
	Script         string            `json:"script"`
	Summary        string            `json:"summary"`
	Checkin        string            `json:"checkin"`
	Pronunciations map[string]string `json:"pronunciations"`
}

// sessionTurns: a course keeps one rolling teacher session, refreshed this often so it never bloats.
const sessionTurns = 15

// doScript decides and writes the next segment. Nothing is ever "finished": the teacher sees the whole
// map (mastery, how often and how recently each idea came up) and what the learner has said, and
// picks what to do next — continue, revisit from a new angle, go deeper, review, connect or branch out.
func (e *Engine) doScript(ctx context.Context, j *Job) error {
	c, err := e.st.course(ctx, j.CourseID)
	if err != nil {
		return err
	}
	nodes, err := e.st.nodes(ctx, c.ID)
	if err != nil {
		return err
	}
	x := nextContext{Steer: c.Steer}
	for _, n := range nodes {
		if n.Kind == "concept" {
			x.Concepts = append(x.Concepts, n)
		} else {
			x.Modules = append(x.Modules, n)
		}
	}
	if len(x.Concepts) == 0 {
		return errors.New("no map yet")
	}
	recent, _ := e.st.recentSegments(ctx, c.ID, 1e18, 10)
	x.Recent = recent
	asked, _ := e.st.interactions(ctx, c.ID, 6)
	x.Asked = asked
	last, err := e.st.lastSegment(ctx, c.ID)
	if err != nil && !errors.Is(err, errNotFound) {
		return err
	}
	ord := 1.0
	if last != nil {
		ord = float64(int(last.Ord)) + 1
	}
	x.SegmentsSoFar = int(ord) - 1

	// Default suggestion: stay with the current idea for a few segments, then the least-covered idea in map order.
	var lastLesson *Segment
	for k := len(recent) - 1; k >= 0; k-- {
		if recent[k].Kind == "lesson" {
			lastLesson = recent[k]
			break
		}
	}
	run := 0
	for k := len(recent) - 1; k >= 0 && lastLesson != nil; k-- {
		if recent[k].Kind == "lesson" {
			if recent[k].NodeID != lastLesson.NodeID {
				break
			}
			run++
		}
	}
	if lastLesson != nil && run < 3 {
		for _, n := range x.Concepts {
			if n.ID == lastLesson.NodeID {
				x.Suggestion = n
			}
		}
	}
	if x.Suggestion == nil {
		for _, n := range x.Concepts {
			if x.Suggestion == nil || n.Covered < x.Suggestion.Covered {
				x.Suggestion = n
			}
		}
	}
	sinceCheckin := 0
	for k := len(recent) - 1; k >= 0; k-- {
		if recent[k].Checkin != "" {
			break
		}
		if recent[k].Kind == "lesson" {
			sinceCheckin++
		}
	}
	x.CheckinDue = sinceCheckin >= 4

	// Only what's relevant goes into the turn: the ideas likely in play, and notes and past questions on them.
	cands := []*Node{x.Suggestion}
	if lastLesson != nil {
		for _, n := range x.Concepts {
			if n.ID == lastLesson.NodeID {
				cands = append(cands, n)
			}
		}
	}
	cands = append(cands, mentioned(x.Concepts, c.Steer)...)
	cands = append(cands, weakest(x.Concepts, 3)...)
	e.workingSet(ctx, c, &x, cands)

	session := c.SessionID
	if c.SessionN >= sessionTurns {
		session = "" // refresh: a new session gets the full brief, map and recent history
	}
	var out nextOut
	res, err := e.turn(ctx, c, "learn-"+c.ID, c.SessionProv, session, func(resumed bool) string { return nextPrompt(c, x, resumed) }, &out)
	if err != nil {
		return err
	}
	if strings.TrimSpace(out.Script) == "" {
		return errors.New("empty script")
	}

	// Where on the map is this? An existing concept, or a new one the teacher branched into.
	var focus *Node
	for _, n := range x.Concepts {
		if strings.EqualFold(strings.TrimSpace(n.Title), strings.TrimSpace(out.Focus)) {
			focus = n
		}
	}
	if focus == nil && strings.TrimSpace(out.NewConcept.Title) != "" {
		focus, err = e.addConcept(ctx, c, x.Modules, x.Concepts, out.NewConcept.Title, out.NewConcept.Objectives, out.NewConcept.Module)
		if err != nil {
			return err
		}
	}
	if focus == nil {
		focus = x.Suggestion
	}
	focus.Covered++
	focus.LastCoveredAt = now()
	if focus.Status == "pending" {
		focus.Status = "active"
	}
	_ = e.st.updateNode(ctx, focus)

	g := &Segment{
		CourseID: c.ID, Ord: ord, Kind: "lesson", NodeID: focus.ID, Title: out.Title, Script: cleanScript(out.Script),
		Summary: out.Summary, Checkin: strings.TrimSpace(out.Checkin), Status: SegScripted, LLM: llmLabel(res),
	}
	g.Speech = applyPronunciations(g.Script, out.Pronunciations)
	g.Pron, _ = json.Marshal(out.Pronunciations)
	if err := e.st.insertSegment(ctx, g); err != nil {
		return err
	}
	set := map[string]any{"error": "", "steer": ""}
	if res.SessionID != "" && res.Provider == c.Provider {
		if res.SessionID != c.SessionID || session == "" {
			set["session_id"], set["session_provider"], set["session_turns"] = res.SessionID, res.Provider, 1
		} else {
			set["session_turns"] = c.SessionN + 1
		}
	} else {
		set["session_id"], set["session_turns"] = "", 0 // a failover turn: start fresh next time
	}
	_ = e.st.updateCourse(ctx, c.ID, set)

	// Keep the map growing ahead of the learner, so there's always somewhere new to go.
	fresh := 0
	for _, n := range x.Concepts {
		if n.Covered == 0 {
			fresh++
		}
	}
	if fresh < 4 && !e.st.hasJob(ctx, c.ID, "extend") {
		_ = e.st.enqueue(ctx, c.ID, "extend", 20, nil)
	}
	return e.st.enqueue(ctx, c.ID, "voice", 40, map[string]string{"segment_id": g.ID})
}

// addConcept puts a concept the teacher branched into on the map, under a module with that title
// (made if needed).
func (e *Engine) addConcept(ctx context.Context, c *Course, modules, concepts []*Node, title, objectives, module string) (*Node, error) {
	var mod *Node
	for _, m := range modules {
		if strings.EqualFold(strings.TrimSpace(m.Title), strings.TrimSpace(module)) {
			mod = m
		}
	}
	maxOrd := 0.0
	for _, n := range append(append([]*Node{}, modules...), concepts...) {
		if n.Ord > maxOrd {
			maxOrd = n.Ord
		}
	}
	if mod == nil {
		if strings.TrimSpace(module) == "" {
			module = "Further afield"
		}
		mod = &Node{CourseID: c.ID, Kind: "module", Ord: (float64(int(maxOrd/100)) + 1) * 100, Title: module}
		if err := e.st.insertNode(ctx, mod); err != nil {
			return nil, err
		}
		maxOrd = mod.Ord
	}
	last := mod.Ord
	for _, n := range concepts {
		if n.ParentID == mod.ID && n.Ord > last {
			last = n.Ord
		}
	}
	n := &Node{CourseID: c.ID, ParentID: mod.ID, Kind: "concept", Ord: last + 0.5, Title: strings.TrimSpace(title), Summary: objectives}
	return n, e.st.insertNode(ctx, n)
}

func (e *Engine) doVoice(ctx context.Context, j *Job) error {
	var p struct {
		SegmentID string `json:"segment_id"`
		Redo      bool   `json:"redo"` // re-voice a ready segment (a pronunciation changed); it stays playable meanwhile
	}
	if err := json.Unmarshal(j.Payload, &p); err != nil {
		return err
	}
	g, err := e.st.segment(ctx, p.SegmentID)
	if err != nil {
		return err
	}
	redo := p.Redo && g.Status == SegReady
	if g.Status != SegScripted && !redo {
		return nil // discarded or already voiced
	}
	c, err := e.st.course(ctx, g.CourseID)
	if err != nil {
		return err
	}
	old := g.AudioPath
	if !redo {
		_ = e.st.setSegmentStatus(ctx, g.ID, SegVoicing)
	}
	dir := filepath.Join(e.opt.dataDir, "audio", c.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// A new file name each time, so a browser never plays a stale copy.
	name := g.ID + "-" + newID()[:6]
	wav := filepath.Join(dir, name+".wav")
	m4a := filepath.Join(dir, name+".m4a")

	text, clips := e.speechFor(ctx, c, g)
	display, speech := paragraphs(g.Script), paragraphs(text)
	if len(speech) != len(display) {
		speech = display
	}
	r, err := e.opt.speaker.Render(ctx, c.VoiceID, display, speech, clips, wav)
	if err != nil {
		if !redo {
			_ = e.st.setSegmentStatus(context.WithoutCancel(ctx), g.ID, SegScripted)
		}
		return err
	}
	defer os.Remove(wav)
	if r.AlignScore < 0.6 && j.Attempts < 2 {
		if !redo {
			_ = e.st.setSegmentStatus(ctx, g.ID, SegScripted)
		}
		return fmt.Errorf("voice drifted from the script (alignment %.2f); re-voicing", r.AlignScore)
	}
	if out, err := exec.CommandContext(ctx, e.opt.ffmpeg, "-y", "-loglevel", "error", "-i", wav,
		"-af", "loudnorm=I=-16:TP=-1.5:LRA=11", "-ac", "1", "-c:a", "aac", "-b:a", "64k", "-movflags", "+faststart", m4a).CombinedOutput(); err != nil {
		if !redo {
			_ = e.st.setSegmentStatus(context.WithoutCancel(ctx), g.ID, SegScripted)
		}
		return fmt.Errorf("ffmpeg: %v: %s", err, out)
	}
	words, _ := json.Marshal(r.Words)
	rel, _ := filepath.Rel(e.opt.dataDir, m4a)
	g.Status, g.AudioPath, g.DurationMs, g.Words, g.AlignScore, g.VoiceID = SegReady, rel, int64(r.DurationS*1000), words, r.AlignScore, c.VoiceID
	// It may have been discarded while it was being voiced.
	if cur, err := e.st.segment(ctx, g.ID); err == nil && cur.Status == SegDiscarded {
		os.Remove(m4a)
		return nil
	}
	if err := e.st.updateSegment(ctx, g); err != nil {
		return err
	}
	if old != "" && old != rel {
		// Keep the old file a few minutes for a player mid-way through it; pruneOldAudio sweeps it later.
		_ = os.Rename(e.audioPath(old), e.audioPath(old)+".old")
	}
	return nil
}

type noteOut struct {
	Concept string `json:"concept"`
	Kind    string `json:"kind"`
	Text    string `json:"text"`
}

type answerOut struct {
	Title          string             `json:"title"`
	Script         string             `json:"script"`
	Summary        string             `json:"summary"`
	Verdict        string             `json:"verdict"`
	Replan         string             `json:"replan"`
	Drop           []int              `json:"drop"`
	About          string             `json:"about"`
	Profile        json.RawMessage    `json:"profile"`
	Mastery        map[string]float64 `json:"mastery"`
	Notes          []noteOut          `json:"notes"`
	Known          []string           `json:"known"`
	NextConcept    string             `json:"next_concept"`
	Pronunciations map[string]string  `json:"pronunciations"`
}

// loadMap reads a course's modules and concepts.
func (e *Engine) loadMap(ctx context.Context, c *Course) (x nextContext, err error) {
	nodes, err := e.st.nodes(ctx, c.ID)
	if err != nil {
		return x, err
	}
	for _, n := range nodes {
		if n.Kind == "concept" {
			x.Concepts = append(x.Concepts, n)
		} else {
			x.Modules = append(x.Modules, n)
		}
	}
	return x, nil
}

// workingSet fills in what's relevant to a turn about the candidate concepts: which ideas get full detail
// in the map, the notes tagged to them (plus a few recent general ones) and the learner's older
// questions about them. This is what keeps a turn small however long the course runs.
func (e *Engine) workingSet(ctx context.Context, c *Course, x *nextContext, cands []*Node) {
	x.InPlay = map[string]bool{}
	var ids []string
	for _, n := range cands {
		if n != nil && !x.InPlay[n.ID] {
			x.InPlay[n.ID] = true
			ids = append(ids, n.ID)
		}
	}
	x.Notes, _ = e.st.notes(ctx, c.ID, ids, false, 15)
	past, _ := e.st.interactionsOn(ctx, c.ID, ids, 8)
	seen := map[string]bool{}
	for _, i := range x.Asked {
		seen[i.ID] = true
	}
	for _, i := range past {
		if !seen[i.ID] && len(x.Past) < 5 {
			x.Past = append(x.Past, i)
		}
	}
}

// mentioned: concepts whose title appears in the text (for steering and questions).
func mentioned(concepts []*Node, text string) []*Node {
	t := strings.ToLower(text)
	if strings.TrimSpace(t) == "" {
		return nil
	}
	var out []*Node
	for _, n := range concepts {
		title := strings.ToLower(strings.TrimSpace(n.Title))
		if title != "" && (strings.Contains(t, title) || strings.Contains(title, strings.TrimSpace(t))) {
			out = append(out, n)
		}
	}
	return out
}

// weakest: covered concepts with the lowest mastery.
func weakest(concepts []*Node, k int) []*Node {
	var cov []*Node
	for _, n := range concepts {
		if n.Covered > 0 {
			cov = append(cov, n)
		}
	}
	sort.Slice(cov, func(a, b int) bool { return cov[a].Mastery < cov[b].Mastery })
	if len(cov) > k {
		cov = cov[:k]
	}
	return cov
}

func byTitle(concepts []*Node, title string) *Node {
	for _, n := range concepts {
		if strings.EqualFold(strings.TrimSpace(n.Title), strings.TrimSpace(title)) {
			return n
		}
	}
	return nil
}

// saveNotes keeps what a turn learned about the learner, tagged to concepts.
func (e *Engine) saveNotes(ctx context.Context, c *Course, concepts []*Node, notes []noteOut, source string) {
	for _, n := range notes {
		text := strings.TrimSpace(n.Text)
		kind := strings.ToLower(strings.TrimSpace(n.Kind))
		switch kind {
		case "misconception", "gap", "strength", "interest", "preference":
		default:
			kind = "note"
		}
		if text == "" {
			continue
		}
		node := ""
		if c := byTitle(concepts, n.Concept); c != nil {
			node = c.ID
		}
		_ = e.st.addNote(ctx, &Note{CourseID: c.ID, NodeID: node, Kind: kind, Text: text, Source: source})
	}
}

// recordRecall applies one retrieval attempt (check-in or quiz) to a concept. Mastered means two correct
// recalls more than a day apart; getting it right clears its open misconceptions.
func (e *Engine) recordRecall(ctx context.Context, c *Course, n *Node, verdict string) {
	if n == nil {
		return
	}
	switch strings.ToLower(strings.TrimSpace(verdict)) {
	case "correct":
		prev, perr := time.Parse(time.RFC3339Nano, n.LastRecallAt)
		if n.CorrectRecalls == 0 || perr != nil || time.Since(prev) > 24*time.Hour {
			n.CorrectRecalls++
		}
		n.LastRecallAt = now()
		n.Mastery = clamp01(n.Mastery + 0.1)
		if n.CorrectRecalls >= 2 && n.Mastery < 0.9 {
			n.Mastery = 0.9
		}
		e.st.resolveNotes(ctx, c.ID, n.ID, "misconception")
	case "partial":
		n.Mastery = clamp01(n.Mastery + 0.03)
	case "wrong":
		n.CorrectRecalls = 0
		if n.Mastery > 0.4 {
			n.Mastery = 0.4
		}
	}
	_ = e.st.updateNode(ctx, n)
}

func (e *Engine) doAnswer(ctx context.Context, j *Job) error {
	var p struct {
		InteractionID string `json:"interaction_id"`
	}
	if err := json.Unmarshal(j.Payload, &p); err != nil {
		return err
	}
	i, err := e.st.interaction(ctx, p.InteractionID)
	if err != nil {
		return err
	}
	if i.Status != "pending" {
		return nil
	}
	c, err := e.st.course(ctx, i.CourseID)
	if err != nil {
		return err
	}
	current, _ := e.st.segment(ctx, i.SegmentID)
	at := 0.0
	if current != nil {
		at = current.Ord
	}
	recent, _ := e.st.recentSegments(ctx, c.ID, at, 6)
	if len(recent) > 0 && current != nil && recent[len(recent)-1].ID == current.ID {
		recent = recent[:len(recent)-1]
	}
	upcoming, _ := e.st.segmentsAfter(ctx, c.ID, at, 8)
	x, err := e.loadMap(ctx, c)
	if err != nil {
		return err
	}
	var cands []*Node
	if current != nil {
		for _, n := range x.Concepts {
			if n.ID == current.NodeID {
				cands = append(cands, n)
			}
		}
	}
	cands = append(cands, mentioned(x.Concepts, i.Text)...)
	e.workingSet(ctx, c, &x, cands)

	var out answerOut
	res, err := e.turn(ctx, c, "learn-"+c.ID+"-ask", "", "", func(bool) string { return answerPrompt(c, i, current, recent, upcoming, x, false) }, &out)
	if err != nil {
		return err
	}
	if strings.TrimSpace(out.Script) == "" {
		return errors.New("empty answer")
	}

	// Insert right after where the owner is now, after any answers already queued there.
	base := at
	if pos, _ := e.st.ownerPosition(ctx, c); pos > base {
		base = pos
	}
	after, _ := e.st.segmentsAfter(ctx, c.ID, base, 20)
	k := 0
	for k < len(after) && (after[k].Kind == "answer" || after[k].Kind == "feedback") {
		base = after[k].Ord
		k++
	}
	ord := base + 1
	if k < len(after) {
		ord = (base + after[k].Ord) / 2
	}
	kind := "answer"
	if i.Kind == "checkin" {
		kind = "feedback"
	}
	about := byTitle(x.Concepts, out.About)
	if about == nil && current != nil {
		for _, n := range x.Concepts {
			if n.ID == current.NodeID {
				about = n
			}
		}
	}
	node := ""
	if about != nil {
		node = about.ID
	}
	g := &Segment{CourseID: c.ID, Ord: ord, Kind: kind, NodeID: node, Title: out.Title, Script: cleanScript(out.Script), Summary: out.Summary,
		Status: SegScripted, LLM: llmLabel(res), InteractionID: i.ID}
	g.Speech = applyPronunciations(g.Script, out.Pronunciations)
	g.Pron, _ = json.Marshal(out.Pronunciations)
	if err := e.st.insertSegment(ctx, g); err != nil {
		return err
	}
	if err := e.st.enqueue(ctx, c.ID, "voice", 95, map[string]string{"segment_id": g.ID}); err != nil {
		return err
	}
	// Change only what the feedback makes wrong, and only after where the learner is.
	if i.Kind == "ask" {
		dropped := 0
		for _, k := range out.Drop {
			if k >= 1 && k <= len(upcoming) {
				u := upcoming[k-1]
				if cur, err := e.st.segment(ctx, u.ID); err == nil && cur.PlayedAt == "" && cur.Ord > g.Ord && cur.Status != SegDiscarded {
					_ = e.st.setSegmentStatus(ctx, u.ID, SegDiscarded)
					dropped++
				}
			}
		}
		if dropped == 0 && len(out.Drop) == 0 && strings.EqualFold(out.Replan, "regenerate") {
			n, _ := e.st.discardAfter(ctx, c.ID, g.Ord, g.ID)
			dropped = int(n)
		}
		if dropped > 0 {
			e.log.Info().Str("course", c.ID).Int("discarded", dropped).Msg("replanning after the learner's input")
		}
	}

	// Learn from it.
	if len(out.Profile) > 2 && json.Valid(out.Profile) {
		_ = e.st.updateCourse(ctx, c.ID, map[string]any{"profile": []byte(out.Profile)})
	}
	for title, m := range out.Mastery {
		if n := byTitle(x.Concepts, title); n != nil {
			n.Mastery = clamp01(m)
			_ = e.st.updateNode(ctx, n)
		}
	}
	for _, t := range out.Known {
		if n := byTitle(x.Concepts, t); n != nil && n.Mastery < 0.8 {
			n.Mastery = 0.8
			_ = e.st.updateNode(ctx, n)
		}
	}
	e.saveNotes(ctx, c, x.Concepts, out.Notes, i.Kind)
	if next := strings.TrimSpace(out.NextConcept); next != "" {
		_ = e.st.updateCourse(ctx, c.ID, map[string]any{"steer": next})
	} else if i.Kind == "ask" && strings.EqualFold(out.Replan, "regenerate") {
		_ = e.st.updateCourse(ctx, c.ID, map[string]any{"steer": "follow up on what they just asked: " + i.Text})
	}
	if i.Kind == "checkin" && current != nil {
		if n, err := e.st.node(ctx, current.NodeID); err == nil {
			e.recordRecall(ctx, c, n, out.Verdict)
		}
	}
	return e.st.finishInteraction(ctx, i.ID, "answered", g.ID, strings.ToLower(strings.TrimSpace(out.Verdict)), node)
}

// ---- quizzes ----

type quizQuestion struct {
	Type        string   `json:"type"` // mc | short
	Concept     string   `json:"concept"`
	NodeID      string   `json:"node_id"`
	Q           string   `json:"q"`
	Options     []string `json:"options,omitempty"`
	Answer      int      `json:"answer"`
	Explain     string   `json:"explain,omitempty"`
	ModelAnswer string   `json:"model_answer,omitempty"`
	Rubric      string   `json:"rubric,omitempty"`
}

type quizResult struct {
	I        int    `json:"i"`
	Verdict  string `json:"verdict"`
	Feedback string `json:"feedback"`
}

func (e *Engine) doQuiz(ctx context.Context, j *Job) error {
	var p struct {
		QuizID string `json:"quiz_id"`
	}
	if err := json.Unmarshal(j.Payload, &p); err != nil {
		return err
	}
	q, err := e.st.quiz(ctx, p.QuizID)
	if err != nil {
		return err
	}
	c, err := e.st.course(ctx, q.CourseID)
	if err != nil {
		return err
	}
	x, err := e.loadMap(ctx, c)
	if err != nil {
		return err
	}
	var focus *Node
	cands := weakest(x.Concepts, 4)
	for _, n := range x.Concepts {
		if n.ID == q.NodeID {
			focus = n
			cands = append([]*Node{n}, cands...)
		}
	}
	e.workingSet(ctx, c, &x, cands)
	recent, _ := e.st.recentSegments(ctx, c.ID, 1e18, 15)
	var out struct {
		Questions []quizQuestion `json:"questions"`
	}
	if _, err := e.turn(ctx, c, "learn-"+c.ID+"-quiz", "", "", func(bool) string { return quizPrompt(c, x, recent, focus) }, &out); err != nil {
		return err
	}
	var qs []quizQuestion
	for _, qq := range out.Questions {
		qq.Type = strings.ToLower(qq.Type)
		if strings.TrimSpace(qq.Q) == "" || (qq.Type == "mc" && (len(qq.Options) < 2 || qq.Answer < 0 || qq.Answer >= len(qq.Options))) {
			continue
		}
		if qq.Type != "mc" {
			qq.Type = "short"
		}
		if n := byTitle(x.Concepts, qq.Concept); n != nil {
			qq.NodeID = n.ID
		}
		qs = append(qs, qq)
	}
	if len(qs) == 0 {
		return errors.New("the quiz came back empty")
	}
	q.Questions, _ = json.Marshal(qs)
	q.Status = "ready"
	return e.st.updateQuiz(ctx, q)
}

// submitQuiz marks multiple-choice answers at once and sends short answers for grading.
func (e *Engine) submitQuiz(ctx context.Context, c *Course, q *Quiz, answers []json.RawMessage) error {
	var qs []quizQuestion
	if err := json.Unmarshal(q.Questions, &qs); err != nil {
		return err
	}
	results := make([]quizResult, len(qs))
	needGrading := false
	nodes, _ := e.st.nodes(ctx, c.ID)
	byID := map[string]*Node{}
	for _, n := range nodes {
		byID[n.ID] = n
	}
	for k, qq := range qs {
		results[k] = quizResult{I: k}
		var raw json.RawMessage
		if k < len(answers) {
			raw = answers[k]
		}
		if qq.Type == "mc" {
			var pick int = -1
			_ = json.Unmarshal(raw, &pick)
			results[k].Verdict = "wrong"
			if pick == qq.Answer {
				results[k].Verdict = "correct"
			}
			results[k].Feedback = qq.Explain
			e.recordRecall(ctx, c, byID[qq.NodeID], results[k].Verdict)
		} else {
			needGrading = true
			results[k].Verdict = "pending"
		}
	}
	q.Answers, _ = json.Marshal(answers)
	q.Results, _ = json.Marshal(results)
	q.Score = quizScore(results)
	q.Status = "done"
	if needGrading {
		q.Status = "grading"
	}
	if err := e.st.updateQuiz(ctx, q); err != nil {
		return err
	}
	if needGrading {
		return e.st.enqueue(ctx, c.ID, "grade", 85, map[string]string{"quiz_id": q.ID})
	}
	return nil
}

func quizScore(rs []quizResult) float64 {
	if len(rs) == 0 {
		return 0
	}
	t := 0.0
	for _, r := range rs {
		switch r.Verdict {
		case "correct":
			t++
		case "partial":
			t += 0.5
		}
	}
	return t / float64(len(rs))
}

func (e *Engine) doGrade(ctx context.Context, j *Job) error {
	var p struct {
		QuizID string `json:"quiz_id"`
	}
	if err := json.Unmarshal(j.Payload, &p); err != nil {
		return err
	}
	q, err := e.st.quiz(ctx, p.QuizID)
	if err != nil {
		return err
	}
	if q.Status != "grading" {
		return nil
	}
	c, err := e.st.course(ctx, q.CourseID)
	if err != nil {
		return err
	}
	var qs []quizQuestion
	var answers []json.RawMessage
	var results []quizResult
	_ = json.Unmarshal(q.Questions, &qs)
	_ = json.Unmarshal(q.Answers, &answers)
	_ = json.Unmarshal(q.Results, &results)
	var items strings.Builder
	for k, qq := range qs {
		if qq.Type != "short" {
			continue
		}
		var a string
		if k < len(answers) {
			_ = json.Unmarshal(answers[k], &a)
		}
		fmt.Fprintf(&items, "Question i=%d (concept: %s): %s\nModel answer: %s\nMust contain: %s\nLearner's answer: %q\n\n", k, qq.Concept, qq.Q, qq.ModelAnswer, qq.Rubric, a)
	}
	var out struct {
		Results []quizResult `json:"results"`
		Notes   []noteOut    `json:"notes"`
	}
	if _, err := e.turn(ctx, c, "learn-"+c.ID+"-grade", "", "", func(bool) string { return gradePrompt(c, items.String()) }, &out); err != nil {
		return err
	}
	nodes, _ := e.st.nodes(ctx, c.ID)
	byID := map[string]*Node{}
	var concepts []*Node
	for _, n := range nodes {
		byID[n.ID] = n
		if n.Kind == "concept" {
			concepts = append(concepts, n)
		}
	}
	// Match each grade to its question: by index when the grader used ours, else by order among the
	// short-answer questions (graders often renumber from 0 or 1).
	var short []int
	isShort := map[int]bool{}
	for k, qq := range qs {
		if qq.Type == "short" {
			short = append(short, k)
			isShort[k] = true
		}
	}
	byIndex := true
	for _, r := range out.Results {
		byIndex = byIndex && isShort[r.I]
	}
	for pos, r := range out.Results {
		k := r.I
		if !byIndex {
			if pos >= len(short) {
				break
			}
			k = short[pos]
		}
		if k < 0 || k >= len(results) || !isShort[k] {
			continue
		}
		v := strings.ToLower(strings.TrimSpace(r.Verdict))
		if v != "correct" && v != "partial" {
			v = "wrong"
		}
		results[k] = quizResult{I: k, Verdict: v, Feedback: r.Feedback}
		e.recordRecall(ctx, c, byID[qs[k].NodeID], v)
	}
	for k := range results {
		if results[k].Verdict == "pending" {
			results[k].Verdict, results[k].Feedback = "partial", "Couldn't be graded automatically."
		}
	}
	e.saveNotes(ctx, c, concepts, out.Notes, "quiz")
	q.Results, _ = json.Marshal(results)
	q.Score = quizScore(results)
	q.Status = "done"
	return e.st.updateQuiz(ctx, q)
}

// speechFor: the text to voice — the teacher's hints, then the owner's lexicon, which always wins.
// A lexicon word with an approved clip stays as written and is returned in clips, so the voice plays
// exactly what the owner picked; the rest are respelled in the text.
func (e *Engine) speechFor(ctx context.Context, c *Course, g *Segment) (string, map[string]string) {
	all, _ := e.st.lexicon(ctx, c.Owner)
	clips := map[string]string{}
	var lex []LexEntry
	for _, l := range all {
		if p := e.ensureClip(ctx, c.Owner, c.VoiceID, l); p != "" {
			clips[l.Term] = p
		} else {
			lex = append(lex, l)
		}
	}
	return e.speechText(c, g, all, lex), clips
}

// lexClip is where the approved recording of a lexicon word in a voice lives. The spelling is part of
// the name, so changing it never plays a stale clip.
func (e *Engine) lexClip(owner, voiceID, term, say string) string {
	h := func(s string) string { b := sha256.Sum256([]byte(s)); return hex.EncodeToString(b[:])[:16] }
	return filepath.Join(e.opt.dataDir, "lexicon", h(owner), voiceID, h(strings.ToLower(term)+"|"+say)+".wav")
}

// ensureClip returns the word's clip in this voice, rendering it from the spelling if the owner chose it
// in another voice or by typing only.
func (e *Engine) ensureClip(ctx context.Context, owner, voiceID string, l LexEntry) string {
	p := e.lexClip(owner, voiceID, l.Term, l.Say)
	if _, err := os.Stat(p); err == nil {
		return p
	}
	if e.opt.speaker == nil || os.MkdirAll(filepath.Dir(p), 0o755) != nil {
		return ""
	}
	if err := e.opt.speaker.Say(ctx, voiceID, speakable(l.Say), p); err != nil {
		e.log.Warn().Err(err).Str("term", l.Term).Msg("couldn't render a pronunciation clip; respelling instead")
		return ""
	}
	return p
}

// speechText always starts from the script as shown. (Early segments stored a spoken copy with the
// teacher's respellings baked in, e.g. "eh-LAS-tee-oh", which a lexicon entry for "Elastio" could
// never match.)
func (e *Engine) speechText(c *Course, g *Segment, all, lex []LexEntry) string {
	pron := map[string]string{}
	if len(g.Pron) > 2 {
		_ = json.Unmarshal(g.Pron, &pron)
	}
	for _, l := range all {
		for k := range pron {
			if strings.EqualFold(k, l.Term) {
				delete(pron, k)
			}
		}
	}
	// Lexicon last: a hint like "elastio-stack" -> "elastio stack" must not bring the old word back.
	return applyLexicon(applyPronunciations(g.Script, pron), lex)
}

func applyLexicon(text string, lex []LexEntry) string {
	sort.Slice(lex, func(a, b int) bool { return len(lex[a].Term) > len(lex[b].Term) })
	for _, l := range lex {
		re, err := regexp.Compile(`(?i)(^|[^\pL\pN])` + regexp.QuoteMeta(l.Term) + `($|[^\pL\pN])`)
		if err != nil || strings.TrimSpace(l.Say) == "" {
			continue
		}
		text = re.ReplaceAllString(text, "${1}"+strings.ReplaceAll(speakable(l.Say), "$", "$$")+"${2}")
	}
	return text
}

var syllables = regexp.MustCompile(`^\pL+(-\pL+)+$`)

// speakable turns a dictionary-style respelling ("ee-LAS-tee-oh") into one word ("Eelasteeoh"). The voice
// pauses at hyphens and reads capitals as letters, so a syllable spelling comes out as "ee, L-A-S, tee, oh".
func speakable(say string) string {
	say = strings.TrimSpace(say)
	if !syllables.MatchString(say) {
		return say
	}
	lower, caps := false, false // a stress-marked spelling mixes lowercase and capitalised syllables
	for _, p := range strings.Split(say, "-") {
		lower = lower || p == strings.ToLower(p)
		caps = caps || (len([]rune(p)) > 1 && p == strings.ToUpper(p))
	}
	if !lower || !caps {
		return say
	}
	w := strings.ToLower(strings.ReplaceAll(say, "-", ""))
	r := []rune(w)
	return strings.ToUpper(string(r[0])) + string(r[1:])
}

// revoice re-voices unheard segments that use term: the one the owner is on first (the player swaps it
// in at the same word), then the rest in listening order. They stay playable with the old audio until
// the new audio is ready. Segments not voiced yet need nothing: they pick the lexicon up when voiced.
func (e *Engine) revoice(ctx context.Context, owner, term string) int {
	segs, err := e.st.unheardWith(ctx, owner, term)
	if err != nil {
		return 0
	}
	current := map[string]string{} // course -> segment being listened to
	for _, g := range segs {
		if _, ok := current[g.CourseID]; !ok {
			current[g.CourseID] = ""
			if pb, err := e.st.playback(ctx, owner, g.CourseID); err == nil {
				current[g.CourseID] = pb.SegmentID
			}
		}
	}
	n, rank := 0, map[string]int{}
	for _, g := range segs {
		prio := 50 - rank[g.CourseID]
		if g.ID == current[g.CourseID] {
			prio = 60
		} else {
			rank[g.CourseID]++
		}
		if prio < 36 {
			prio = 36
		}
		if e.st.voiceQueued(ctx, g.ID) {
			continue
		}
		_ = e.st.enqueue(ctx, g.CourseID, "voice", prio, map[string]any{"segment_id": g.ID, "redo": true})
		n++
	}
	e.poke()
	return n
}

// pruneClips removes pronunciation trial clips older than a day, and audio replaced by a re-voice.
func (e *Engine) pruneClips() {
	olds, _ := filepath.Glob(filepath.Join(e.opt.dataDir, "audio", "*", "*.old"))
	for _, f := range olds {
		if info, err := os.Stat(f); err == nil && time.Since(info.ModTime()) > time.Hour {
			_ = os.Remove(f)
		}
	}
	dir := filepath.Join(e.opt.dataDir, "pron")
	ents, _ := os.ReadDir(dir)
	for _, d := range ents {
		if info, err := d.Info(); err == nil && time.Since(info.ModTime()) > 24*time.Hour {
			_ = os.RemoveAll(filepath.Join(dir, d.Name()))
		}
	}
}

// audioPath resolves a stored audio path (relative to the data folder; older rows are absolute).
func (e *Engine) audioPath(p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(e.opt.dataDir, p)
}

// ---- text helpers ----

var blankLines = regexp.MustCompile(`\n\s*\n`)

func paragraphs(s string) []string {
	var out []string
	for _, p := range blankLines.Split(strings.TrimSpace(s), -1) {
		if p = strings.Join(strings.Fields(p), " "); p != "" {
			out = append(out, p)
		}
	}
	return out
}

var mdNoise = regexp.MustCompile("(?m)^\\s*(#+|[-*•]|\\d+[.)])\\s+|[*`]{1,3}")

// cleanScript strips stray markdown so the transcript reads cleanly and the voice doesn't say "asterisk".
func cleanScript(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = mdNoise.ReplaceAllString(s, "")
	return strings.Join(paragraphs(s), "\n\n")
}

func applyPronunciations(script string, p map[string]string) string {
	out := script
	keys := make([]string, 0, len(p))
	for k := range p {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(a, b int) bool { return len(keys[a]) > len(keys[b]) })
	for _, term := range keys {
		say := strings.TrimSpace(p[term])
		if strings.TrimSpace(term) == "" || say == "" {
			continue
		}
		re, err := regexp.Compile(`(^|[^\pL\pN])` + regexp.QuoteMeta(term) + `($|[^\pL\pN])`)
		if err != nil {
			continue
		}
		out = re.ReplaceAllString(out, "${1}"+strings.ReplaceAll(say, "$", "$$")+"${2}")
	}
	return out
}

func clamp01(f float64) float64 {
	if f < 0 {
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}
