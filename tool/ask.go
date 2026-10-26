package main

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"html/template"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/microcosm-cc/bluemonday"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	gmhtml "github.com/yuin/goldmark/renderer/html"
)

//go:embed ask.html.tmpl
var askTmplSrc string

var askTmpl = template.Must(template.New("ask").Parse(askTmplSrc))

const maxIncludeBytes = 1 << 20

var (
	includeRe = regexp.MustCompile(`^include: (\S+)(?: (.+))?$`)
	optionRe  = regexp.MustCompile(`^### +([a-z0-9][a-z0-9-]{0,40}): +(\S.*)$`)
)

type askOption struct {
	ID, Label   string
	Body        template.HTML
	Recommended bool
}

type askExtra struct {
	Title string
	Body  template.HTML
}

type askQuestion struct {
	ID, Lead, Label, Resolve string // Label is the key, or a placeholder when keyless
	Question, Change         string
	Warning                  string
	Context, Next            template.HTML
	Extra                    []askExtra
	Options                  []askOption
	source                   *card // Plain source for the shared information view, not rendered HTML.
}

type askPage struct {
	Questions []askQuestion
	Mermaid   bool
}

// card is a parsed question-card file, before includes are rendered.
type card struct {
	question, recommend, change string
	area                        string // optional: the topic the question belongs to (sweep routing)
	route, ready, action, owner string // readiness is separate from decision authority
	context, next               string
	extra                       []struct{ title, body string }
	options                     []struct{ id, label, body string }
}

// fenceTracker tells whether a line sits inside a fenced code block.
type fenceTracker struct{ fence string }

func (f *fenceTracker) inside(line string) bool {
	t := strings.TrimSpace(line)
	if f.fence != "" {
		if strings.HasPrefix(t, f.fence) {
			f.fence = ""
		}
		return true
	}
	for _, m := range []string{"```", "~~~"} {
		if strings.HasPrefix(t, m) {
			f.fence = m
			return true
		}
	}
	return false
}

func parseCard(src string) (*card, error) {
	lines := strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return nil, fmt.Errorf("front matter must start with ---")
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return nil, fmt.Errorf("front matter has no closing ---")
	}
	c := &card{}
	for _, l := range lines[1:end] {
		if strings.TrimSpace(l) == "" {
			continue
		}
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			return nil, fmt.Errorf("front matter line %q is not key: value", l)
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		switch strings.TrimSpace(k) {
		case "question":
			c.question = v
		case "recommend":
			c.recommend = v
		case "change":
			c.change = v
		case "area":
			c.area = v
		case "route":
			c.route = v
		case "ready":
			c.ready = v
		case "action":
			c.action = v
		case "owner":
			c.owner = v
		}
	}
	if c.question == "" {
		return nil, fmt.Errorf("front matter needs a question")
	}
	if c.change != "" && c.change != "easy" && c.change != "hard" {
		return nil, fmt.Errorf("change must be easy or hard, got %q", c.change)
	}
	// Split the rest into "## " sections, keeping fenced code intact.
	var name string
	var body []string
	var ft fenceTracker
	flush := func() {
		text := strings.TrimSpace(strings.Join(body, "\n"))
		switch strings.ToLower(name) {
		case "":
		case "context":
			c.context = text
		case "options":
			c.options = nil
			c.parseOptions(text)
		case "next":
			c.next = text
		default:
			c.extra = append(c.extra, struct{ title, body string }{name, text})
		}
		body = nil
	}
	for _, l := range lines[end+1:] {
		if !ft.inside(l) && strings.HasPrefix(l, "## ") {
			flush()
			name = strings.TrimSpace(l[3:])
			continue
		}
		body = append(body, l)
	}
	flush()
	return c, c.validate()
}

func (c *card) parseOptions(text string) {
	var ft fenceTracker
	for _, l := range strings.Split(text, "\n") {
		if !ft.inside(l) && strings.HasPrefix(l, "### ") {
			if m := optionRe.FindStringSubmatch(l); m != nil {
				c.options = append(c.options, struct{ id, label, body string }{m[1], strings.TrimSpace(m[2]), ""})
			} else {
				c.options = append(c.options, struct{ id, label, body string }{"", l, ""})
			}
			continue
		}
		if n := len(c.options); n > 0 {
			c.options[n-1].body += l + "\n"
		}
	}
}

