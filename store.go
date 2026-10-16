package learn

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// schema: SQLite dialect, idempotent, every table prefixed learn_ so it can share a host's database.
var schema = []string{
	`CREATE TABLE IF NOT EXISTS learn_courses (
		id TEXT PRIMARY KEY,
		owner TEXT NOT NULL,
		title TEXT NOT NULL DEFAULT '',
		topic TEXT NOT NULL,
		goal TEXT NOT NULL DEFAULT '',
		background TEXT NOT NULL DEFAULT '',
		voice_id TEXT NOT NULL,
		provider TEXT NOT NULL DEFAULT 'claude',
		model TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL,
		calibration TEXT NOT NULL DEFAULT '{}',
		profile TEXT NOT NULL DEFAULT '{}',
		error TEXT NOT NULL DEFAULT '',
		paused INTEGER NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS learn_members (
		course_id TEXT NOT NULL,
		user_id TEXT NOT NULL,
		added_at TEXT NOT NULL,
		PRIMARY KEY (course_id, user_id))`,
	`CREATE TABLE IF NOT EXISTS learn_nodes (
		id TEXT PRIMARY KEY,
		course_id TEXT NOT NULL,
		parent_id TEXT NOT NULL DEFAULT '',
		kind TEXT NOT NULL,
		ord REAL NOT NULL,
		title TEXT NOT NULL,
		summary TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL DEFAULT 'pending',
		mastery REAL NOT NULL DEFAULT 0,
		correct_recalls INTEGER NOT NULL DEFAULT 0,
		last_recall_at TEXT NOT NULL DEFAULT '',
		next_review_at TEXT NOT NULL DEFAULT '',
		session_id TEXT NOT NULL DEFAULT '',
		session_provider TEXT NOT NULL DEFAULT '')`,
	`CREATE INDEX IF NOT EXISTS learn_nodes_course ON learn_nodes(course_id, ord)`,
	`CREATE TABLE IF NOT EXISTS learn_segments (
		id TEXT PRIMARY KEY,
		course_id TEXT NOT NULL,
		ord REAL NOT NULL,
		kind TEXT NOT NULL,
		node_id TEXT NOT NULL DEFAULT '',
		title TEXT NOT NULL DEFAULT '',
		script TEXT NOT NULL,
		speech TEXT NOT NULL DEFAULT '',
		summary TEXT NOT NULL DEFAULT '',
		checkin TEXT NOT NULL DEFAULT '',
		concept_done INTEGER NOT NULL DEFAULT 0,
		status TEXT NOT NULL,
		audio_path TEXT NOT NULL DEFAULT '',
		duration_ms INTEGER NOT NULL DEFAULT 0,
		words TEXT NOT NULL DEFAULT '[]',
		align_score REAL NOT NULL DEFAULT 0,
		voice_id TEXT NOT NULL DEFAULT '',
		llm TEXT NOT NULL DEFAULT '',
		interaction_id TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL,
		played_at TEXT NOT NULL DEFAULT '')`,
	`CREATE INDEX IF NOT EXISTS learn_segments_course ON learn_segments(course_id, ord)`,
	`CREATE TABLE IF NOT EXISTS learn_interactions (
		id TEXT PRIMARY KEY,
		course_id TEXT NOT NULL,
		user_id TEXT NOT NULL,
		kind TEXT NOT NULL,
		segment_id TEXT NOT NULL DEFAULT '',
		offset_ms INTEGER NOT NULL DEFAULT 0,
		text TEXT NOT NULL,
		status TEXT NOT NULL,
		answer_segment_id TEXT NOT NULL DEFAULT '',
		verdict TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL)`,
	`CREATE INDEX IF NOT EXISTS learn_interactions_course ON learn_interactions(course_id, created_at)`,
	`CREATE TABLE IF NOT EXISTS learn_playback (
		user_id TEXT NOT NULL,
		course_id TEXT NOT NULL,
		segment_id TEXT NOT NULL,
		offset_ms INTEGER NOT NULL DEFAULT 0,
		rate REAL NOT NULL DEFAULT 1,
		updated_at TEXT NOT NULL,
		PRIMARY KEY (user_id, course_id))`,
	`CREATE TABLE IF NOT EXISTS learn_jobs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		course_id TEXT NOT NULL,
		kind TEXT NOT NULL,
		priority INTEGER NOT NULL,
		payload TEXT NOT NULL DEFAULT '{}',
		status TEXT NOT NULL,
		attempts INTEGER NOT NULL DEFAULT 0,
		error TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL)`,
	`CREATE INDEX IF NOT EXISTS learn_jobs_queue ON learn_jobs(status, priority, id)`,
	// What the course has learned about the learner, tagged to concepts so only relevant notes are
	// loaded into a turn.
	`CREATE TABLE IF NOT EXISTS learn_notes (
		id TEXT PRIMARY KEY,
		course_id TEXT NOT NULL,
		node_id TEXT NOT NULL DEFAULT '',
		kind TEXT NOT NULL,
		text TEXT NOT NULL,
		source TEXT NOT NULL DEFAULT '',
		resolved INTEGER NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL)`,
	`CREATE INDEX IF NOT EXISTS learn_notes_course ON learn_notes(course_id, node_id, resolved)`,
	`CREATE TABLE IF NOT EXISTS learn_quizzes (
		id TEXT PRIMARY KEY,
		course_id TEXT NOT NULL,
		node_id TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL,
		questions TEXT NOT NULL DEFAULT '[]',
		answers TEXT NOT NULL DEFAULT '[]',
		results TEXT NOT NULL DEFAULT '[]',
		score REAL NOT NULL DEFAULT 0,
		error TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL)`,
	`CREATE INDEX IF NOT EXISTS learn_quizzes_course ON learn_quizzes(course_id, created_at)`,
}

