package skills

import (
	"strings"
	"testing"
)

func TestParseFrontMatter(t *testing.T) {
	r := strings.NewReader("---\nname: myskill\ndescription: does things\n---\nbody\n")
	m, err := ParseFrontMatter(r, "fallback", "/dir")
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "myskill" {
		t.Errorf("Name = %q, want %q", m.Name, "myskill")
	}
	if m.Description != "does things" {
		t.Errorf("Description = %q, want %q", m.Description, "does things")
	}
	if m.Dir != "/dir" {
		t.Errorf("Dir = %q, want %q", m.Dir, "/dir")
	}
}

func TestParseFrontMatterDefaultName(t *testing.T) {
	r := strings.NewReader("---\ndescription: stuff\n---\n")
	m, err := ParseFrontMatter(r, "dirname", "/d")
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "dirname" {
		t.Errorf("Name = %q, want fallback %q", m.Name, "dirname")
	}
}

func TestParseFrontMatterMissingDescription(t *testing.T) {
	r := strings.NewReader("---\nname: x\n---\n")
	_, err := ParseFrontMatter(r, "x", "/d")
	if err == nil {
		t.Error("expected error for missing description")
	}
}

func TestParseFrontMatterNoFrontMatter(t *testing.T) {
	r := strings.NewReader("just body text\n")
	_, err := ParseFrontMatter(r, "x", "/d")
	if err == nil {
		t.Error("expected error for missing front-matter")
	}
}

func TestParseFrontMatterEmpty(t *testing.T) {
	r := strings.NewReader("")
	_, err := ParseFrontMatter(r, "", "/d")
	if err == nil {
		t.Error("expected error for empty input")
	}
}

func TestParseFrontMatterBodyBeforeFence(t *testing.T) {
	r := strings.NewReader("preamble\n---\nname: s\ndescription: d\n---\n")
	m, err := ParseFrontMatter(r, "fallback", "/d")
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "s" {
		t.Errorf("Name = %q, want %q", m.Name, "s")
	}
}