func (c *card) validate() error {
	seen := map[string]bool{}
	for _, o := range c.options {
		if o.id == "" {
			return fmt.Errorf("option heading %q must be \"### id: label\" with a lowercase slug id", o.label)
		}
		if seen[o.id] {
			return fmt.Errorf("duplicate option id %q", o.id)
		}
		seen[o.id] = true
	}
	if len(c.options) == 1 {
		return fmt.Errorf("one option is not a choice: give two or more, or none for a free-text question")
	}
	if c.recommend != "" && !seen[c.recommend] {
		return fmt.Errorf("recommend names %q, which is not an option", c.recommend)
	}
	return nil
}

// renderer turns card text into HTML for one question, resolving includes.
type renderer struct {
	lead, key, cardPath string
	taskDir, qDir       string // taskDir is symlink-resolved
	rawTaskDir          string
	filesDir            string
	mermaid             bool
}

// Raw HTML in markdown is not emitted. Hard wraps keep a card's short lines
// (Gain:, Cost:) on their own lines, as the lead wrote them.
var md = goldmark.New(goldmark.WithExtensions(extension.Table), goldmark.WithRendererOptions(gmhtml.WithHardWraps()))

func renderMarkdown(src string) (string, error) {
	var b bytes.Buffer
	if err := md.Convert([]byte(src), &b); err != nil {
		return "", err
	}
	return b.String(), nil
}

// body renders markdown, replacing include lines with the rendered file.
func (r *renderer) body(text string) (template.HTML, error) {
	var out, chunk strings.Builder
	flush := func() error {
		if strings.TrimSpace(chunk.String()) == "" {
			chunk.Reset()
			return nil
		}
		s, err := renderMarkdown(chunk.String())
		chunk.Reset()
		out.WriteString(s)
		return err
	}
	var ft fenceTracker
	for _, l := range strings.Split(text, "\n") {
		if m := includeRe.FindStringSubmatch(strings.TrimRight(l, "\r")); m != nil && !ft.inside(l) {
			if err := flush(); err != nil {
				return "", err
			}
			s, err := r.include(m[1], strings.TrimSpace(m[2]))
			if err != nil {
				return "", err
			}
			out.WriteString(s)
			out.WriteString("\n")
			continue
		}
		chunk.WriteString(l)
		chunk.WriteString("\n")
	}
	if err := flush(); err != nil {
		return "", err
	}
	// #nosec: markdown went through goldmark without raw HTML, and HTML includes went through htmlPolicy.
	return template.HTML(out.String()), nil
}

