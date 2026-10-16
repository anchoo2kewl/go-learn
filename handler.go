package learn

import (
	"context"
	"embed"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/anchoo2kewl/go-learn/agent"
	"github.com/anchoo2kewl/go-learn/voice"
)

//go:embed static
var staticFS embed.FS

// Handler serves the player and its API. Mount it under any prefix with http.StripPrefix; every URL
// the page uses is relative.
func (e *Engine) Handler() http.Handler {
	mux := http.NewServeMux()
	sub, _ := fs.Sub(staticFS, "static")
	files := http.FileServerFS(sub)
	mux.Handle("GET /static/", http.StripPrefix("/static/", files))
	mux.HandleFunc("GET /manifest.webmanifest", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/manifest+json")
		http.ServeFileFS(w, r, sub, "manifest.webmanifest")
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFileFS(w, r, sub, "index.html")
	})

	mux.HandleFunc("GET /api/me", e.authed(e.me))
	mux.HandleFunc("GET /api/voices", e.authed(e.listVoices))
	mux.HandleFunc("GET /api/voices/{voice}/preview", e.authed(e.voicePreview))
	mux.HandleFunc("GET /api/courses", e.authed(e.listCourses))
	mux.HandleFunc("POST /api/courses", e.authed(e.createCourse))
	mux.HandleFunc("GET /api/courses/{id}", e.course(false, e.getCourse))
	mux.HandleFunc("PATCH /api/courses/{id}", e.course(true, e.patchCourse))
	mux.HandleFunc("DELETE /api/courses/{id}", e.course(true, e.removeCourse))
	mux.HandleFunc("POST /api/courses/{id}/calibration", e.course(true, e.answerCalibration))
	mux.HandleFunc("POST /api/courses/{id}/retry", e.course(true, e.retryCourse))
	mux.HandleFunc("POST /api/courses/{id}/rebuild", e.course(true, e.rebuildCourse))
	mux.HandleFunc("GET /api/courses/{id}/queue", e.course(false, e.queue))
	mux.HandleFunc("GET /api/courses/{id}/segments/{sid}", e.course(false, e.getSegment))
	mux.HandleFunc("GET /api/courses/{id}/audio/{sid}", e.course(false, e.audio))
	mux.HandleFunc("PUT /api/courses/{id}/playback", e.course(false, e.putPlayback))
	mux.HandleFunc("GET /api/courses/{id}/playback", e.course(false, e.getPlayback))
	mux.HandleFunc("POST /api/courses/{id}/ask", e.course(true, e.ask))
	mux.HandleFunc("POST /api/courses/{id}/transcribe", e.course(true, e.transcribe))
	mux.HandleFunc("GET /api/courses/{id}/interactions", e.course(false, e.listInteractions))
	mux.HandleFunc("POST /api/courses/{id}/members", e.course(true, e.setMember))
	mux.HandleFunc("POST /api/courses/{id}/focus", e.course(true, e.setFocus))
	mux.HandleFunc("GET /api/courses/{id}/notes", e.course(true, e.listNotes))
	mux.HandleFunc("PATCH /api/courses/{id}/notes/{nid}", e.course(true, e.patchNote))
	mux.HandleFunc("GET /api/courses/{id}/quizzes", e.course(true, e.listQuizzes))
	mux.HandleFunc("POST /api/courses/{id}/quizzes", e.course(true, e.newQuiz))
	mux.HandleFunc("GET /api/courses/{id}/quizzes/{qid}", e.course(true, e.getQuiz))
	mux.HandleFunc("POST /api/courses/{id}/quizzes/{qid}/answers", e.course(true, e.answerQuiz))
	return mux
}

type ctxKey int

const (
	userKey ctxKey = iota
	courseKey
	steerKey
)

func (e *Engine) authed(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, ok := e.opt.identity(r)
		if !ok || u == "" {
			jsonErr(w, http.StatusUnauthorized, "sign in to use Learn")
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), userKey, u)))
	}
}

