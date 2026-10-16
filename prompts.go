package learn

import (
	"fmt"
	"strings"
	"time"
)

// The teacher's standing instructions. They go at the top of every new session; resumed sessions
// already have them.
const teacherBrief = `You are the teacher in a personal, never-ending audio course. Everything you write is read aloud by a
text-to-speech voice and shown as a word-by-word transcript. One person, the learner, is listening.

How to write for the ear:
- Speak to the learner directly and warmly, like a great one-on-one tutor. Plain, natural spoken English.
- Short sentences. One idea at a time. Signpost structure out loud ("there are three parts to this…").
- Never read code, commands or tables out loud symbol by symbol. Describe what they do and name the key
  identifiers in words.
- Spell out an acronym the first time you use it, then use it normally.
- No markdown, no bullet points, no headings, no emoji, no URLs. Paragraphs separated by a blank line.
- Use concrete examples, analogies and small stories from real systems and real incidents.
- Pitch everything at the learner's current level: don't re-explain what they know; slow down where they're weak.
- Never invent facts. If something is contested or version-specific, say so.`

func profileBlock(c *Course) string {
	p := strings.TrimSpace(string(c.Profile))
	if p == "" || p == "{}" {
		p = "(no profile yet)"
	}
	return "Learner profile (JSON, maintained by you):\n" + p
}

func courseBlock(c *Course) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Course topic: %s\n", c.Topic)
	if c.Goal != "" {
		fmt.Fprintf(&b, "The learner's goal: %s\n", c.Goal)
	}
	if c.Background != "" {
		fmt.Fprintf(&b, "What the learner says they already know: %s\n", c.Background)
	}
	for _, q := range c.Calibration.Questions {
		if q.Answer != "" {
			fmt.Fprintf(&b, "Level check — Q: %s\nA: %s\n", q.Q, q.Answer)
		}
	}
	return b.String()
}

func calibratePrompt(c *Course) string {
	return teacherBrief + "\n\n" + courseBlock(c) + `
Before planning the course, you want to find the learner's real level. Write 4 short probe questions of
rising depth that, from the answers, tell you what to skip and where to start. Make them answerable in a
sentence or two by typing or speaking; no multiple choice. Also propose a short, specific course title.

Reply with only a JSON object, no prose and no code fences:
{"title": "...", "questions": [{"q": "...", "why": "what the answer tells you"}]}`
}

func planPrompt(c *Course) string {
	return teacherBrief + "\n\n" + courseBlock(c) + `
Sketch a map of this topic for a learner who will listen for many hours. It is not a syllabus to work
through once: it's the territory you'll roam, coming back to ideas from new angles as they learn, and it
will keep growing. Use the level-check answers to pitch where to begin. Write 6 to 8 modules, each with
4 to 6 concepts, in a sensible starting order; later modules go deeper and more practical. Also write the first learner profile: what they know, gaps, misconceptions you
suspect, and how to pitch the teaching. Give each concept your estimate of the learner's mastery (0 to 1).

Reply with only a JSON object, no prose and no code fences:
{"title": "...",
 "modules": [{"title": "...", "summary": "what this module covers and why",
   "concepts": [{"title": "...", "objectives": "what the learner will be able to explain", "mastery": 0.1}]}],
 "profile": {"level": "...", "knows": ["..."], "gaps": ["..."], "misconceptions": ["..."], "pitch": "how to teach this person", "notes": []}}`
}

func extendPrompt(c *Course, done []string) string {
	return teacherBrief + "\n\n" + courseBlock(c) + "\n" + profileBlock(c) + `

The map is getting well covered. Modules so far:
- ` + strings.Join(done, "\n- ") + `

Add 3 more modules to the map: go deeper, cover adjacent topics the learner would benefit from given
their profile and questions, and include at least one practical, real-world module (case studies,
incidents, hands-on reasoning). Don't duplicate concepts already on the map.

Reply with only a JSON object, no prose and no code fences:
{"modules": [{"title": "...", "summary": "...", "concepts": [{"title": "...", "objectives": "...", "mastery": 0.0}]}]}`
}

