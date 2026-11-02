package execute

import (
	"strconv"
	"strings"
	"sync"

	"raccoonci/internal/runner"
	"raccoonci/internal/server/github"
)

// noFilePath is where GitHub puts an annotation that names no file, as its own
// runners' annotations do.
const noFilePath = ".github"

// annotationOf turns a runner event into a check run annotation: the notices,
// warnings and errors a step printed (`::error file=x,line=3::msg`) or the
// runner reported. The file and line are the command's own; without them the
// annotation is attached to the commit's .github path, line 1, like GitHub's.
func annotationOf(e runner.Event) (github.Annotation, bool) {
	var level string
	switch e.Kind {
	case runner.Notice:
		level = github.LevelNotice
	case runner.Warning:
		level = github.LevelWarning
	case runner.Error:
		level = github.LevelFailure
	default:
		return github.Annotation{}, false
	}
	msg := strings.TrimSpace(e.Text)
	if msg == "" {
		return github.Annotation{}, false
	}
	a := github.Annotation{Path: noFilePath, StartLine: 1, EndLine: 1, Level: level, Title: e.Props["title"], Message: msg}
	if f := strings.TrimPrefix(strings.TrimSpace(e.Props["file"]), "./"); f != "" && !strings.HasPrefix(f, "/") && !strings.Contains(f, "..") {
		a.Path = f
	}
	if l, err := strconv.Atoi(e.Props["line"]); err == nil && l > 0 && a.Path != noFilePath {
		a.StartLine, a.EndLine = l, l
		if end, err := strconv.Atoi(e.Props["endLine"]); err == nil && end >= l {
			a.EndLine = end
		}
	}
	return a, true
}

// annotations collects the annotations of one job instance, the first
// github.MaxAnnotations of them (what one request can carry); more is counted.
type annotations struct {
	mu      sync.Mutex
	list    []github.Annotation
	dropped int
}

func (c *annotations) add(e runner.Event) {
	a, ok := annotationOf(e)
	if !ok {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.list) >= github.MaxAnnotations {
		c.dropped++
		return
	}
	c.list = append(c.list, a)
}

func (c *annotations) get() ([]github.Annotation, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]github.Annotation(nil), c.list...), c.dropped
}
