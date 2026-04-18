// Package skills discovers and serves skill definitions from OLLIE_SKILLS_PATH.
//
// Skills are directories containing a SKILL.md file with YAML front-matter
// (name, description). They are exposed as a flat namespace of <name>.md files.
// OLLIE_SKILLS_PATH is colon-separated; the first occurrence of a name wins.
package skills

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"ollie/pkg/paths"
)

// Meta holds parsed front-matter from a SKILL.md file.
type Meta struct {
	Name        string
	Description string
	Dir         string // directory containing SKILL.md
}

// DefaultDir returns the default skills directory.
func DefaultDir() string {
	return paths.CfgDir() + "/skills"
}

// Dirs returns the skill directories from OLLIE_SKILLS_PATH,
// falling back to DefaultDir if unset.
func Dirs() []string {
	if env := os.Getenv("OLLIE_SKILLS_PATH"); env != "" {
		return strings.Split(env, ":")
	}
	return []string{DefaultDir()}
}

// List scans all skill directories and returns deduplicated metadata.
// First occurrence by directory order wins on name collision.
func List() []Meta {
	seen := make(map[string]bool)
	var skills []Meta
	for _, dir := range Dirs() {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || seen[e.Name()] {
				continue
			}
			skillDir := filepath.Join(dir, e.Name())
			meta, err := parseFrontMatter(skillDir)
			if err != nil {
				continue
			}
			seen[meta.Name] = true
			skills = append(skills, *meta)
		}
	}
	sort.Slice(skills, func(i, j int) bool { return skills[i].Name < skills[j].Name })
	return skills
}

// Read returns the SKILL.md content for the named skill.
func Read(name string) ([]byte, error) {
	for _, m := range List() {
		if m.Name == name {
			return os.ReadFile(filepath.Join(m.Dir, "SKILL.md"))
		}
	}
	return nil, os.ErrNotExist
}

func parseFrontMatter(skillDir string) (*Meta, error) {
	f, err := os.Open(filepath.Join(skillDir, "SKILL.md"))
	if err != nil {
		return nil, err
	}
	defer f.Close()

	meta := &Meta{Name: filepath.Base(skillDir), Dir: skillDir}
	scanner := bufio.NewScanner(f)
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