// nextContext is what the teacher needs to decide and write the next segment.
type nextContext struct {
	Modules       []*Node
	Concepts      []*Node
	Recent        []*Segment
	Asked         []*Interaction
	Suggestion    *Node
	Steer         string
	CheckinDue    bool
	SegmentsSoFar int
	InPlay        map[string]bool // concept ids the turn is likely about: full detail for these
	Notes         []*Note         // what we know about the learner, on the concepts in play
	Past          []*Interaction  // older questions about the concepts in play
}

// mapBlock renders the map within a budget: small maps in full; big ones in full only for the modules
// in play, and otherwise just the weak, stale or fresh ideas plus a one-line summary per module.
func mapBlock(modules, concepts []*Node, inPlay map[string]bool) string {
	var b strings.Builder
	b.WriteString("Map of the topic (module › concept — mastery 0-1, times covered, last covered):\n")
	full := len(concepts) <= 50
	for _, m := range modules {
		var mine []*Node
		hot := false
		for _, n := range concepts {
			if n.ParentID == m.ID {
				mine = append(mine, n)
				hot = hot || inPlay[n.ID]
			}
		}
		fmt.Fprintf(&b, "%s\n", m.Title)
		shown, sum, fresh := 0, 0.0, 0
		for _, n := range mine {
			sum += n.Mastery
			if n.Covered == 0 {
				fresh++
			}
			stale := n.Covered > 0 && strings.HasSuffix(ago(n.LastCoveredAt), "days ago")
			if full || hot || inPlay[n.ID] || (n.Covered > 0 && n.Mastery < 0.5) || stale || (n.Covered == 0 && fresh <= 2) {
				fmt.Fprintf(&b, "  › %s — %.1f, %d×, %s\n", n.Title, n.Mastery, n.Covered, ago(n.LastCoveredAt))
				shown++
			}
		}
		if hidden := len(mine) - shown; hidden > 0 {
			fmt.Fprintf(&b, "  (%d more here; average mastery %.1f; %d not yet covered)\n", hidden, sum/float64(max(1, len(mine))), fresh)
		}
	}
	return b.String()
}

func notesBlock(notes []*Note, titles map[string]string) string {
	if len(notes) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("What you've learned about this learner, relevant here:\n")
	for _, n := range notes {
		about := ""
		if t := titles[n.NodeID]; t != "" {
			about = " (" + t + ")"
		}
		fmt.Fprintf(&b, "- %s%s: %s\n", n.Kind, about, n.Text)
	}
	return b.String()
}

func ago(ts string) string {
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	}
	return fmt.Sprintf("%d days ago", int(d.Hours()/24))
}