// columns added after the first release; "duplicate column" errors are expected and ignored.
var alters = []string{
	`ALTER TABLE learn_courses ADD COLUMN session_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE learn_courses ADD COLUMN session_provider TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE learn_courses ADD COLUMN session_turns INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE learn_courses ADD COLUMN steer TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE learn_nodes ADD COLUMN covered INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE learn_nodes ADD COLUMN last_covered_at TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE learn_playback ADD COLUMN device TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE learn_interactions ADD COLUMN node_id TEXT NOT NULL DEFAULT ''`,
	`ALTER TABLE learn_courses ADD COLUMN research INTEGER NOT NULL DEFAULT 1`,
	`ALTER TABLE learn_segments ADD COLUMN pron TEXT NOT NULL DEFAULT '{}'`,
	`CREATE TABLE IF NOT EXISTS learn_lexicon (
		owner TEXT NOT NULL,
		term TEXT NOT NULL,
		say TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		PRIMARY KEY (owner, term))`,
}

// Course statuses.
const (
	StatusCalibrating = "calibrating" // writing the level questions
	StatusAwaiting    = "awaiting"    // waiting for the owner's answers
	StatusPlanning    = "planning"    // writing the curriculum
	StatusActive      = "active"
	StatusError       = "error"
)

// Segment statuses.
const (
	SegScripted  = "scripted"
	SegVoicing   = "voicing"
	SegReady     = "ready"
	SegFailed    = "failed"
	SegDiscarded = "discarded"
)

type Course struct {
	ID          string          `json:"id"`
	Owner       string          `json:"owner"`
	Title       string          `json:"title"`
	Topic       string          `json:"topic"`
	Goal        string          `json:"goal"`
	Background  string          `json:"background"`
	VoiceID     string          `json:"voice_id"`
	Provider    string          `json:"provider"`
	Model       string          `json:"model"`
	Status      string          `json:"status"`
	Calibration Calibration     `json:"calibration"`
	Profile     json.RawMessage `json:"profile"`
	Error       string          `json:"error"`
	Paused      bool            `json:"paused"`
	CreatedAt   string          `json:"created_at"`
	UpdatedAt   string          `json:"updated_at"`
	SessionID   string          `json:"-"`
	SessionProv string          `json:"-"`
	SessionN    int             `json:"-"`
	Steer       string          `json:"steer"`
	Research    bool            `json:"research"` // the teacher may read local code and GitHub (read-only)
}

type Calibration struct {
	Questions []CalQuestion `json:"questions"`
	Skipped   bool          `json:"skipped,omitempty"`
}

type CalQuestion struct {
	Q      string `json:"q"`
	Why    string `json:"why,omitempty"`
	Answer string `json:"answer,omitempty"`
}

type Node struct {
	ID             string  `json:"id"`
	CourseID       string  `json:"-"`
	ParentID       string  `json:"parent_id"`
	Kind           string  `json:"kind"`
	Ord            float64 `json:"ord"`
	Title          string  `json:"title"`
	Summary        string  `json:"summary"`
	Status         string  `json:"status"`
	Mastery        float64 `json:"mastery"`
	CorrectRecalls int     `json:"correct_recalls"`
	LastRecallAt   string  `json:"last_recall_at"`
	SessionID      string  `json:"-"`
	SessionProv    string  `json:"-"`
	Covered        int     `json:"covered"`
	LastCoveredAt  string  `json:"last_covered_at"`
}

type Segment struct {
	ID            string          `json:"id"`
	CourseID      string          `json:"course_id"`
	Ord           float64         `json:"ord"`
	Kind          string          `json:"kind"`
	NodeID        string          `json:"node_id"`
	Title         string          `json:"title"`
	Script        string          `json:"script"`
	Speech        string          `json:"-"`
	Summary       string          `json:"summary"`
	Checkin       string          `json:"checkin"`
	ConceptDone   bool            `json:"-"`
	Status        string          `json:"status"`
	AudioPath     string          `json:"-"`
	DurationMs    int64           `json:"duration_ms"`
	Words         json.RawMessage `json:"words,omitempty"`
	AlignScore    float64         `json:"align_score"`
	VoiceID       string          `json:"voice_id"`
	LLM           string          `json:"llm"`
	InteractionID string          `json:"interaction_id"`
	CreatedAt     string          `json:"created_at"`
	PlayedAt      string          `json:"played_at"`
	Pron          json.RawMessage `json:"-"` // the teacher's pronunciation hints for this segment
}