func (r *renderer) include(rel, alt string) (string, error) {
	inside := func(p string) bool {
		return p == r.taskDir || strings.HasPrefix(p, r.taskDir+string(filepath.Separator))
	}
	// Check the lexical path first so a missing outside file is still "outside".
	if lex := filepath.Join(r.qDir, rel); !inside(lex) && !inside(r.resolveDir(lex)) {
		return "", fmt.Errorf("%s: include %s is outside the task directory", r.cardPath, rel)
	}
	real, err := filepath.EvalSymlinks(filepath.Join(r.qDir, rel))
	if err != nil {
		return "", fmt.Errorf("%s: include %s: file not found", r.cardPath, rel)
	}
	if !inside(real) {
		return "", fmt.Errorf("%s: include %s is outside the task directory", r.cardPath, rel)
	}
	st, err := os.Stat(real)
	if err != nil || !st.Mode().IsRegular() {
		return "", fmt.Errorf("%s: include %s is not a regular file", r.cardPath, rel)
	}
	if st.Size() > maxIncludeBytes {
		return "", fmt.Errorf("%s: include %s is over 1 MiB", r.cardPath, rel)
	}
	ext := strings.ToLower(filepath.Ext(rel))
	switch ext {
	// SVG goes in as an image: a browser runs no script in an <img>.
	case ".png", ".jpg", ".jpeg", ".webp", ".gif", ".svg":
		if alt == "" {
			return "", fmt.Errorf("%s: include %s needs alt text: include: %s <alt text>", r.cardPath, rel, rel)
		}
		sum := sha256.Sum256([]byte(real))
		name := fmt.Sprintf("%s-%s-%x-%s", r.lead, r.key, sum[:8], filepath.Base(rel))
		if err := copyFile(real, filepath.Join(r.filesDir, name)); err != nil {
			return "", fmt.Errorf("%s: include %s: %v", r.cardPath, rel, err)
		}
		return fmt.Sprintf(`<img src="files/%s" alt="%s">`, url.PathEscape(name), html.EscapeString(alt)), nil
	case ".md", ".html", ".mmd", ".mermaid":
	default:
		return "", fmt.Errorf("%s: include %s has an unsupported extension", r.cardPath, rel)
	}
	b, err := os.ReadFile(real)
	if err != nil {
		return "", fmt.Errorf("%s: include %s: %v", r.cardPath, rel, err)
	}
	switch ext {
	case ".md":
		return renderMarkdown(string(b))
	case ".mmd", ".mermaid":
		r.mermaid = true
		return `<pre class="mermaid">` + html.EscapeString(string(b)) + `</pre>`, nil
	}
	// A lead can copy outside text into an include, so HTML is cleaned: no
	// script, event handler, form control, id or javascript: link.
	// The wrapper clips the include so its styles cannot draw over the form.
	return `<div class="include">` + htmlPolicy.Sanitize(string(b)) + `</div>`, nil
}

// htmlPolicy is an allowlist that keeps layout, tables, lists and inline
// styles for UI mocks. It allows no id or name, so an include cannot shadow
// window globals such as window.lavish.
var htmlPolicy = func() *bluemonday.Policy {
	p := bluemonday.NewPolicy()
	p.AllowStandardURLs()
	p.AllowAttrs("href").OnElements("a")
	p.RequireNoFollowOnLinks(true)
	p.AddTargetBlankToFullyQualifiedLinks(true)
	p.AllowImages()
	p.AllowLists()
	p.AllowTables()
	p.AllowElements("p", "br", "hr", "div", "span", "h1", "h2", "h3", "h4", "h5", "h6",
		"strong", "em", "b", "i", "u", "s", "small", "sub", "sup", "mark", "kbd", "code", "pre",
		"blockquote", "figure", "figcaption", "section", "article", "aside", "header", "footer",
		"nav", "dl", "dt", "dd", "details", "summary")
	p.AllowAttrs("class", "style", "title", "lang", "role", "aria-label", "aria-hidden").Globally()
	return p
}()

// resolveDir maps a lexical path under the unresolved task dir to the resolved one.
func (r *renderer) resolveDir(p string) string {
	if rel, err := filepath.Rel(r.rawTaskDir, p); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.Join(r.taskDir, rel)
	}
	return p
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// buildCard reads and renders the card for one decision into q.
func buildCard(r *renderer, q *askQuestion) error {
	src, err := os.ReadFile(r.cardPath)
	if err != nil {
		return err
	}
	c, err := parseCard(string(src))
	if err != nil {
		return fmt.Errorf("%s: %v", r.cardPath, err)
	}
	return renderCard(r, q, c)
}

// Use the same parsed snapshot for readiness and rendering.
func renderCard(r *renderer, q *askQuestion, c *card) error {
	var err error
	q.source = c
	q.Question, q.Change = c.question, c.change
	if q.Context, err = r.body(c.context); err != nil {
		return err
	}
	if q.Next, err = r.body(c.next); err != nil {
		return err
	}
	for _, x := range c.extra {
		b, err := r.body(x.body)
		if err != nil {
			return err
		}
		q.Extra = append(q.Extra, askExtra{x.title, b})
	}
	for _, o := range c.options {
		b, err := r.body(o.body)
		if err != nil {
			return err
		}
		opt := askOption{ID: o.id, Label: o.label, Body: b, Recommended: o.id == c.recommend}
		if opt.Recommended {
			q.Options = append([]askOption{opt}, q.Options...)
		} else {
			q.Options = append(q.Options, opt)
		}
	}
	return nil
}

