package logs

import "io"

type LimitWriter struct {
	w         io.Writer
	max       int64
	n         int64
	truncated bool
}

func New(w io.Writer, max int64) *LimitWriter { return &LimitWriter{w: w, max: max} }
func (l *LimitWriter) Write(p []byte) (int, error) {
	if l.max >= 0 && l.n >= l.max {
		l.truncated = true
		return len(p), nil
	}
	allowed := p
	if l.max >= 0 && int64(len(allowed)) > l.max-l.n {
		allowed = allowed[:l.max-l.n]
		l.truncated = true
	}
	n, err := l.w.Write(allowed)
	l.n += int64(n)
	if len(allowed) < len(p) {
		l.truncated = true
		return len(p), err
	}
	return len(p), err
}
func (l *LimitWriter) Truncated() bool { return l.truncated }

type Pair struct {
	Stdout *LimitWriter
	Stderr *LimitWriter
}

func NewPair(stdout, stderr io.Writer, stdoutMax, stderrMax int64) Pair {
	return Pair{Stdout: New(stdout, stdoutMax), Stderr: New(stderr, stderrMax)}
}