type Interaction struct {
	ID              string `json:"id"`
	CourseID        string `json:"course_id"`
	UserID          string `json:"-"`
	Kind            string `json:"kind"` // ask | checkin
	SegmentID       string `json:"segment_id"`
	OffsetMs        int64  `json:"offset_ms"`
	Text            string `json:"text"`
	Status          string `json:"status"` // pending | answered | failed
	AnswerSegmentID string `json:"answer_segment_id"`
	Verdict         string `json:"verdict"`
	CreatedAt       string `json:"created_at"`
	NodeID          string `json:"node_id"`
}

type Playback struct {
	Device    string  `json:"device"`
	SegmentID string  `json:"segment_id"`
	OffsetMs  int64   `json:"offset_ms"`
	Rate      float64 `json:"rate"`
	UpdatedAt string  `json:"updated_at"`
}

type Job struct {
	ID       int64
	CourseID string
	Kind     string
	Priority int
	Payload  json.RawMessage
	Attempts int
}

type store struct{ db *sql.DB }

var errNotFound = errors.New("not found")

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *store) migrate(ctx context.Context) error {
	for _, q := range schema {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("learn: migrate: %w", err)
		}
	}
	for _, q := range alters {
		if _, err := s.db.ExecContext(ctx, q); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return fmt.Errorf("learn: migrate: %w", err)
		}
	}
	// Jobs that were running when the process stopped go back to the queue.
	_, err := s.db.ExecContext(ctx, `UPDATE learn_jobs SET status='queued' WHERE status='running'`)
	if err == nil {
		_, err = s.db.ExecContext(ctx, `UPDATE learn_segments SET status='scripted' WHERE status='voicing'`)
	}
	return err
}

// ---- courses ----

const courseCols = `id, owner, title, topic, goal, background, voice_id, provider, model, status, calibration, profile, error, paused, created_at, updated_at, session_id, session_provider, session_turns, steer, research`

func scanCourse(row interface{ Scan(...any) error }) (*Course, error) {
	var c Course
	var cal, prof string
	var paused, research int
	if err := row.Scan(&c.ID, &c.Owner, &c.Title, &c.Topic, &c.Goal, &c.Background, &c.VoiceID, &c.Provider, &c.Model,
		&c.Status, &cal, &prof, &c.Error, &paused, &c.CreatedAt, &c.UpdatedAt, &c.SessionID, &c.SessionProv, &c.SessionN, &c.Steer, &research); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errNotFound
		}
		return nil, err
	}
	_ = json.Unmarshal([]byte(cal), &c.Calibration)
	if prof == "" {
		prof = "{}"
	}
	c.Profile = json.RawMessage(prof)
	c.Paused = paused != 0
	c.Research = research != 0
	return &c, nil
}

func (s *store) createCourse(ctx context.Context, c *Course) error {
	c.ID, c.CreatedAt, c.UpdatedAt = newID(), now(), now()
	cal, _ := json.Marshal(c.Calibration)
	_, err := s.db.ExecContext(ctx, `INSERT INTO learn_courses (`+courseCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		c.ID, c.Owner, c.Title, c.Topic, c.Goal, c.Background, c.VoiceID, c.Provider, c.Model, c.Status, string(cal), "{}", "", 0, c.CreatedAt, c.UpdatedAt, "", "", 0, "", b2i(c.Research))
	return err
}

func (s *store) course(ctx context.Context, id string) (*Course, error) {
	return scanCourse(s.db.QueryRowContext(ctx, `SELECT `+courseCols+` FROM learn_courses WHERE id=?`, id))
}