func cmdAsk(a *app, args []string) (retErr error) {
	fs := a.flags("ask")
	human := fs.Bool("human", false, "readable JSON question view, preserving all choices and uncertainty")
	machine := fs.Bool("json", false, "machine JSON of the selected question view")
	audience := outputAgent
	for _, arg := range args {
		if arg == "--" {
			break
		}
		if arg == "--human" || arg == "--human=true" {
			audience = outputHuman
		}
	}
	for _, arg := range args {
		if arg == "--" {
			break
		}
		if arg == "--json" || arg == "--json=true" {
			audience = outputJSON
		}
	}
	defer func() {
		if retErr == nil {
			return
		}
		code := 1
		var ee *exitError
		if errors.As(retErr, &ee) {
			code = ee.code
		}
		_ = writeOutput(a.out, audience, map[string]any{"status": "failed", "error": retErr.Error()})
		retErr = &exitError{code: code}
	}()
	_, _ = human, machine
	outFlag := fs.String("out", "", "write the page to `DIR` (default <run root>/ask, or <cards dir>/../ask)")
	cardsFlag := fs.String("cards", "", "ask ready owner/unclear cards in `DIR`; list lead-settled and not-ready cards")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if err := exactArgs(pos, 0, "ask [--cards DIR] [--out DIR]"); err != nil {
		return err
	}
	abs := func(p string) string {
		p = expandHome(p)
		if !filepath.IsAbs(p) {
			p = filepath.Join(a.cwd, p)
		}
		return filepath.Clean(p)
	}
	var out, cardsDir, root string
	if *cardsFlag != "" {
		cardsDir = abs(*cardsFlag)
		if st, err := os.Stat(cardsDir); err != nil || !st.IsDir() {
			return fail("--cards %s is not a directory", cardsDir)
		}
		out = filepath.Join(filepath.Dir(cardsDir), "ask")
	} else {
		s, err := a.site()
		if err != nil {
			return err
		}
		root = s.root
		out = filepath.Join(root, "ask")
	}
	if *outFlag != "" {
		out = abs(*outFlag)
	}
	filesDir := filepath.Join(out, "files")
	if err := os.RemoveAll(filesDir); err != nil {
		return err
	}
	if err := os.MkdirAll(filesDir, 0o755); err != nil {
		return err
	}

	var page askPage
	var warnings []askWarning
	type cardDisposition struct{ key, ready, reason string }
	var settled, notReady []cardDisposition
	cardCount := 0
	add := func(q askQuestion, r *renderer, fallback string) {
		q.ID = fmt.Sprintf("q%d", len(page.Questions)+1)
		var werr error
		if r != nil {
			card := q
			if werr = buildCard(r, &card); werr == nil {
				q = card
				page.Mermaid = page.Mermaid || r.mermaid
			}
		}
		if q.Question == "" {
			q.Question = fallback
			q.Context, q.Next, q.Extra, q.Options, q.Change = "", "", nil, nil, ""
		}
		if q.Warning != "" && werr == nil {
			werr = errors.New(q.Warning)
		}
		if werr != nil {
			msg := werr.Error()
			if r != nil {
				msg = strings.TrimPrefix(msg, r.cardPath+": ")
			}
			q.Warning = msg
			warnings = append(warnings, askWarning{q.Lead, q.Label, msg})
		}
		page.Questions = append(page.Questions, q)
	}

	if cardsDir != "" {
		// Each <key>.md in the folder is one open question; a card that is
		// answered moves to answered/, which this glob does not read.
		topic := filepath.Dir(cardsDir)
		topicReal, err := filepath.EvalSymlinks(topic)
		if err != nil {
			topicReal = topic
		}
		paths, _ := filepath.Glob(filepath.Join(cardsDir, "*.md"))
		sort.Strings(paths)
		for _, cardPath := range paths {
			if filepath.Base(cardPath) == "INDEX.md" { // overview, not an answerable card
				src, err := os.ReadFile(cardPath)
				if err != nil {
					warnings = append(warnings, askWarning{"", "INDEX.md", "cannot read retrieval warnings: " + err.Error()})
				} else {
					for _, msg := range cardIndexWarnings(string(src)) {
						warnings = append(warnings, askWarning{"", "INDEX.md", msg})
					}
				}
				continue
			}
			cardCount++
			key := strings.TrimSuffix(filepath.Base(cardPath), ".md")
			if !keyRe.MatchString(key) {
				msg := fmt.Sprintf("skipped: the name must match %s.md", keyRe)
				warnings = append(warnings, askWarning{"", filepath.Base(cardPath), msg})
				notReady = append(notReady, cardDisposition{key, "unclear", msg})
				continue
			}
			src, err := os.ReadFile(cardPath)
			if err != nil {
				notReady = append(notReady, cardDisposition{key, "unclear", "cannot read card: " + err.Error()})
				continue
			}
			c, err := parseCard(string(src))
			if err != nil {
				notReady = append(notReady, cardDisposition{key, "unclear", err.Error()})
				continue
			}
			readiness := c.readiness(string(src))
			if readiness.Status != "ready" {
				notReady = append(notReady, cardDisposition{key, readiness.Status, strings.Join(readiness.Missing, "; ")})
				continue
			}
			if c.route == "lead-may-decide" {
				settled = append(settled, cardDisposition{key, "ready", c.question})
				continue
			}
			// Missing, unknown and explicit unclear routes stay with the owner.
			r := &renderer{key: key, cardPath: cardPath, taskDir: topicReal, rawTaskDir: topic, qDir: cardsDir, filesDir: filesDir}
			q := askQuestion{Label: key}
			if err := renderCard(r, &q, c); err != nil {
				msg := strings.TrimPrefix(err.Error(), cardPath+": ")
				notReady = append(notReady, cardDisposition{key, "missing_evidence", msg})
				warnings = append(warnings, askWarning{"", key, msg})
				continue
			}
			page.Mermaid = page.Mermaid || r.mermaid
			add(q, nil, key)
		}
	}
	var leads []openLead
	if root != "" {
		leads = myLeads(openLeads(root), orchID())
	}
	for _, ol := range leads {
		taskReal, err := filepath.EvalSymlinks(ol.dir)
		if err != nil {
			taskReal = ol.dir
		}
		// The lead name and key go into a shell command on the page, so only
		// names that match their patterns are used there.
		lead := filepath.Base(ol.dir)
		if !taskIDRe.MatchString(lead) {
			warnings = append(warnings, askWarning{"", lead, fmt.Sprintf("skipped task directory: the name must match %s", taskIDRe)})
			continue
		}
		keyless := 0
		for _, e := range openDecisions(readStatus(ol.dir)) {
			q := askQuestion{Lead: lead, Label: e.Key, Resolve: e.Key}
			var r *renderer
			if !keyRe.MatchString(e.Key) {
				if e.Key != "" {
					q.Warning = fmt.Sprintf("key %q must match %s; answer it without --resolve", e.Key, keyRe)
				}
				keyless++
				q.Label, q.Resolve = fmt.Sprintf("keyless-%d", keyless), ""
			} else if cardPath := filepath.Join(ol.dir, "questions", e.Key+".md"); fileExists(cardPath) {
				r = &renderer{lead: lead, key: e.Key, cardPath: cardPath, taskDir: taskReal, rawTaskDir: ol.dir,
					qDir: filepath.Join(ol.dir, "questions"), filesDir: filesDir}
			}
			add(q, r, e.Text)
		}
	}

	var buf bytes.Buffer
	if err := askTmpl.Execute(&buf, page); err != nil {
		return err
	}
	path := filepath.Join(out, "index.html")
	if err := writeAtomic(path, buf.Bytes()); err != nil {
		return err
	}
	view := askOutputView(path, page.Questions, warnings)
	if cardsDir != "" {
		// Preserve B3's output boundary and D2's readiness dispositions.
		b, _ := json.Marshal(view)
		var fields map[string]any
		_ = json.Unmarshal(b, &fields)
		fields["cards"], fields["settled_by_lead"], fields["not_ready"] = cardCount, len(settled), len(notReady)
		rows := func(ds []cardDisposition) []map[string]string {
			out := make([]map[string]string, 0, len(ds))
			for _, d := range ds {
				out = append(out, map[string]string{"card": d.key, "ready": d.ready, "reason": d.reason})
			}
			return out
		}
		fields["lead_cards"], fields["unready_cards"] = rows(settled), rows(notReady)
		view = fields
	}
	return writeOutput(a.out, audience, view)
}