// course loads {id} and checks access: the owner can do everything; members can listen only.
func (e *Engine) course(steer bool, h http.HandlerFunc) http.HandlerFunc {
	return e.authed(func(w http.ResponseWriter, r *http.Request) {
		u := user(r)
		c, err := e.st.course(r.Context(), r.PathValue("id"))
		if err != nil {
			jsonErr(w, http.StatusNotFound, "no such course")
			return
		}
		owner := c.Owner == u
		if !owner && !e.st.isMember(r.Context(), c.ID, u) {
			jsonErr(w, http.StatusNotFound, "no such course")
			return
		}
		if steer && !owner {
			jsonErr(w, http.StatusForbidden, "only the course owner can change it; you can listen")
			return
		}
		ctx := context.WithValue(r.Context(), courseKey, c)
		ctx = context.WithValue(ctx, steerKey, owner)
		h(w, r.WithContext(ctx))
	})
}

func user(r *http.Request) string         { s, _ := r.Context().Value(userKey).(string); return s }
func courseOf(r *http.Request) *Course    { c, _ := r.Context().Value(courseKey).(*Course); return c }
func canSteer(r *http.Request) bool       { b, _ := r.Context().Value(steerKey).(bool); return b }
func jsonOK(w http.ResponseWriter, v any) { writeJSON(w, http.StatusOK, v) }

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func jsonErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func decode(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v)
}

func (e *Engine) me(w http.ResponseWriter, r *http.Request) {
	var installed []string
	for _, p := range agent.Providers {
		if _, err := exec.LookPath(p); err == nil {
			installed = append(installed, p)
		}
	}
	jsonOK(w, map[string]any{"user": user(r), "title": e.opt.title, "providers": installed, "research_dirs": e.opt.researchDirs})
}

func (e *Engine) voiceList(ctx context.Context) []voice.Voice {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.voices) == 0 && e.opt.speaker != nil {
		if v, err := e.opt.speaker.Voices(ctx); err == nil {
			e.voices = v
		}
	}
	return e.voices
}

func (e *Engine) listVoices(w http.ResponseWriter, r *http.Request) {
	jsonOK(w, e.voiceList(r.Context()))
}

func (e *Engine) voicePreview(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("voice")
	known := false
	for _, v := range e.voiceList(r.Context()) {
		known = known || v.ID == id
	}
	if !known || e.opt.speaker == nil {
		jsonErr(w, http.StatusNotFound, "no such voice")
		return
	}
	dir := filepath.Join(e.opt.dataDir, "previews")
	_ = os.MkdirAll(dir, 0o755)
	m4a := filepath.Join(dir, id+".m4a")
	if _, err := os.Stat(m4a); err != nil {
		wav := filepath.Join(dir, id+".wav")
		if err := e.opt.speaker.Preview(r.Context(), id, wav); err != nil {
			jsonErr(w, http.StatusBadGateway, err.Error())
			return
		}
		defer os.Remove(wav)
		if out, err := exec.CommandContext(r.Context(), e.opt.ffmpeg, "-y", "-loglevel", "error", "-i", wav, "-af", "loudnorm=I=-16:TP=-1.5:LRA=11",
			"-ac", "1", "-c:a", "aac", "-b:a", "64k", "-movflags", "+faststart", m4a).CombinedOutput(); err != nil {
			jsonErr(w, http.StatusInternalServerError, "ffmpeg: "+string(out))
			return
		}
	}
	w.Header().Set("Content-Type", "audio/mp4")
	http.ServeFile(w, r, m4a)
}

type courseCard struct {
	*Course
	Steer    bool    `json:"can_steer"`
	ReadyS   float64 `json:"ready_s"`
	Segments int     `json:"segments"`
	Played   int     `json:"played"`
}