// nextPrompt: this course is not a book. Each segment, the teacher decides what this learner needs
// now — continue the thread, revisit an idea from a new angle, go deeper, review, or branch out —
// and writes it.
func nextPrompt(c *Course, x nextContext, resumed bool) string {
	var b strings.Builder
	if !resumed {
		b.WriteString(teacherBrief + "\n\n" + courseBlock(c) + `
This course is not a book with chapters to finish. It is an endless, adaptive stream: before each
segment you decide what this learner needs next. You may continue the current thread, come back to an
idea from a fresh angle (repetition from new angles is good teaching), go deeper, run a quick review,
connect two ideas, or branch into something new. Let the learner's questions and answers steer you.
`)
	}
	b.WriteString("\n" + profileBlock(c) + "\n\n")
	b.WriteString(mapBlock(x.Modules, x.Concepts, x.InPlay))
	titles := map[string]string{}
	for _, n := range x.Concepts {
		titles[n.ID] = n.Title
	}
	if nb := notesBlock(x.Notes, titles); nb != "" {
		b.WriteString("\n" + nb)
	}
	if len(x.Past) > 0 {
		b.WriteString("\nEarlier questions of theirs on these ideas:\n")
		for _, i := range x.Past {
			fmt.Fprintf(&b, "- %q\n", i.Text)
		}
	}
	if len(x.Recent) > 0 {
		b.WriteString("\nWhat the learner has just heard, most recent last:\n")
		for _, g := range x.Recent {
			fmt.Fprintf(&b, "- [%s] %s\n", g.Kind, g.Summary)
		}
	} else {
		b.WriteString("\nThis is the very first segment. Welcome them briefly, connect to what they told you, and begin.\n")
	}
	if len(x.Asked) > 0 {
		b.WriteString("\nWhat the learner has said recently, most recent first:\n")
		for _, i := range x.Asked {
			v := ""
			if i.Verdict != "" {
				v = " (" + i.Verdict + ")"
			}
			fmt.Fprintf(&b, "- %s%s: %q\n", i.Kind, v, i.Text)
		}
	}
	if x.Steer != "" {
		fmt.Fprintf(&b, "\nThe learner asked for this next: %s\n", x.Steer)
	}
	if x.Suggestion != nil {
		fmt.Fprintf(&b, "\nIf nothing above suggests otherwise, a sensible default is: %q.\n", x.Suggestion.Title)
	}
	b.WriteString(`
Decide what comes next, then write it: about 400 to 500 spoken words (roughly three minutes), ending at
a natural pause. Make transitions explicit when you change subject ("let's come back to…", "now that
you've got X, here's where it gets interesting…").
Put the exact map concept title you're teaching in "focus". If you're branching into something that
isn't on the map, describe it in "new_concept" (and give its title as "focus"); name the module it
belongs to, or a new module title.`)
	if x.CheckinDue {
		b.WriteString(` End this segment with a short check-in question the learner can answer out loud or by typing
(recall, or "explain it back in your own words"), and put that question in "checkin" too. Tell them to
pause and answer if they like, or just keep listening.`)
	}
	b.WriteString(`
List any technical terms a text-to-speech voice might mispronounce in "pronunciations", mapping the term
as written to how it should be said (e.g. {"etcd": "et-see-dee", "nginx": "engine-x"}).

Reply with only a JSON object, no prose and no code fences:
{"move": "continue|revisit|deeper|review|connect|new", "focus": "exact concept title",
 "new_concept": {"title": "", "objectives": "", "module": ""},
 "title": "a short title for this segment", "script": "the spoken text, paragraphs separated by blank lines",
 "summary": "one or two sentences: what this segment taught", "checkin": "", "pronunciations": {}}`)
	return b.String()
}