// askInformation is selected once for agents, humans and machine clients.
// Keep question/choices/required action ahead of provenance. Source Markdown is
// intentional: it preserves uncertainty and include references without HTML.
type askInformation struct {
	ID               string                  `json:"id"`
	Question         string                  `json:"question"`
	Options          []askChoiceInformation  `json:"options"`
	AnswerNeededFrom string                  `json:"answer_needed_from"`
	Context          string                  `json:"context,omitempty"`
	Recommendation   string                  `json:"recommendation,omitempty"`
	NextAction       string                  `json:"next_action,omitempty"`
	Change           string                  `json:"change,omitempty"`
	MaterialContext  []askContextInformation `json:"material_context,omitempty"`
	Warning          string                  `json:"warning,omitempty"`
	Lead             string                  `json:"lead,omitempty"`
	Resolve          string                  `json:"resolve,omitempty"`
}

type askChoiceInformation struct {
	ID     string `json:"id"`
	Choice string `json:"choice"`
	Effect any    `json:"effect"`
}

type askContextInformation struct {
	Topic  string   `json:"topic"`
	Detail []string `json:"detail"`
}

// Long prose is a block, not a cell in a dense uniform metadata table.
func askEffect(body string) any {
	if len(body) > 160 || strings.Contains(body, "\n") {
		return []string{body}
	}
	return body
}