func (e *Engine) listCourses(w http.ResponseWriter, r *http.Request) {
	cs, err := e.st.coursesFor(r.Context(), user(r))
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := []courseCard{}
	for _, c := range cs {
		n, ms, played := e.st.segmentCounts(r.Context(), c.ID)
		c.Profile = nil
		out = append(out, courseCard{Course: c, Steer: c.Owner == user(r), ReadyS: float64(ms) / 1000, Segments: n, Played: played})
	}
	jsonOK(w, out)
}

func validProvider(p string) bool {
	for _, x := range agent.Providers {
		if x == p {
			return true
		}
	}
	return false
}

func (e *Engine) createCourse(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Topic           string `json:"topic"`
		Goal            string `json:"goal"`
		Background      string `json:"background"`
		VoiceID         string `json:"voice_id"`
		Provider        string `json:"provider"`
		Model           string `json:"model"`
		SkipCalibration bool   `json:"skip_calibration"`
		Research        *bool  `json:"research"`
	}
	if err := decode(r, &in); err != nil {
		jsonErr(w, http.StatusBadRequest, "send JSON: "+err.Error())
		return
	}
	in.Topic = strings.TrimSpace(in.Topic)
	if in.Topic == "" {
		jsonErr(w, http.StatusBadRequest, "say what you want to learn")
		return
	}
	if in.Provider == "" {
		in.Provider = "claude"
	}
	if !validProvider(in.Provider) {
		jsonErr(w, http.StatusBadRequest, "unknown model provider")
		return
	}
	if _, err := exec.LookPath(in.Provider); err != nil {
		jsonErr(w, http.StatusBadRequest, in.Provider+" isn't installed on this Mac")
		return
	}
	if in.VoiceID == "" {
		if vs := e.voiceList(r.Context()); len(vs) > 0 {
			in.VoiceID = vs[0].ID
		}
	}
	c := &Course{Owner: user(r), Topic: in.Topic, Goal: strings.TrimSpace(in.Goal), Background: strings.TrimSpace(in.Background),
		VoiceID: in.VoiceID, Provider: in.Provider, Model: strings.TrimSpace(in.Model), Status: StatusCalibrating,
		Research: in.Research == nil || *in.Research}
	kind := "calibrate"
	if in.SkipCalibration {
		c.Status, kind = StatusPlanning, "plan"
		c.Calibration.Skipped = true
	}
	if err := e.st.createCourse(r.Context(), c); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := e.st.enqueue(r.Context(), c.ID, kind, 60, nil); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	e.poke()
	e.log.Info().Str("course", c.ID).Str("topic", c.Topic).Str("provider", c.Provider).Msg("course created")
	writeJSON(w, http.StatusCreated, c)
}

func (e *Engine) getCourse(w http.ResponseWriter, r *http.Request) {
	c := courseOf(r)
	ctx := r.Context()
	nodes, _ := e.st.nodes(ctx, c.ID)
	pb, _ := e.st.playback(ctx, user(r), c.ID)
	pos, _ := e.st.ownerPosition(ctx, c)
	if pb != nil {
		if g, err := e.st.segment(ctx, pb.SegmentID); err == nil {
			pos = g.Ord
		}
	}
	ready, pending := e.st.secondsAhead(ctx, c.ID, pos)
	n, ms, played := e.st.segmentCounts(ctx, c.ID)
	members, _ := e.st.members(ctx, c.ID)
	if !canSteer(r) {
		c.Profile = nil
		members = nil
	}
	jsonOK(w, map[string]any{
		"course": c, "nodes": nodes, "playback": pb, "can_steer": canSteer(r), "members": members,
		"stats": map[string]any{"ahead_s": ready, "pending_s": pending, "segments": n, "ready_total_s": float64(ms) / 1000, "played": played,
			"jobs": e.st.jobSummary(ctx, c.ID), "voice_ok": e.opt.speaker != nil},
	})
}

