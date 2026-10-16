package learn

import (
	"fmt"
	"strings"
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
Plan this course as a curriculum the learner will listen to for many hours. Use the level-check answers
to skip what they know and start at the right depth. Write 6 to 8 modules, each with 4 to 6 concepts, in
teaching order; each concept should take roughly 10 to 25 minutes of listening. Later modules go deeper
and more practical. Also write the first learner profile: what they know, gaps, misconceptions you
suspect, and how to pitch the teaching. Give each concept your estimate of the learner's mastery (0 to 1).

Reply with only a JSON object, no prose and no code fences:
{"title": "...",
 "modules": [{"title": "...", "summary": "what this module covers and why",
   "concepts": [{"title": "...", "objectives": "what the learner will be able to explain", "mastery": 0.1}]}],
 "profile": {"level": "...", "knows": ["..."], "gaps": ["..."], "misconceptions": ["..."], "pitch": "how to teach this person", "notes": []}}`
}

func extendPrompt(c *Course, done []string) string {
	return teacherBrief + "\n\n" + courseBlock(c) + "\n" + profileBlock(c) + `

The learner has finished the curriculum so far. Modules covered:
- ` + strings.Join(done, "\n- ") + `

Plan the next 3 modules: go deeper, cover adjacent topics the learner would benefit from, and include at
least one practical, real-world module (case studies, incidents, hands-on reasoning). Don't repeat
what's covered.

Reply with only a JSON object, no prose and no code fences:
{"modules": [{"title": "...", "summary": "...", "concepts": [{"title": "...", "objectives": "...", "mastery": 0.0}]}]}`
}

// segmentContext is what a script turn needs to know about where the course is.
type segmentContext struct {
	Module, Concept     *Node
	NewConcept          bool
	Recent              []*Segment
	Reviews             []*Node
	CheckinDue          bool
	NextConceptTitle    string
	ModuleIndex, Of     int
	ConceptIndex, OfCon int
}

func scriptPrompt(c *Course, x segmentContext, resumed bool) string {
	var b strings.Builder
	if !resumed {
		b.WriteString(teacherBrief + "\n\n" + courseBlock(c) + "\n")
		fmt.Fprintf(&b, "You're teaching module %d of %d: %q — %s\n", x.ModuleIndex, x.Of, x.Module.Title, x.Module.Summary)
	}
	b.WriteString(profileBlock(c) + "\n\n")
	if len(x.Recent) > 0 {
		b.WriteString("What the learner has just heard, most recent last (summaries):\n")
		for _, g := range x.Recent {
			fmt.Fprintf(&b, "- [%s] %s\n", g.Kind, g.Summary)
		}
		b.WriteString("\n")
	}
	if x.NewConcept {
		fmt.Fprintf(&b, "Start concept %d of %d in this module: %q. Objectives: %s\n", x.ConceptIndex, x.OfCon, x.Concept.Title, x.Concept.Summary)
	} else {
		fmt.Fprintf(&b, "Continue concept %q (objectives: %s) from where the last segment stopped.\n", x.Concept.Title, x.Concept.Summary)
	}
	fmt.Fprintf(&b, "The learner's estimated mastery of it: %.1f.\n", x.Concept.Mastery)
	if len(x.Reviews) > 0 {
		b.WriteString("Weave in a brief spaced-repetition callback to one of these earlier concepts where it fits naturally: ")
		for i, r := range x.Reviews {
			if i > 0 {
				b.WriteString("; ")
			}
			b.WriteString(r.Title)
		}
		b.WriteString(".\n")
	}
	if x.NextConceptTitle != "" {
		fmt.Fprintf(&b, "(The concept after this one is %q.)\n", x.NextConceptTitle)
	}
	b.WriteString(`
Write the next segment: about 400 to 500 spoken words (roughly three minutes). End at a natural pause.
Set concept_done to true when this concept has been taught well enough to move on (usually after 3 to 6
segments; fewer if the learner already knows it well).`)
	if x.CheckinDue {
		b.WriteString(` End this segment with a short check-in question the learner can answer out loud or by typing
(recall, or "explain it back in your own words"), and put that question in "checkin" too. Tell them to
pause and answer if they like, or just keep listening.`)
	}
	b.WriteString(`
List any technical terms a text-to-speech voice might mispronounce in "pronunciations", mapping the term
as written to how it should be said (e.g. {"etcd": "et-see-dee", "nginx": "engine-x"}).

Reply with only a JSON object, no prose and no code fences:
{"title": "a short title for this segment", "script": "the spoken text, paragraphs separated by blank lines",
 "summary": "one or two sentences: what this segment taught", "concept_done": false, "checkin": "",
 "pronunciations": {}}`)
	return b.String()
}

func answerPrompt(c *Course, i *Interaction, current *Segment, recent []*Segment, upcoming []*Segment, curriculum []*Node, resumed bool) string {
	var b strings.Builder
	if !resumed {
		b.WriteString(teacherBrief + "\n\n" + courseBlock(c) + "\n")
	}
	b.WriteString(profileBlock(c) + "\n\n")
	if len(curriculum) > 0 {
		b.WriteString("Curriculum concepts (exact titles; status; estimated mastery):\n")
		for _, n := range curriculum {
			if n.Kind == "concept" {
				fmt.Fprintf(&b, "- %s (%s, %.1f)\n", n.Title, n.Status, n.Mastery)
			}
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
Update the learner profile with anything this reveals (as a complete replacement JSON object), and give
mastery estimates (0 to 1) for any concepts this tells you about, by exact concept title.
If the learner already knows concepts, list their exact titles in "known" so the course skips them. If
they asked to focus on a concept that comes later, put its exact title in "next_concept" so it's taught next.

Reply with only a JSON object, no prose and no code fences:
{"title": "short title", "script": "the spoken reply, paragraphs separated by blank lines", "summary": "one sentence",
 "verdict": "", "replan": "keep", "profile": {}, "mastery": {"concept title": 0.5}, "known": [], "next_concept": "",
 "pronunciations": {}}`)
	return b.String()
}