func askOutputView(path string, questions []askQuestion, warnings []askWarning) any {
	packets := make([]askInformation, 0, len(questions))
	for _, q := range questions {
		id := q.Label
		if q.Lead != "" {
			id = q.Lead + "/" + id
		}
		p := askInformation{ID: id, Question: q.Question, Options: []askChoiceInformation{}, AnswerNeededFrom: "owner", Lead: q.Lead, Resolve: q.Resolve, Warning: q.Warning}
		if c := q.source; c != nil {
			p.Context, p.Recommendation, p.NextAction, p.Change = c.context, c.recommend, c.next, c.change
			for _, o := range c.options {
				p.Options = append(p.Options, askChoiceInformation{o.id, o.label, askEffect(o.body)})
			}
			for _, x := range c.extra {
				p.MaterialContext = append(p.MaterialContext, askContextInformation{x.title, []string{x.body}})
			}
		}
		packets = append(packets, p)
	}
	type warning struct {
		Question string `json:"question"`
		Message  string `json:"message"`
	}
	ws := make([]warning, 0, len(warnings))
	for _, w := range warnings {
		ws = append(ws, warning{w.id(), w.msg})
	}
	status, message := "ok", ""
	if len(questions) == 0 {
		status, message = "empty", "No open questions."
	}
	return struct {
		Status    string           `json:"status"`
		Questions int              `json:"questions"`
		Message   string           `json:"message,omitempty"`
		Decisions []askInformation `json:"decisions"`
		Page      string           `json:"page"`
		Warnings  []warning        `json:"warnings,omitempty"`
		Help      []string         `json:"help"`
	}{status, len(questions), message, packets, path, ws, []string{"lavish-axi " + path, "lavish-axi poll " + path}}
}

type askWarning struct{ lead, label, msg string }

func (w askWarning) id() string {
	if w.lead == "" {
		return w.label
	}
	return w.lead + "/" + w.label
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
