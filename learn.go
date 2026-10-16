package learn

import (
	"context"
	"database/sql"
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
}

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
	agentKinds := []string{"answer", "calibrate", "plan", "extend", "script"}
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
	for ctx.Err() == nil {
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
	}
	return fmt.Errorf("unknown job kind %q", j.Kind)
}

// jobGaveUp records a final failure where the owner will see it.
func (e *Engine) jobGaveUp(ctx context.Context, j *Job, err error) {
	switch j.Kind {
	case "calibrate", "plan":
		_ = e.st.updateCourse(ctx, j.CourseID, map[string]any{"status": StatusError, "error": err.Error()})
	case "voice":
		var p struct{ SegmentID string `json:"segment_id"` }
		_ = json.Unmarshal(j.Payload, &p)
		_ = e.st.setSegmentStatus(ctx, p.SegmentID, SegFailed)
	case "answer":
		var p struct{ InteractionID string `json:"interaction_id"` }
		_ = json.Unmarshal(j.Payload, &p)
		_ = e.st.finishInteraction(ctx, p.InteractionID, "failed", "", "")
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
		if ready+pending >= e.opt.bufferAhead.Seconds() || e.st.hasJob(ctx, c.ID, "script", "extend", "plan") {
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
	try := func(provider, model, resume string) (agent.Result, error) {
		res, err := e.opt.runner.Run(ctx, agent.Turn{
			Provider: provider, Model: model, Dir: dir, Prompt: prompt(resume != ""), Resume: resume, Name: name,
		})
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

type scriptOut struct {
	Title          string            `json:"title"`
	Script         string            `json:"script"`
	Summary        string            `json:"summary"`
	ConceptDone    bool              `json:"concept_done"`
	Checkin        string            `json:"checkin"`
	Pronunciations map[string]string `json:"pronunciations"`
}

func (e *Engine) doScript(ctx context.Context, j *Job) error {
	c, err := e.st.course(ctx, j.CourseID)
	if err != nil {
		return err
	}
	nodes, err := e.st.nodes(ctx, c.ID)
	if err != nil {
		return err
	}
	var concepts, modules []*Node
	byID := map[string]*Node{}
	for _, n := range nodes {
		byID[n.ID] = n
		if n.Kind == "concept" {
			concepts = append(concepts, n)
		} else {
			modules = append(modules, n)
		}
	}
	if len(concepts) == 0 {
		return errors.New("no curriculum yet")
	}
	last, err := e.st.lastSegment(ctx, c.ID)
	if err != nil && !errors.Is(err, errNotFound) {
		return err
	}
	// Where are we? The last lesson segment decides: continue its concept, or move to the next one.
	var lastLesson *Segment
	recentAll, _ := e.st.recentSegments(ctx, c.ID, 1e18, 40)
	lessonsInConcept, sinceCheckin := 0, 0
	for k := len(recentAll) - 1; k >= 0; k-- {
		if recentAll[k].Kind == "lesson" {
			lastLesson = recentAll[k]
			break
		}
	}
	x := segmentContext{}
	idx := 0
	if lastLesson == nil {
		x.Concept, x.NewConcept = concepts[0], true
	} else {
		for k, n := range concepts {
			if n.ID == lastLesson.NodeID {
				idx = k
			}
		}
		for _, g := range recentAll {
			if g.Kind == "lesson" && g.NodeID == lastLesson.NodeID {
				lessonsInConcept++
			}
		}
		if lastLesson.ConceptDone || lessonsInConcept >= 8 {
			idx++
			x.NewConcept = true
		}
		if idx >= len(concepts) {
			return e.st.enqueue(ctx, c.ID, "extend", 30, nil)
		}
		x.Concept = concepts[idx]
	}
	for k, n := range concepts {
		if n.ID == x.Concept.ID {
			idx = k
		}
	}
	if idx+1 < len(concepts) {
		x.NextConceptTitle = concepts[idx+1].Title
	}
	x.Module = byID[x.Concept.ParentID]
	if x.Module == nil {
		return errors.New("concept has no module")
	}
	for k, m := range modules {
		if m.ID == x.Module.ID {
			x.ModuleIndex = k + 1
		}
	}
	x.Of = len(modules)
	var inModule []*Node
	for _, n := range concepts {
		if n.ParentID == x.Module.ID {
			inModule = append(inModule, n)
		}
	}
	for k, n := range inModule {
		if n.ID == x.Concept.ID {
			x.ConceptIndex = k + 1
		}
	}
	x.OfCon = len(inModule)
	for k := len(recentAll) - 1; k >= 0; k-- {
		if recentAll[k].Checkin != "" {
			break
		}
		if recentAll[k].Kind == "lesson" {
			sinceCheckin++
		}
	}
	x.CheckinDue = sinceCheckin >= 4
	if n := len(recentAll); n > 8 {
		x.Recent = recentAll[n-8:]
	} else {
		x.Recent = recentAll
	}
	ord := 1.0
	if last != nil {
		ord = float64(int(last.Ord)) + 1
	}
	// Spaced review: every sixth segment, call back to weak finished concepts.
	if int(ord)%6 == 5 {
		var weak []*Node
		for _, n := range concepts[:idx] {
			if n.Status == "done" && n.Mastery < 0.8 {
				weak = append(weak, n)
			}
		}
		sort.Slice(weak, func(a, b int) bool { return weak[a].Mastery < weak[b].Mastery })
		if len(weak) > 2 {
			weak = weak[:2]
		}
		x.Reviews = weak
	}

	var out scriptOut
	res, err := e.turn(ctx, c, "learn-"+c.ID+"-m"+x.Module.ID, x.Module.SessionProv, x.Module.SessionID,
		func(resumed bool) string { return scriptPrompt(c, x, resumed) }, &out)
	if err != nil {
		return err
	}
	if strings.TrimSpace(out.Script) == "" {
		return errors.New("empty script")
	}
	if res.SessionID != "" && (res.SessionID != x.Module.SessionID || res.Provider != x.Module.SessionProv) {
		x.Module.SessionID, x.Module.SessionProv = res.SessionID, res.Provider
		_ = e.st.updateNode(ctx, x.Module)
	}
	g := &Segment{
		CourseID: c.ID, Ord: ord, Kind: "lesson", NodeID: x.Concept.ID, Title: out.Title, Script: cleanScript(out.Script),
		Summary: out.Summary, Checkin: strings.TrimSpace(out.Checkin), ConceptDone: out.ConceptDone, Status: SegScripted, LLM: llmLabel(res),
	}
	g.Speech = applyPronunciations(g.Script, out.Pronunciations)
	if err := e.st.insertSegment(ctx, g); err != nil {
		return err
	}
	if x.Concept.Status == "pending" || out.ConceptDone {
		x.Concept.Status = "active"
		if out.ConceptDone {
			x.Concept.Status = "done"
		}
		_ = e.st.updateNode(ctx, x.Concept)
	}
	_ = e.st.updateCourse(ctx, c.ID, map[string]any{"error": ""})
	return e.st.enqueue(ctx, c.ID, "voice", 40, map[string]string{"segment_id": g.ID})
}

func (e *Engine) doVoice(ctx context.Context, j *Job) error {
	var p struct {
		SegmentID string `json:"segment_id"`
	}
	if err := json.Unmarshal(j.Payload, &p); err != nil {
		return err
	}
	g, err := e.st.segment(ctx, p.SegmentID)
	if err != nil {
		return err
	}
	if g.Status != SegScripted {
		return nil // discarded or already voiced
	}
	c, err := e.st.course(ctx, g.CourseID)
	if err != nil {
		return err
	}
	_ = e.st.setSegmentStatus(ctx, g.ID, SegVoicing)
	dir := filepath.Join(e.opt.dataDir, "audio", c.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	wav := filepath.Join(dir, g.ID+".wav")
	m4a := filepath.Join(dir, g.ID+".m4a")
	display, speech := paragraphs(g.Script), paragraphs(g.Speech)
	if len(speech) != len(display) {
		speech = display
	}
	r, err := e.opt.speaker.Render(ctx, c.VoiceID, display, speech, wav)
	if err != nil {
		_ = e.st.setSegmentStatus(context.WithoutCancel(ctx), g.ID, SegScripted)
		return err
	}
	defer os.Remove(wav)
	if r.AlignScore < 0.6 && j.Attempts < 2 {
		_ = e.st.setSegmentStatus(ctx, g.ID, SegScripted)
		return fmt.Errorf("voice drifted from the script (alignment %.2f); re-voicing", r.AlignScore)
	}
	if out, err := exec.CommandContext(ctx, e.opt.ffmpeg, "-y", "-loglevel", "error", "-i", wav,
		"-af", "loudnorm=I=-16:TP=-1.5:LRA=11", "-ac", "1", "-c:a", "aac", "-b:a", "64k", "-movflags", "+faststart", m4a).CombinedOutput(); err != nil {
		_ = e.st.setSegmentStatus(context.WithoutCancel(ctx), g.ID, SegScripted)
		return fmt.Errorf("ffmpeg: %v: %s", err, out)
	}
	words, _ := json.Marshal(r.Words)
	g.Status, g.AudioPath, g.DurationMs, g.Words, g.AlignScore, g.VoiceID = SegReady, m4a, int64(r.DurationS*1000), words, r.AlignScore, c.VoiceID
	// It may have been discarded while it was being voiced.
	if cur, err := e.st.segment(ctx, g.ID); err == nil && cur.Status == SegDiscarded {
		os.Remove(m4a)
		return nil
	}
	return e.st.updateSegment(ctx, g)
}

type answerOut struct {
	Title          string             `json:"title"`
	Script         string             `json:"script"`
	Summary        string             `json:"summary"`
	Verdict        string             `json:"verdict"`
	Replan         string             `json:"replan"`
	Profile        json.RawMessage    `json:"profile"`
	Mastery        map[string]float64 `json:"mastery"`
	Known          []string           `json:"known"`
	NextConcept    string             `json:"next_concept"`
	Pronunciations map[string]string  `json:"pronunciations"`
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
	upcoming, _ := e.st.segmentsAfter(ctx, c.ID, at, 4)
	curriculum, _ := e.st.nodes(ctx, c.ID)
	var out answerOut
	res, err := e.turn(ctx, c, "learn-"+c.ID+"-ask", "", "", func(bool) string { return answerPrompt(c, i, current, recent, upcoming, curriculum, false) }, &out)
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
	node := ""
	if current != nil {
		node = current.NodeID
	}
	g := &Segment{CourseID: c.ID, Ord: ord, Kind: kind, NodeID: node, Title: out.Title, Script: cleanScript(out.Script), Summary: out.Summary,
		Status: SegScripted, LLM: llmLabel(res), InteractionID: i.ID}
	g.Speech = applyPronunciations(g.Script, out.Pronunciations)
	if err := e.st.insertSegment(ctx, g); err != nil {
		return err
	}
	if err := e.st.enqueue(ctx, c.ID, "voice", 95, map[string]string{"segment_id": g.ID}); err != nil {
		return err
	}
	if strings.EqualFold(out.Replan, "regenerate") && i.Kind == "ask" {
		n, _ := e.st.discardAfter(ctx, c.ID, g.Ord, g.ID)
		e.log.Info().Str("course", c.ID).Int64("discarded", n).Msg("replanning after the learner's input")
	}
	e.learnFrom(ctx, c, current, out, i)
	return e.st.finishInteraction(ctx, i.ID, "answered", g.ID, strings.ToLower(strings.TrimSpace(out.Verdict)))
}

// learnFrom updates the profile and mastery from an answer turn. A concept counts as mastered only
// after two correct recalls more than a day apart.
func (e *Engine) learnFrom(ctx context.Context, c *Course, current *Segment, out answerOut, i *Interaction) {
	if len(out.Profile) > 2 && json.Valid(out.Profile) {
		_ = e.st.updateCourse(ctx, c.ID, map[string]any{"profile": []byte(out.Profile)})
	}
	nodes, _ := e.st.nodes(ctx, c.ID)
	for title, m := range out.Mastery {
		for _, n := range nodes {
			if n.Kind == "concept" && strings.EqualFold(strings.TrimSpace(n.Title), strings.TrimSpace(title)) {
				n.Mastery = clamp01(m)
				_ = e.st.updateNode(ctx, n)
			}
		}
	}
	// Steering the curriculum: skip what they know, pull forward what they asked for.
	find := func(title string) *Node {
		for _, n := range nodes {
			if n.Kind == "concept" && strings.EqualFold(strings.TrimSpace(n.Title), strings.TrimSpace(title)) {
				return n
			}
		}
		return nil
	}
	var cur *Node
	if current != nil {
		for _, n := range nodes {
			if n.ID == current.NodeID {
				cur = n
			}
		}
	}
	for _, t := range out.Known {
		if n := find(t); n != nil && n.Status != "done" {
			n.Status = "done"
			if n.Mastery < 0.8 {
				n.Mastery = 0.8
			}
			_ = e.st.updateNode(ctx, n)
			if cur != nil && n.ID == cur.ID {
				e.markConceptDone(ctx, current)
			}
		}
	}
	if n := find(out.NextConcept); n != nil && cur != nil && n.ID != cur.ID && n.Status != "done" {
		// Teach it next: move it just after the current concept and close the current one.
		_, _ = e.st.db.ExecContext(ctx, `UPDATE learn_nodes SET ord=? WHERE id=?`, cur.Ord+0.01, n.ID)
		e.markConceptDone(ctx, current)
		e.log.Info().Str("course", c.ID).Str("concept", n.Title).Msg("pulled a concept forward")
	}
	if i.Kind != "checkin" || current == nil {
		return
	}
	n, err := e.st.node(ctx, current.NodeID)
	if err != nil {
		return
	}
	switch strings.ToLower(strings.TrimSpace(out.Verdict)) {
	case "correct":
		prev, perr := time.Parse(time.RFC3339Nano, n.LastRecallAt)
		if n.CorrectRecalls == 0 || perr != nil || time.Since(prev) > 24*time.Hour {
			n.CorrectRecalls++
		}
		n.LastRecallAt = now()
		if n.CorrectRecalls >= 2 && n.Mastery < 0.9 {
			n.Mastery = 0.9
		}
	case "wrong":
		n.CorrectRecalls = 0
		if n.Mastery > 0.4 {
			n.Mastery = 0.4
		}
	}
	_ = e.st.updateNode(ctx, n)
}

// markConceptDone makes the next script move on from the concept the learner was in.
func (e *Engine) markConceptDone(ctx context.Context, current *Segment) {
	if current == nil {
		return
	}
	_, _ = e.st.db.ExecContext(ctx, `UPDATE learn_segments SET concept_done=1 WHERE course_id=? AND node_id=? AND kind='lesson'`, current.CourseID, current.NodeID)
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
