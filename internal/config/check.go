package config

import (
	"fmt"
	"reflect"
	"strings"
)

// Problem is one thing a config must change before the service starts. Key names it as section.key (a list entry as
// key[n], counted from 1), Want says what to set.
type Problem struct {
	Key  string
	Want string
}

func (p Problem) String() string { return p.Key + ": " + p.Want }

// Problems is everything one validation pass found, one problem per line.
type Problems []Problem

func (ps Problems) Error() string {
	lines := make([]string, len(ps))
	for i, p := range ps {
		lines[i] = p.String()
	}
	return strings.Join(lines, "\n")
}

// Required names every field of v (a struct or a pointer to one) tagged `required:"<what to set>"` that still holds
// its zero value (an empty list counts as unset), as prefix.key, saying when it is required. It descends into nested
// tables and lists of tables, so a list entry's missing key is prefix.key[n].sub.
func Required(prefix, when string, v any) []Problem {
	rv := reflect.Indirect(reflect.ValueOf(v))
	if rv.Kind() != reflect.Struct {
		return nil
	}
	var out []Problem
	rt := rv.Type()
	for i := range rt.NumField() {
		f := rt.Field(i)
		key, _, _ := strings.Cut(f.Tag.Get("toml"), ",")
		if !f.IsExported() || key == "-" {
			continue
		}
		if key == "" {
			key = f.Name
		}
		if prefix != "" {
			key = prefix + "." + key
		}
		fv := rv.Field(i)
		if want, ok := f.Tag.Lookup("required"); ok && (fv.IsZero() || fv.Kind() == reflect.Slice && fv.Len() == 0) {
			out = append(out, Problem{Key: key, Want: requiredWant(when, want)})
			continue
		}
		switch {
		case fv.Kind() == reflect.Struct:
			out = append(out, Required(key, when, fv.Interface())...)
		case fv.Kind() == reflect.Slice && fv.Type().Elem().Kind() == reflect.Struct:
			for j := range fv.Len() {
				out = append(out, Required(fmt.Sprintf("%s[%d]", key, j+1), when, fv.Index(j).Interface())...)
			}
		}
	}
	return out
}

func requiredWant(when, want string) string {
	if when == "" {
		return "required; set " + want
	}
	return "required " + when + "; set " + want
}

// Missing is the Problem for a key that only some settings require: when says which (e.g. "when mode is sampled").
func Missing(key, when, want string) Problem {
	return Problem{Key: key, Want: requiredWant(when, want)}
}

// CheckStage decodes a stage's [stages.<name>] table into opt (which holds the stage's defaults) and names every
// required key the table leaves unset. A table that does not decode is one problem, and decoded is false: the stage's
// other rules would only read its defaults.
func CheckStage(name string, decode func(any) error, opt any) (ps []Problem, decoded bool) {
	if err := decode(opt); err != nil {
		return []Problem{{Key: "stages." + name, Want: strings.TrimPrefix(err.Error(), "stages."+name+": ")}}, false
	}
	return Required("stages."+name, EnabledWhen(name), opt), true
}

// EnabledWhen is the condition a stage's own keys are required under.
func EnabledWhen(stage string) string { return "when stages." + stage + " is enabled" }