func (e *Engine) patchCourse(w http.ResponseWriter, r *http.Request) {
	c := courseOf(r)
	var in map[string]json.RawMessage
	if err := decode(r, &in); err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	set := map[string]any{}
	for _, k := range []string{"title", "voice_id", "provider", "model"} {
		if raw, ok := in[k]; ok {
			var s string
			if json.Unmarshal(raw, &s) == nil {
				s = strings.TrimSpace(s)
				if k == "provider" && !validProvider(s) {
					jsonErr(w, http.StatusBadRequest, "unknown model provider")
					return
				}
				set[k] = s
			}
		}
	}
	for _, k := range []string{"paused", "research"} {
		if raw, ok := in[k]; ok {
			var b bool
			if json.Unmarshal(raw, &b) == nil {
				set[k] = b2i(b)
			}
		}
	}
	if len(set) == 0 {
		jsonErr(w, http.StatusBadRequest, "nothing to change")
		return
	}
	if err := e.st.updateCourse(r.Context(), c.ID, set); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	e.poke()
	c, _ = e.st.course(r.Context(), c.ID)
	jsonOK(w, c)
}

func (e *Engine) removeCourse(w http.ResponseWriter, r *http.Request) {
	c := courseOf(r)
	if err := e.st.deleteCourse(r.Context(), c.ID); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = os.RemoveAll(filepath.Join(e.opt.dataDir, "audio", c.ID))
	_ = os.RemoveAll(filepath.Join(e.opt.dataDir, "scratch", c.ID))
	jsonOK(w, map[string]bool{"deleted": true})
}

func (e *Engine) answerCalibration(w http.ResponseWriter, r *http.Request) {
	c := courseOf(r)
	var in struct {
		Answers []string `json:"answers"`
		Skip    bool     `json:"skip"`
	}
	if err := decode(r, &in); err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if c.Status != StatusAwaiting {
		jsonErr(w, http.StatusConflict, "this course isn't waiting for answers")
		return
	}
	cal := c.Calibration
	for k := range cal.Questions {
		if k < len(in.Answers) {
			cal.Questions[k].Answer = strings.TrimSpace(in.Answers[k])
		}
	}
	cal.Skipped = in.Skip
	if err := e.st.updateCourse(r.Context(), c.ID, map[string]any{"calibration": cal, "status": StatusPlanning}); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = e.st.enqueue(r.Context(), c.ID, "plan", 60, nil)
	e.poke()
	jsonOK(w, map[string]string{"status": StatusPlanning})
}

func (e *Engine) retryCourse(w http.ResponseWriter, r *http.Request) {
	c := courseOf(r)
	if c.Status != StatusError {
		jsonOK(w, map[string]string{"status": c.Status})
		return
	}
	status, kind := StatusCalibrating, "calibrate"
	if len(c.Calibration.Questions) > 0 || c.Calibration.Skipped {
		status, kind = StatusPlanning, "plan"
	}
	_ = e.st.updateCourse(r.Context(), c.ID, map[string]any{"status": status, "error": ""})
	_ = e.st.enqueue(r.Context(), c.ID, kind, 60, nil)
	e.poke()
	jsonOK(w, map[string]string{"status": status})
}

// queue returns ready segments after the given one (or from the start), with words for read-along.
func (e *Engine) queue(w http.ResponseWriter, r *http.Request) {
	c := courseOf(r)
	ctx := r.Context()
	ord := 0.0
	if after := r.URL.Query().Get("after"); after != "" {
		if g, err := e.st.segment(ctx, after); err == nil && g.CourseID == c.ID {
			ord = g.Ord
		}
	}
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))
	if n <= 0 || n > 10 {
		n = 3
	}
	// Ready ones only, in order, stopping at the first gap so nothing is skipped while it's being voiced.
	all, err := e.st.segmentsAfter(ctx, c.ID, ord, n+5)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := []*Segment{}
	waiting := false
	for _, g := range all {
		if g.Status != SegReady {
			waiting = true
			break
		}
		if len(out) < n {
			out = append(out, g)
		}
	}
	generating := waiting || e.st.hasJob(ctx, c.ID, "script", "voice", "answer", "plan", "extend")
	jsonOK(w, map[string]any{"segments": out, "generating": generating})
}