func answerPrompt(c *Course, i *Interaction, current *Segment, recent []*Segment, upcoming []*Segment, x nextContext, resumed bool) string {
	var b strings.Builder
	if !resumed {
		b.WriteString(teacherBrief + "\n\n" + courseBlock(c) + "\n")
	}
	b.WriteString(profileBlock(c) + "\n\n")
	b.WriteString(mapBlock(x.Modules, x.Concepts, x.InPlay) + "\n")
	titles := map[string]string{}
	for _, n := range x.Concepts {
		titles[n.ID] = n.Title
	}
	if nb := notesBlock(x.Notes, titles); nb != "" {
		b.WriteString(nb + "\n")
	}
	if len(x.Past) > 0 {
		b.WriteString("Their earlier questions on these ideas:\n")
		for _, p := range x.Past {
			fmt.Fprintf(&b, "- %q\n", p.Text)
		}
		b.WriteString("\n")
	}
	if len(recent) > 0 {
		b.WriteString("Recent segments (summaries), most recent last:\n")
		for _, g := range recent {
			fmt.Fprintf(&b, "- %s\n", g.Summary)
		}
	}
	if current != nil {
		fmt.Fprintf(&b, "\nThe learner was listening to this segment when they spoke up:\n%q\n", current.Script)
	}
	if len(upcoming) > 0 {
		b.WriteString("\nAlready prepared to play next (summaries):\n")
		for _, g := range upcoming {
			fmt.Fprintf(&b, "- %s\n", g.Summary)
		}
	}
	if i.Kind == "checkin" {
		fmt.Fprintf(&b, "\nYou asked the check-in question %q. The learner answered:\n%q\n", current.Checkin, i.Text)
		b.WriteString(`
Respond out loud: tell them warmly and honestly how they did, correct any misconception, fill the gap in
a few sentences, and lead back into the course. Keep it short (60 to 200 words).
Judge the answer: "correct", "partial" or "wrong".`)
	} else {
		fmt.Fprintf(&b, "\nThe learner said or asked:\n%q\n", i.Text)
		b.WriteString(`
Respond out loud, directly to this, at their level: answer the question, or act on the request. Keep it
focused (80 to 350 words) and lead back into the course at the end.
Then decide what happens to the prepared segments: "keep" if they still fit, or "regenerate" if this
changes direction (they already know it, want more depth, want to switch focus, or are confused).`)
	}
	b.WriteString(`
Update the learner profile with anything this reveals (as a complete replacement JSON object, under 250
words; keep it a compact summary, the details belong in notes), and give mastery estimates (0 to 1) for any
concepts this tells you about, by exact concept title. Record what this tells you about the learner as
short notes, each tagged to an exact concept title where it applies: kind is one of misconception, gap,
strength, interest, preference. Put the concept this exchange is mainly about in "about".
If the learner already knows concepts, list their exact titles in "known" so the course skips them. If
they asked to focus on a concept that comes later, put its exact title in "next_concept" so it's taught next.

Reply with only a JSON object, no prose and no code fences:
{"title": "short title", "script": "the spoken reply, paragraphs separated by blank lines", "summary": "one sentence",
 "verdict": "", "replan": "keep", "about": "exact concept title", "profile": {}, "mastery": {"concept title": 0.5},
 "notes": [{"concept": "exact concept title or empty", "kind": "gap", "text": "..."}], "known": [], "next_concept": "",
 "pronunciations": {}}`)
	return b.String()
}

func quizPrompt(c *Course, x nextContext, recent []*Segment, focus *Node) string {
	var b strings.Builder
	b.WriteString(courseBlock(c) + "\n" + profileBlock(c) + "\n\n" + mapBlock(x.Modules, x.Concepts, x.InPlay) + "\n")
	titles := map[string]string{}
	for _, n := range x.Concepts {
		titles[n.ID] = n.Title
	}
	b.WriteString(notesBlock(x.Notes, titles))
	if len(recent) > 0 {
		b.WriteString("\nWhat the learner has heard recently (summaries):\n")
		for _, g := range recent {
			fmt.Fprintf(&b, "- %s\n", g.Summary)
		}
	}
	if focus != nil {
		fmt.Fprintf(&b, "\nThe learner asked to be quizzed on: %q.\n", focus.Title)
	} else {
		b.WriteString("\nQuiz what they've actually heard, weighted towards weak, stale or misunderstood ideas.\n")
	}
	b.WriteString(`
Write a 5-question quiz that makes the learner retrieve and apply ideas, not recognise wording: 3
multiple-choice questions (4 options, one correct, plausible distractors drawn from real misconceptions)
and 2 short-answer questions answerable in a sentence or two. Tag each with the exact concept title.

Reply with only a JSON object, no prose and no code fences:
{"questions": [
  {"type": "mc", "concept": "exact title", "q": "...", "options": ["...", "...", "...", "..."], "answer": 0, "explain": "why the right one is right and the others aren't"},
  {"type": "short", "concept": "exact title", "q": "...", "model_answer": "...", "rubric": "what a correct answer must contain"}]}`)
	return b.String()
}

func gradePrompt(c *Course, items string) string {
	return courseBlock(c) + "\n" + profileBlock(c) + `

Grade the learner's short answers to these quiz questions. Be fair: credit the idea even if the wording
differs. For each, give a verdict (correct, partial or wrong) and one or two sentences of feedback that
teach the missing piece. Then record what the answers reveal about the learner as short notes tagged to
exact concept titles (kind: misconception, gap or strength).

` + items + `

Reply with only a JSON object, no prose and no code fences:
{"results": [{"i": <the question's i>, "verdict": "correct", "feedback": "..."}],
 "notes": [{"concept": "exact concept title", "kind": "misconception", "text": "..."}]}`
}