// coursesFor lists courses the user owns or is a member of.
func (s *store) coursesFor(ctx context.Context, user string) ([]*Course, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+courseCols+` FROM learn_courses
		WHERE owner=? OR id IN (SELECT course_id FROM learn_members WHERE user_id=?) ORDER BY updated_at DESC`, user, user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Course
	for rows.Next() {
		c, err := scanCourse(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *store) activeCourses(ctx context.Context) ([]*Course, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+courseCols+` FROM learn_courses WHERE status=? AND paused=0`, StatusActive)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Course
	for rows.Next() {
		c, err := scanCourse(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *store) updateCourse(ctx context.Context, id string, set map[string]any) error {
	cols, args := []string{}, []any{}
	for k, v := range set {
		switch k {
		case "title", "status", "error", "voice_id", "provider", "model", "paused", "session_id", "session_provider", "session_turns", "steer", "research":
		case "calibration", "profile":
			if b, ok := v.([]byte); ok {
				v = string(b)
			} else if _, ok := v.(string); !ok {
				b, _ := json.Marshal(v)
				v = string(b)
			}
		default:
			return fmt.Errorf("learn: can't update course field %q", k)
		}
		cols = append(cols, k+"=?")
		args = append(args, v)
	}
	cols = append(cols, "updated_at=?")
	args = append(args, now(), id)
	_, err := s.db.ExecContext(ctx, `UPDATE learn_courses SET `+strings.Join(cols, ",")+` WHERE id=?`, args...)
	return err
}

func (s *store) deleteCourse(ctx context.Context, id string) error {
	for _, t := range []string{"learn_members", "learn_nodes", "learn_segments", "learn_interactions", "learn_playback", "learn_jobs", "learn_notes", "learn_quizzes"} {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM `+t+` WHERE course_id=?`, id); err != nil {
			return err
		}
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM learn_courses WHERE id=?`, id)
	return err
}

// ---- members (listen-only) ----

func (s *store) isMember(ctx context.Context, course, user string) bool {
	var n int
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM learn_members WHERE course_id=? AND user_id=?`, course, user).Scan(&n)
	return n > 0
}

func (s *store) members(ctx context.Context, course string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT user_id FROM learn_members WHERE course_id=? ORDER BY added_at`, course)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *store) setMember(ctx context.Context, course, user string, on bool) error {
	if on {
		_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO learn_members (course_id, user_id, added_at) VALUES (?,?,?)`, course, user, now())
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM learn_members WHERE course_id=? AND user_id=?`, course, user)
	return err
}

// ---- nodes ----

const nodeCols = `id, course_id, parent_id, kind, ord, title, summary, status, mastery, correct_recalls, last_recall_at, session_id, session_provider, covered, last_covered_at`

func scanNode(row interface{ Scan(...any) error }) (*Node, error) {
	var n Node
	err := row.Scan(&n.ID, &n.CourseID, &n.ParentID, &n.Kind, &n.Ord, &n.Title, &n.Summary, &n.Status, &n.Mastery,
		&n.CorrectRecalls, &n.LastRecallAt, &n.SessionID, &n.SessionProv, &n.Covered, &n.LastCoveredAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	return &n, err
}

func (s *store) insertNode(ctx context.Context, n *Node) error {
	if n.ID == "" {
		n.ID = newID()
	}
	if n.Status == "" {
		n.Status = "pending"
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO learn_nodes (`+nodeCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		n.ID, n.CourseID, n.ParentID, n.Kind, n.Ord, n.Title, n.Summary, n.Status, n.Mastery, n.CorrectRecalls, n.LastRecallAt, n.SessionID, n.SessionProv, n.Covered, n.LastCoveredAt)
	return err
}

func (s *store) nodes(ctx context.Context, course string) ([]*Node, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+nodeCols+` FROM learn_nodes WHERE course_id=? ORDER BY ord`, course)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *store) node(ctx context.Context, id string) (*Node, error) {
	return scanNode(s.db.QueryRowContext(ctx, `SELECT `+nodeCols+` FROM learn_nodes WHERE id=?`, id))
}

func (s *store) updateNode(ctx context.Context, n *Node) error {
	_, err := s.db.ExecContext(ctx, `UPDATE learn_nodes SET status=?, mastery=?, correct_recalls=?, last_recall_at=?, session_id=?, session_provider=?, summary=?, covered=?, last_covered_at=? WHERE id=?`,
		n.Status, n.Mastery, n.CorrectRecalls, n.LastRecallAt, n.SessionID, n.SessionProv, n.Summary, n.Covered, n.LastCoveredAt, n.ID)
	return err
}

// ---- segments ----

// segColsQ: segCols qualified for joins.
var segColsQ = "s." + strings.ReplaceAll(segCols, ", ", ", s.")

const segCols = `id, course_id, ord, kind, node_id, title, script, speech, summary, checkin, concept_done, status, audio_path, duration_ms, words, align_score, voice_id, llm, interaction_id, created_at, played_at, pron`

func scanSeg(row interface{ Scan(...any) error }) (*Segment, error) {
	var g Segment
	var done int
	var words, pron string
	err := row.Scan(&g.ID, &g.CourseID, &g.Ord, &g.Kind, &g.NodeID, &g.Title, &g.Script, &g.Speech, &g.Summary, &g.Checkin, &done,
		&g.Status, &g.AudioPath, &g.DurationMs, &words, &g.AlignScore, &g.VoiceID, &g.LLM, &g.InteractionID, &g.CreatedAt, &g.PlayedAt, &pron)
	g.Pron = json.RawMessage(pron)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	g.ConceptDone = done != 0
	g.Words = json.RawMessage(words)
	return &g, err
}

func (s *store) insertSegment(ctx context.Context, g *Segment) error {
	if g.ID == "" {
		g.ID = newID()
	}
	g.CreatedAt = now()
	if len(g.Words) == 0 {
		g.Words = json.RawMessage("[]")
	}
	if len(g.Pron) == 0 {
		g.Pron = json.RawMessage("{}")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO learn_segments (`+segCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		g.ID, g.CourseID, g.Ord, g.Kind, g.NodeID, g.Title, g.Script, g.Speech, g.Summary, g.Checkin, b2i(g.ConceptDone), g.Status,
		g.AudioPath, g.DurationMs, string(g.Words), g.AlignScore, g.VoiceID, g.LLM, g.InteractionID, g.CreatedAt, g.PlayedAt, string(g.Pron))
	return err
}

func (s *store) segment(ctx context.Context, id string) (*Segment, error) {
	return scanSeg(s.db.QueryRowContext(ctx, `SELECT `+segCols+` FROM learn_segments WHERE id=?`, id))
}

// lastSegment: the furthest segment that still counts (not discarded or failed).
func (s *store) lastSegment(ctx context.Context, course string) (*Segment, error) {
	return scanSeg(s.db.QueryRowContext(ctx, `SELECT `+segCols+` FROM learn_segments WHERE course_id=? AND status NOT IN (?,?) ORDER BY ord DESC LIMIT 1`,
		course, SegDiscarded, SegFailed))
}

// segmentsAfter: segments past ord in order, by status filter (empty = all live ones).
func (s *store) segmentsAfter(ctx context.Context, course string, ord float64, limit int, statuses ...string) ([]*Segment, error) {
	if len(statuses) == 0 {
		statuses = []string{SegScripted, SegVoicing, SegReady}
	}
	q := `SELECT ` + segCols + ` FROM learn_segments WHERE course_id=? AND ord>? AND status IN (` + placeholders(len(statuses)) + `) ORDER BY ord LIMIT ?`
	args := []any{course, ord}
	for _, st := range statuses {
		args = append(args, st)
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Segment
	for rows.Next() {
		g, err := scanSeg(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// recentSegments: the last n live segments up to and including ord, oldest first.
func (s *store) recentSegments(ctx context.Context, course string, ord float64, n int) ([]*Segment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+segCols+` FROM learn_segments WHERE course_id=? AND ord<=? AND status NOT IN (?,?) ORDER BY ord DESC LIMIT ?`,
		course, ord, SegDiscarded, SegFailed, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Segment
	for rows.Next() {
		g, err := scanSeg(rows)
		if err != nil {
			return nil, err
		}
		out = append([]*Segment{g}, out...)
	}
	return out, rows.Err()
}

func (s *store) updateSegment(ctx context.Context, g *Segment) error {
	_, err := s.db.ExecContext(ctx, `UPDATE learn_segments SET status=?, audio_path=?, duration_ms=?, words=?, align_score=?, voice_id=?, played_at=? WHERE id=?`,
		g.Status, g.AudioPath, g.DurationMs, string(g.Words), g.AlignScore, g.VoiceID, g.PlayedAt, g.ID)
	return err
}

func (s *store) setSegmentStatus(ctx context.Context, id, status string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE learn_segments SET status=? WHERE id=?`, status, id)
	return err
}

// discardAfter drops unplayed live segments after ord; returns how many.
func (s *store) discardAfter(ctx context.Context, course string, ord float64, keep string) (int64, error) {
	r, err := s.db.ExecContext(ctx, `UPDATE learn_segments SET status=? WHERE course_id=? AND ord>? AND id<>? AND played_at='' AND status IN (?,?,?)`,
		SegDiscarded, course, ord, keep, SegScripted, SegVoicing, SegReady)
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}

// secondsAhead: audio ready after ord plus an estimate for scripted segments still to be voiced.
func (s *store) secondsAhead(ctx context.Context, course string, ord float64) (ready, pending float64) {
	var ms sql.NullInt64
	var n int
	_ = s.db.QueryRowContext(ctx, `SELECT SUM(duration_ms) FROM learn_segments WHERE course_id=? AND ord>? AND status=?`, course, ord, SegReady).Scan(&ms)
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM learn_segments WHERE course_id=? AND ord>? AND status IN (?,?)`, course, ord, SegScripted, SegVoicing).Scan(&n)
	return float64(ms.Int64) / 1000, float64(n) * 180
}

func (s *store) segmentCounts(ctx context.Context, course string) (ready int, readyMs int64, played int) {
	var ms sql.NullInt64
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*), SUM(duration_ms) FROM learn_segments WHERE course_id=? AND status=?`, course, SegReady).Scan(&ready, &ms)
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM learn_segments WHERE course_id=? AND played_at<>''`, course).Scan(&played)
	return ready, ms.Int64, played
}

// ---- interactions ----

func (s *store) insertInteraction(ctx context.Context, i *Interaction) error {
	i.ID, i.CreatedAt = newID(), now()
	_, err := s.db.ExecContext(ctx, `INSERT INTO learn_interactions (id, course_id, user_id, kind, segment_id, offset_ms, text, status, answer_segment_id, verdict, created_at, node_id)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, i.ID, i.CourseID, i.UserID, i.Kind, i.SegmentID, i.OffsetMs, i.Text, i.Status, i.AnswerSegmentID, i.Verdict, i.CreatedAt, i.NodeID)
	return err
}

func (s *store) interaction(ctx context.Context, id string) (*Interaction, error) {
	var i Interaction
	err := s.db.QueryRowContext(ctx, `SELECT id, course_id, user_id, kind, segment_id, offset_ms, text, status, answer_segment_id, verdict, created_at, node_id
		FROM learn_interactions WHERE id=?`, id).Scan(&i.ID, &i.CourseID, &i.UserID, &i.Kind, &i.SegmentID, &i.OffsetMs, &i.Text, &i.Status, &i.AnswerSegmentID, &i.Verdict, &i.CreatedAt, &i.NodeID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	return &i, err
}

func (s *store) interactions(ctx context.Context, course string, limit int) ([]*Interaction, error) {
	return s.interactionsWhere(ctx, course, "", limit)
}

// interactionsOn: past questions about any of these concepts, newest first.
func (s *store) interactionsOn(ctx context.Context, course string, nodes []string, limit int) ([]*Interaction, error) {
	if len(nodes) == 0 {
		return nil, nil
	}
	return s.interactionsWhere(ctx, course, "AND node_id IN ("+quoteIDs(nodes)+")", limit)
}

func (s *store) interactionsWhere(ctx context.Context, course, where string, limit int) ([]*Interaction, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, course_id, user_id, kind, segment_id, offset_ms, text, status, answer_segment_id, verdict, created_at, node_id
		FROM learn_interactions WHERE course_id=? `+where+` ORDER BY created_at DESC LIMIT ?`, course, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Interaction
	for rows.Next() {
		var i Interaction
		if err := rows.Scan(&i.ID, &i.CourseID, &i.UserID, &i.Kind, &i.SegmentID, &i.OffsetMs, &i.Text, &i.Status, &i.AnswerSegmentID, &i.Verdict, &i.CreatedAt, &i.NodeID); err != nil {
			return nil, err
		}
		out = append(out, &i)
	}
	return out, rows.Err()
}

func (s *store) finishInteraction(ctx context.Context, id, status, answerSeg, verdict, node string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE learn_interactions SET status=?, answer_segment_id=?, verdict=?, node_id=CASE WHEN ?<>'' THEN ? ELSE node_id END WHERE id=?`,
		status, answerSeg, verdict, node, node, id)
	return err
}

// ---- notes: what the course has learned about the learner ----

type Note struct {
	ID        string `json:"id"`
	CourseID  string `json:"-"`
	NodeID    string `json:"node_id"`
	Kind      string `json:"kind"` // misconception | gap | strength | interest | preference
	Text      string `json:"text"`
	Source    string `json:"source"`
	Resolved  bool   `json:"resolved"`
	CreatedAt string `json:"created_at"`
}

func (s *store) addNote(ctx context.Context, n *Note) error {
	n.ID, n.CreatedAt = newID(), now()
	_, err := s.db.ExecContext(ctx, `INSERT INTO learn_notes (id, course_id, node_id, kind, text, source, resolved, created_at) VALUES (?,?,?,?,?,?,?,?)`,
		n.ID, n.CourseID, n.NodeID, n.Kind, n.Text, n.Source, b2i(n.Resolved), n.CreatedAt)
	return err
}

// notes: open notes for the course; with nodes, only those tagged to them (plus untagged ones).
func (s *store) notes(ctx context.Context, course string, nodes []string, includeResolved bool, limit int) ([]*Note, error) {
	q := `SELECT id, course_id, node_id, kind, text, source, resolved, created_at FROM learn_notes WHERE course_id=?`
	if !includeResolved {
		q += ` AND resolved=0`
	}
	if nodes != nil {
		q += ` AND (node_id='' OR node_id IN (` + quoteIDs(nodes) + `))`
	}
	q += ` ORDER BY created_at DESC LIMIT ?`
	rows, err := s.db.QueryContext(ctx, q, course, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Note
	for rows.Next() {
		var n Note
		var res int
		if err := rows.Scan(&n.ID, &n.CourseID, &n.NodeID, &n.Kind, &n.Text, &n.Source, &res, &n.CreatedAt); err != nil {
			return nil, err
		}
		n.Resolved = res != 0
		out = append(out, &n)
	}
	return out, rows.Err()
}

func (s *store) resolveNotes(ctx context.Context, course, node, kind string) {
	_, _ = s.db.ExecContext(ctx, `UPDATE learn_notes SET resolved=1 WHERE course_id=? AND node_id=? AND kind=?`, course, node, kind)
}

func (s *store) setNote(ctx context.Context, course, id string, resolved, del bool) error {
	if del {
		_, err := s.db.ExecContext(ctx, `DELETE FROM learn_notes WHERE course_id=? AND id=?`, course, id)
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE learn_notes SET resolved=? WHERE course_id=? AND id=?`, b2i(resolved), course, id)
	return err
}

// ---- quizzes ----

type Quiz struct {
	ID        string          `json:"id"`
	CourseID  string          `json:"-"`
	NodeID    string          `json:"node_id"`
	Status    string          `json:"status"` // writing | ready | grading | done | failed
	Questions json.RawMessage `json:"questions"`
	Answers   json.RawMessage `json:"answers"`
	Results   json.RawMessage `json:"results"`
	Score     float64         `json:"score"`
	Error     string          `json:"error"`
	CreatedAt string          `json:"created_at"`
}

func (s *store) addQuiz(ctx context.Context, q *Quiz) error {
	q.ID, q.CreatedAt = newID(), now()
	_, err := s.db.ExecContext(ctx, `INSERT INTO learn_quizzes (id, course_id, node_id, status, created_at, updated_at) VALUES (?,?,?,?,?,?)`,
		q.ID, q.CourseID, q.NodeID, q.Status, q.CreatedAt, q.CreatedAt)
	return err
}

func (s *store) quiz(ctx context.Context, id string) (*Quiz, error) {
	var q Quiz
	var qs, as, rs string
	err := s.db.QueryRowContext(ctx, `SELECT id, course_id, node_id, status, questions, answers, results, score, error, created_at FROM learn_quizzes WHERE id=?`, id).
		Scan(&q.ID, &q.CourseID, &q.NodeID, &q.Status, &qs, &as, &rs, &q.Score, &q.Error, &q.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	q.Questions, q.Answers, q.Results = json.RawMessage(qs), json.RawMessage(as), json.RawMessage(rs)
	return &q, err
}

func (s *store) quizzes(ctx context.Context, course string, limit int) ([]*Quiz, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM learn_quizzes WHERE course_id=? ORDER BY created_at DESC LIMIT ?`, course, limit)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	var out []*Quiz
	for _, id := range ids {
		if q, err := s.quiz(ctx, id); err == nil {
			out = append(out, q)
		}
	}
	return out, nil
}

func (s *store) updateQuiz(ctx context.Context, q *Quiz) error {
	_, err := s.db.ExecContext(ctx, `UPDATE learn_quizzes SET status=?, questions=?, answers=?, results=?, score=?, error=?, updated_at=? WHERE id=?`,
		q.Status, string(orEmpty(q.Questions)), string(orEmpty(q.Answers)), string(orEmpty(q.Results)), q.Score, q.Error, now(), q.ID)
	return err
}

func orEmpty(b json.RawMessage) json.RawMessage {
	if len(b) == 0 {
		return json.RawMessage("[]")
	}
	return b
}

// quoteIDs renders hex ids for an IN list; ids are generated here (hex), never user text.
func quoteIDs(ids []string) string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		ok := id != ""
		for _, r := range id {
			if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
				ok = false
			}
		}
		if ok {
			out = append(out, "'"+id+"'")
		}
	}
	if len(out) == 0 {
		return "''"
	}
	return strings.Join(out, ",")
}

// ---- playback ----

func (s *store) playback(ctx context.Context, user, course string) (*Playback, error) {
	var p Playback
	err := s.db.QueryRowContext(ctx, `SELECT segment_id, offset_ms, rate, updated_at, device FROM learn_playback WHERE user_id=? AND course_id=?`, user, course).
		Scan(&p.SegmentID, &p.OffsetMs, &p.Rate, &p.UpdatedAt, &p.Device)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	return &p, err
}

func (s *store) savePlayback(ctx context.Context, user, course string, p Playback) (string, error) {
	at := now()
	_, err := s.db.ExecContext(ctx, `INSERT INTO learn_playback (user_id, course_id, segment_id, offset_ms, rate, updated_at, device) VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(user_id, course_id) DO UPDATE SET segment_id=excluded.segment_id, offset_ms=excluded.offset_ms, rate=excluded.rate,
		updated_at=excluded.updated_at, device=excluded.device`,
		user, course, p.SegmentID, p.OffsetMs, p.Rate, at, p.Device)
	return at, err
}

// ownerPosition: the ord the owner is listening at (0 before the first segment).
func (s *store) ownerPosition(ctx context.Context, c *Course) (float64, string) {
	var ord float64
	var updated string
	err := s.db.QueryRowContext(ctx, `SELECT s.ord, p.updated_at FROM learn_playback p JOIN learn_segments s ON s.id=p.segment_id
		WHERE p.user_id=? AND p.course_id=?`, c.Owner, c.ID).Scan(&ord, &updated)
	if err != nil {
		return 0, ""
	}
	return ord, updated
}

// ---- jobs ----

func (s *store) enqueue(ctx context.Context, course, kind string, priority int, payload any) error {
	b, _ := json.Marshal(payload)
	if payload == nil {
		b = []byte("{}")
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO learn_jobs (course_id, kind, priority, payload, status, created_at, updated_at) VALUES (?,?,?,?,?,?,?)`,
		course, kind, priority, string(b), "queued", now(), now())
	return err
}

func (s *store) hasJob(ctx context.Context, course string, kinds ...string) bool {
	args := []any{course}
	for _, k := range kinds {
		args = append(args, k)
	}
	var n int
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM learn_jobs WHERE course_id=? AND status IN ('queued','running') AND kind IN (`+placeholders(len(kinds))+`)`, args...).Scan(&n)
	return n > 0
}

// claim takes the highest-priority queued job of the given kinds.
func (s *store) claim(ctx context.Context, kinds []string) (*Job, error) {
	args := []any{}
	for _, k := range kinds {
		args = append(args, k)
	}
	var j Job
	var payload string
	err := s.db.QueryRowContext(ctx, `SELECT id, course_id, kind, priority, payload, attempts FROM learn_jobs
		WHERE status='queued' AND kind IN (`+placeholders(len(kinds))+`) ORDER BY priority DESC, id LIMIT 1`, args...).
		Scan(&j.ID, &j.CourseID, &j.Kind, &j.Priority, &payload, &j.Attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	j.Payload = json.RawMessage(payload)
	r, err := s.db.ExecContext(ctx, `UPDATE learn_jobs SET status='running', attempts=attempts+1, updated_at=? WHERE id=? AND status='queued'`, now(), j.ID)
	if err != nil {
		return nil, err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return nil, nil
	}
	j.Attempts++
	return &j, nil
}

func (s *store) finishJob(ctx context.Context, id int64, err error, retry bool) {
	status, msg := "done", ""
	if err != nil {
		msg = err.Error()
		status = "failed"
		if retry {
			status = "queued"
		}
	}
	_, _ = s.db.ExecContext(ctx, `UPDATE learn_jobs SET status=?, error=?, updated_at=? WHERE id=?`, status, msg, now(), id)
}

func (s *store) jobSummary(ctx context.Context, course string) map[string]int {
	out := map[string]int{}
	rows, err := s.db.QueryContext(ctx, `SELECT kind, COUNT(*) FROM learn_jobs WHERE course_id=? AND status IN ('queued','running') GROUP BY kind`, course)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		var n int
		if rows.Scan(&k, &n) == nil {
			out[k] = n
		}
	}
	return out
}

func (s *store) pruneJobs(ctx context.Context) {
	_, _ = s.db.ExecContext(ctx, `DELETE FROM learn_jobs WHERE status IN ('done','failed') AND updated_at < ?`, time.Now().Add(-7*24*time.Hour).UTC().Format(time.RFC3339Nano))
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// rebuild clears the map and everything not yet heard, ready for a fresh plan.
func (s *store) rebuild(ctx context.Context, c *Course) (int64, error) {
	pos, _ := s.ownerPosition(ctx, c)
	n, err := s.discardAfter(ctx, c.ID, pos, "")
	if err != nil {
		return 0, err
	}
	for _, q := range []string{
		`DELETE FROM learn_nodes WHERE course_id=?`,
		`DELETE FROM learn_jobs WHERE course_id=? AND status IN ('queued','running') AND kind IN ('script','extend','plan','voice')`,
		`UPDATE learn_courses SET status='planning', session_id='', session_provider='', session_turns=0, steer='', error='' WHERE id=?`,
	} {
		if _, err := s.db.ExecContext(ctx, q, c.ID); err != nil {
			return n, err
		}
	}
	return n, nil
}

// ---- pronunciation lexicon: how the owner wants words said, across all their courses ----

type LexEntry struct {
	Term      string `json:"term"`
	Say       string `json:"say"`
	UpdatedAt string `json:"updated_at"`
}

func (s *store) lexicon(ctx context.Context, owner string) ([]LexEntry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT term, say, updated_at FROM learn_lexicon WHERE owner=? ORDER BY term COLLATE NOCASE`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LexEntry{}
	for rows.Next() {
		var l LexEntry
		if err := rows.Scan(&l.Term, &l.Say, &l.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s *store) setLexicon(ctx context.Context, owner, term, say string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO learn_lexicon (owner, term, say, updated_at) VALUES (?,?,?,?)
		ON CONFLICT(owner, term) DO UPDATE SET say=excluded.say, updated_at=excluded.updated_at`, owner, term, say, now())
	return err
}

func (s *store) deleteLexicon(ctx context.Context, owner, term string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM learn_lexicon WHERE owner=? AND term=?`, owner, term)
	return err
}

// unheardWith: voiced, unheard segments in the owner's courses whose text contains term (case-insensitive).
func (s *store) unheardWith(ctx context.Context, owner, term string) ([]*Segment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+segColsQ+` FROM learn_segments s JOIN learn_courses c ON c.id=s.course_id
		WHERE c.owner=? AND s.status=? AND s.played_at='' AND instr(lower(s.script), lower(?))>0`, owner, SegReady, term)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Segment
	for rows.Next() {
		g, err := scanSeg(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// contents: every segment that counts, in order, without the heavy fields.
func (s *store) contents(ctx context.Context, course string) ([]map[string]any, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, ord, kind, node_id, title, status, played_at, duration_ms, checkin<>'' FROM learn_segments
		WHERE course_id=? AND status NOT IN (?,?) ORDER BY ord`, course, SegDiscarded, SegFailed)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, kind, node, title, status, played string
		var ord float64
		var ms int64
		var chk bool
		if err := rows.Scan(&id, &ord, &kind, &node, &title, &status, &played, &ms, &chk); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"id": id, "ord": ord, "kind": kind, "node_id": node, "title": title, "status": status,
			"played": played != "", "duration_ms": ms, "checkin": chk})
	}
	return out, rows.Err()
}