func (e *Engine) getSegment(w http.ResponseWriter, r *http.Request) {
	g, err := e.st.segment(r.Context(), r.PathValue("sid"))
	if err != nil || g.CourseID != courseOf(r).ID {
		jsonErr(w, http.StatusNotFound, "no such segment")
		return
	}
	jsonOK(w, g)
}

func (e *Engine) audio(w http.ResponseWriter, r *http.Request) {
	g, err := e.st.segment(r.Context(), r.PathValue("sid"))
	if err != nil || g.CourseID != courseOf(r).ID || g.AudioPath == "" {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(g.AudioPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	w.Header().Set("Content-Type", "audio/mp4")
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	http.ServeContent(w, r, g.ID+".m4a", st.ModTime(), f) // Range requests for seeking
}

func (e *Engine) putPlayback(w http.ResponseWriter, r *http.Request) {
	c := courseOf(r)
	var in struct {
		SegmentID string  `json:"segment_id"`
		OffsetMs  int64   `json:"offset_ms"`
		Rate      float64 `json:"rate"`
		Finished  string  `json:"finished"` // a segment that just played to the end
		Device    string  `json:"device"`   // which phone, tablet or laptop, for handoff
	}
	if err := decode(r, &in); err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx := r.Context()
	g, err := e.st.segment(ctx, in.SegmentID)
	if err != nil || g.CourseID != c.ID {
		jsonErr(w, http.StatusBadRequest, "unknown segment")
		return
	}
	if in.Rate <= 0 {
		in.Rate = 1
	}
	if len(in.Device) > 80 {
		in.Device = in.Device[:80]
	}
	at, err := e.st.savePlayback(ctx, user(r), c.ID, Playback{SegmentID: g.ID, OffsetMs: in.OffsetMs, Rate: in.Rate, Device: in.Device})
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if in.Finished != "" && canSteer(r) {
		if f, err := e.st.segment(ctx, in.Finished); err == nil && f.CourseID == c.ID && f.PlayedAt == "" {
			f.PlayedAt = now()
			_ = e.st.updateSegment(ctx, f)
		}
	}
	if canSteer(r) {
		e.poke()
	}
	jsonOK(w, map[string]any{"ok": true, "updated_at": at})
}

// getPlayback: where this user is in the course, and on which device, so another device can pick up.
func (e *Engine) getPlayback(w http.ResponseWriter, r *http.Request) {
	pb, err := e.st.playback(r.Context(), user(r), courseOf(r).ID)
	if err != nil {
		jsonOK(w, nil)
		return
	}
	jsonOK(w, pb)
}

func (e *Engine) ask(w http.ResponseWriter, r *http.Request) {
	c := courseOf(r)
	var in struct {
		Text      string `json:"text"`
		Kind      string `json:"kind"`
		SegmentID string `json:"segment_id"`
		OffsetMs  int64  `json:"offset_ms"`
	}
	if err := decode(r, &in); err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	in.Text = strings.TrimSpace(in.Text)
	if in.Text == "" {
		jsonErr(w, http.StatusBadRequest, "say or type something first")
		return
	}
	if len(in.Text) > 4000 {
		in.Text = in.Text[:4000]
	}
	if in.Kind != "checkin" {
		in.Kind = "ask"
	}
	if c.Status != StatusActive {
		jsonErr(w, http.StatusConflict, "the course is still being set up; ask again once it's playing")
		return
	}
	i := &Interaction{CourseID: c.ID, UserID: user(r), Kind: in.Kind, SegmentID: in.SegmentID, OffsetMs: in.OffsetMs, Text: in.Text, Status: "pending"}
	if err := e.st.insertInteraction(r.Context(), i); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := e.st.enqueue(r.Context(), c.ID, "answer", 90, map[string]string{"interaction_id": i.ID}); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	e.poke()
	writeJSON(w, http.StatusAccepted, i)
}

func (e *Engine) transcribe(w http.ResponseWriter, r *http.Request) {
	if e.opt.transcriber == nil {
		jsonErr(w, http.StatusServiceUnavailable, "voice input isn't available")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 25<<20)
	f, _, err := r.FormFile("audio")
	if err != nil {
		jsonErr(w, http.StatusBadRequest, "send the recording as form field 'audio'")
		return
	}
	defer f.Close()
	dir := filepath.Join(e.opt.dataDir, "inbox")
	_ = os.MkdirAll(dir, 0o755)
	id := newID()
	raw, wav := filepath.Join(dir, id+".rec"), filepath.Join(dir, id+".wav")
	defer os.Remove(raw)
	defer os.Remove(wav)
	out, err := os.Create(raw)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	_, err = io.Copy(out, f)
	out.Close()
	if err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	if o, err := exec.CommandContext(ctx, e.opt.ffmpeg, "-y", "-loglevel", "error", "-i", raw, "-ac", "1", "-ar", "16000", wav).CombinedOutput(); err != nil {
		jsonErr(w, http.StatusBadRequest, "couldn't read that recording: "+string(o))
		return
	}
	text, err := e.opt.transcriber.Transcribe(ctx, wav)
	if err != nil {
		jsonErr(w, http.StatusBadGateway, err.Error())
		return
	}
	jsonOK(w, map[string]string{"text": text})
}

func (e *Engine) listInteractions(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if r.URL.Query().Get("all") != "" {
		limit = 1000
	}
	is, err := e.st.interactions(r.Context(), courseOf(r).ID, limit)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if is == nil {
		is = []*Interaction{}
	}
	jsonOK(w, is)
}

func (e *Engine) setMember(w http.ResponseWriter, r *http.Request) {
	var in struct {
		User string `json:"user"`
		Add  bool   `json:"add"`
	}
	if err := decode(r, &in); err != nil || strings.TrimSpace(in.User) == "" {
		jsonErr(w, http.StatusBadRequest, "say who to add or remove")
		return
	}
	c := courseOf(r)
	if strings.TrimSpace(in.User) == c.Owner {
		jsonErr(w, http.StatusBadRequest, "that's the owner")
		return
	}
	if err := e.st.setMember(r.Context(), c.ID, strings.TrimSpace(in.User), in.Add); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	ms, _ := e.st.members(r.Context(), c.ID)
	jsonOK(w, map[string]any{"members": ms})
}

// setFocus: "teach this next" from the mind map.
func (e *Engine) setFocus(w http.ResponseWriter, r *http.Request) {
	c := courseOf(r)
	var in struct {
		NodeID string `json:"node_id"`
	}
	if err := decode(r, &in); err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	n, err := e.st.node(r.Context(), in.NodeID)
	if err != nil || n.CourseID != c.ID {
		jsonErr(w, http.StatusNotFound, "no such idea on the map")
		return
	}
	_ = e.st.updateCourse(r.Context(), c.ID, map[string]any{"steer": n.Title})
	// Make room: drop what's queued but not yet heard, so the next segment is about this.
	pos, _ := e.st.ownerPosition(r.Context(), c)
	nDiscarded, _ := e.st.discardAfter(r.Context(), c.ID, pos, "")
	e.poke()
	jsonOK(w, map[string]any{"steer": n.Title, "discarded": nDiscarded})
}

func (e *Engine) listNotes(w http.ResponseWriter, r *http.Request) {
	ns, err := e.st.notes(r.Context(), courseOf(r).ID, nil, true, 500)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if ns == nil {
		ns = []*Note{}
	}
	jsonOK(w, ns)
}

func (e *Engine) patchNote(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Resolved bool `json:"resolved"`
		Delete   bool `json:"delete"`
	}
	if err := decode(r, &in); err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := e.st.setNote(r.Context(), courseOf(r).ID, r.PathValue("nid"), in.Resolved, in.Delete); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	jsonOK(w, map[string]bool{"ok": true})
}

// publicQuiz hides answer keys until the quiz has been submitted.
func publicQuiz(q *Quiz) *Quiz {
	if q.Status != "ready" && q.Status != "writing" {
		return q
	}
	var qs []quizQuestion
	if json.Unmarshal(q.Questions, &qs) == nil {
		for k := range qs {
			qs[k].Answer, qs[k].Explain, qs[k].ModelAnswer, qs[k].Rubric = -1, "", "", ""
		}
		cp := *q
		cp.Questions, _ = json.Marshal(qs)
		return &cp
	}
	return q
}

func (e *Engine) listQuizzes(w http.ResponseWriter, r *http.Request) {
	qs, err := e.st.quizzes(r.Context(), courseOf(r).ID, 30)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := []*Quiz{}
	for _, q := range qs {
		out = append(out, publicQuiz(q))
	}
	jsonOK(w, out)
}

func (e *Engine) newQuiz(w http.ResponseWriter, r *http.Request) {
	c := courseOf(r)
	var in struct {
		NodeID string `json:"node_id"`
	}
	_ = decode(r, &in)
	if c.Status != StatusActive {
		jsonErr(w, http.StatusConflict, "listen for a bit first; quizzes cover what you've heard")
		return
	}
	q := &Quiz{CourseID: c.ID, NodeID: in.NodeID, Status: "writing"}
	if err := e.st.addQuiz(r.Context(), q); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := e.st.enqueue(r.Context(), c.ID, "quiz", 85, map[string]string{"quiz_id": q.ID}); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	e.poke()
	writeJSON(w, http.StatusAccepted, q)
}

func (e *Engine) getQuiz(w http.ResponseWriter, r *http.Request) {
	q, err := e.st.quiz(r.Context(), r.PathValue("qid"))
	if err != nil || q.CourseID != courseOf(r).ID {
		jsonErr(w, http.StatusNotFound, "no such quiz")
		return
	}
	jsonOK(w, publicQuiz(q))
}

func (e *Engine) answerQuiz(w http.ResponseWriter, r *http.Request) {
	c := courseOf(r)
	q, err := e.st.quiz(r.Context(), r.PathValue("qid"))
	if err != nil || q.CourseID != c.ID {
		jsonErr(w, http.StatusNotFound, "no such quiz")
		return
	}
	if q.Status != "ready" {
		jsonErr(w, http.StatusConflict, "this quiz has already been answered")
		return
	}
	var in struct {
		Answers []json.RawMessage `json:"answers"`
	}
	if err := decode(r, &in); err != nil {
		jsonErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := e.submitQuiz(r.Context(), c, q, in.Answers); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	e.poke()
	q, _ = e.st.quiz(r.Context(), q.ID)
	jsonOK(w, q)
}

// rebuildCourse re-plans the map from scratch (reading the learner's code when research is on): it drops
// what hasn't been heard yet, starts a fresh teacher session, and keeps the level check, profile and notes.
func (e *Engine) rebuildCourse(w http.ResponseWriter, r *http.Request) {
	c := courseOf(r)
	ctx := r.Context()
	if c.Status != StatusActive && c.Status != StatusError {
		jsonErr(w, http.StatusConflict, "this course is still being set up")
		return
	}
	n, err := e.st.rebuild(ctx, c)
	if err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := e.st.enqueue(ctx, c.ID, "plan", 60, nil); err != nil {
		jsonErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	e.poke()
	e.log.Info().Str("course", c.ID).Int64("discarded", n).Msg("rebuilding the course map")
	jsonOK(w, map[string]any{"status": StatusPlanning, "discarded": n})
}
