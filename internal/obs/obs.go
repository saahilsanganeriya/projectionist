// Package obs is logging and metrics: structured logs with UTC timestamps, and a tiny Prometheus text exporter.
package obs

import (
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
)

// NewLogger writes key=value ("text") or JSON lines; every timestamp is UTC.
func NewLogger(w io.Writer, format, level string) *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{
		Level: lvl,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey && len(groups) == 0 {
				a.Value = slog.StringValue(a.Value.Time().UTC().Format(time.RFC3339Nano))
			}
			return a
		},
	}
	if format == "json" {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

// Metrics holds counters and gauges keyed by name and label pairs. Label sets here are tiny and fixed
// (stage × outcome/kind), so a map is the whole implementation.
type Metrics struct {
	mu     sync.Mutex
	values map[string]float64
	types  map[string]string
}

func NewMetrics() *Metrics {
	return &Metrics{values: map[string]float64{}, types: map[string]string{}}
}

// Add increases a counter; labels are alternating name, value.
func (m *Metrics) Add(name string, v float64, labels ...string) {
	m.update(name, "counter", v, true, labels)
}

// Set replaces a gauge.
func (m *Metrics) Set(name string, v float64, labels ...string) {
	m.update(name, "gauge", v, false, labels)
}

func (m *Metrics) update(name, typ string, v float64, add bool, labels []string) {
	key := name + labelString(labels)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.types[name] = typ
	if add {
		m.values[key] += v
	} else {
		m.values[key] = v
	}
}

func labelString(labels []string) string {
	if len(labels) == 0 {
		return ""
	}
	parts := make([]string, 0, len(labels)/2)
	for i := 0; i+1 < len(labels); i += 2 {
		parts = append(parts, fmt.Sprintf("%s=%q", labels[i], labels[i+1]))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// WriteText renders the Prometheus text exposition format, sorted so output is stable.
func (m *Metrics) WriteText(w io.Writer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := make([]string, 0, len(m.values))
	for k := range m.values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	typed := map[string]bool{}
	for _, k := range keys {
		name, _, _ := strings.Cut(k, "{")
		if !typed[name] {
			fmt.Fprintf(w, "# TYPE %s %s\n", name, m.types[name])
			typed[name] = true
		}
		fmt.Fprintf(w, "%s %g\n", k, m.values[k])
	}
}
