// Package skills parses skill metadata from SKILL.md front-matter.
//
// Skills are directories containing a SKILL.md file with YAML front-matter
// (name, description). This package provides the parser and an in-memory
// registry; all filesystem access is the caller's responsibility.
package skills

import (
	"bufio"
	"io"
	"os"
	"strings"
)

// Meta holds parsed front-matter from a SKILL.md file.
type Meta struct {
	Name        string
	Description string
	Dir         string // directory containing SKILL.md
}

// ParseFrontMatter reads YAML front-matter from r, extracting name and
// description fields. defaultName and dir are used as fallbacks/metadata.
func ParseFrontMatter(r io.Reader, defaultName, dir string) (*Meta, error) {
	meta := &Meta{Name: defaultName, Dir: dir}
	scanner := bufio.NewScanner(r)
	inFM := false
	for scanner.Scan() {
		line := scanner.Text()
		if line == "---" {
			if !inFM {
				inFM = true
				continue
			}
			break
		}
		if !inFM {
			continue
		}
		if v, ok := strings.CutPrefix(line, "name:"); ok {
			meta.Name = strings.TrimSpace(v)
		} else if v, ok := strings.CutPrefix(line, "description:"); ok {
			meta.Description = strings.TrimSpace(v)
		}
	}
	if meta.Name == "" || meta.Description == "" {
		return nil, os.ErrNotExist
	}
	return meta, nil
}
