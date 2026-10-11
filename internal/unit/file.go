// Package unit reads systemd unit files, and the [Service] section of a
// service unit in particular, closely enough to run the services they
// describe without systemd.
package unit

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// File holds a unit file's directives by section and key. Every assignment is
// kept in order, including empty ones, because an empty assignment resets a
// list-valued directive to empty.
type File map[string]map[string][]string

// Parse reads a unit file. Comments start with '#' or ';', and a line ending
// in a backslash continues on the next one, joined by a space as systemd
// does.
func Parse(r io.Reader) (File, error) {
	f := File{}
	section := ""
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNo := 0
	var pending strings.Builder

	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())

		if pending.Len() > 0 {
			// systemd skips comment lines inside a continuation.
			if strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
				continue
			}
		} else if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}

		if strings.HasSuffix(line, "\\") {
			pending.WriteString(strings.TrimSuffix(line, "\\"))
			pending.WriteByte(' ')
			continue
		}
		if pending.Len() > 0 {
			pending.WriteString(line)
			line = strings.TrimSpace(pending.String())
			pending.Reset()
		}

		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = line[1 : len(line)-1]
			if f[section] == nil {
				f[section] = map[string][]string{}
			}
			continue
		}

		if section == "" {
			return nil, fmt.Errorf("line %d: assignment outside of a section", lineNo)
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("line %d: expected Key=Value, got %q", lineNo, line)
		}
		key = strings.TrimSpace(key)
		f[section][key] = append(f[section][key], strings.TrimSpace(value))
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if pending.Len() > 0 {
		return nil, fmt.Errorf("line %d: file ends inside a continuation", lineNo)
	}
	return f, nil
}

// Merge appends the assignments in o after f's, the way a drop-in applies on
// top of the unit it extends.
func (f File) Merge(o File) {
	for section, keys := range o {
		if f[section] == nil {
			f[section] = map[string][]string{}
		}
		for key, values := range keys {
			f[section][key] = append(f[section][key], values...)
		}
	}
}

// Value returns the last assignment of a single-valued directive.
func (f File) Value(section, key string) (string, bool) {
	values := f[section][key]
	if len(values) == 0 {
		return "", false
	}
	return values[len(values)-1], true
}

// Values returns the assignments of a list-valued directive that follow its
// last empty assignment.
func (f File) Values(section, key string) []string {
	values := f[section][key]
	for i := len(values) - 1; i >= 0; i-- {
		if values[i] == "" {
			return values[i+1:]
		}
	}
	return values
}

// Load reads the unit at path and applies its drop-ins: the *.conf files in
// "<path>.d" and in "<dir>/<unit name>.d" for each of dropInDirs, in
// lexical order of file name across all of them. Later directories win on a
// file name they share, like /etc over /usr/lib.
func Load(path string, dropInDirs ...string) (File, error) {
	f, err := parseFile(path)
	if err != nil {
		return nil, err
	}

	name := filepath.Base(path)
	dirs := []string{path + ".d"}
	for _, dir := range dropInDirs {
		dirs = append(dirs, filepath.Join(dir, name+".d"))
	}

	confs := map[string]string{}
	for _, dir := range dirs {
		matches, err := filepath.Glob(filepath.Join(dir, "*.conf"))
		if err != nil {
			return nil, err
		}
		for _, m := range matches {
			confs[filepath.Base(m)] = m
		}
	}
	names := make([]string, 0, len(confs))
	for n := range confs {
		names = append(names, n)
	}
	sort.Strings(names)

	for _, n := range names {
		d, err := parseFile(confs[n])
		if err != nil {
			return nil, err
		}
		f.Merge(d)
	}
	return f, nil
}

func parseFile(path string) (File, error) {
	r, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer r.Close()

	f, err := Parse(r)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return f, nil
}
