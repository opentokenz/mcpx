package skill

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestLoadAllYAML(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "demo")
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "skill.yaml"), []byte("name: demo\nruntime: python\nentry: main.py\narguments_schema:\n  type: object\n  required: [query]\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "main.py"), []byte("print(1)\n"), 0o644)
	skills := LoadAll([]string{root}, "")
	if len(skills) != 1 || skills[0].Manifest.Name != "demo" {
		t.Fatalf("%+v", skills)
	}
	if skills[0].Manifest.ArgumentsSchema["type"] != "object" {
		t.Fatalf("arguments schema: %+v", skills[0].Manifest.ArgumentsSchema)
	}
}

func TestLoadAllSkillMD(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "brainstorming")
	_ = os.MkdirAll(dir, 0o755)
	body := "---\nname: brainstorming\ndescription: design\n---\n\n# Hello\n"
	_ = os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644)
	skills := LoadAll([]string{root}, "")
	if len(skills) != 1 {
		t.Fatalf("count %d %+v", len(skills), skills)
	}
	if skills[0].Manifest.Name != "brainstorming" || skills[0].Manifest.Runtime != "markdown" {
		t.Fatalf("%+v", skills[0].Manifest)
	}
	if skills[0].Manifest.Format != "skill_md" {
		t.Fatalf("format %s", skills[0].Manifest.Format)
	}
}

func TestLoadAllSupportsSymlinkedSkillDirectories(t *testing.T) {
	root := t.TempDir()
	realRoot := t.TempDir()
	realSkill := filepath.Join(realRoot, "shared")
	if err := os.MkdirAll(realSkill, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realSkill, "SKILL.md"), []byte("---\nname: shared\ndescription: shared skill\n---\n\n# Shared\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "shared-link")
	if err := os.Symlink(realSkill, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	skills := LoadAll([]string{root}, "")
	if len(skills) != 1 || skills[0].Manifest.Name != "shared" {
		t.Fatalf("skills=%+v", skills)
	}
	if skills[0].Dir != link {
		t.Fatalf("Skill.Dir=%q, want lexical symlink path %q", skills[0].Dir, link)
	}
}

func TestLoadAllSkipsInvalidSymlinkChildren(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{
		"broken": filepath.Join(t.TempDir(), "missing"),
		"file":   file,
	} {
		if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
	}
	loop := filepath.Join(root, "loop")
	if err := os.Symlink(loop, loop); err != nil {
		t.Skipf("symlink loop unavailable: %v", err)
	}

	if skills := LoadAll([]string{root}, ""); len(skills) != 0 {
		t.Fatalf("invalid symlinks discovered as skills: %+v", skills)
	}
}

func TestLoadAllSymlinkedSkillRejectsEntryTraversal(t *testing.T) {
	root := t.TempDir()
	realSkill := filepath.Join(t.TempDir(), "shared")
	if err := os.MkdirAll(realSkill, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realSkill, "skill.yaml"), []byte("name: shared\nruntime: python\nentry: ../outside.py\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "shared-link")
	if err := os.Symlink(realSkill, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	if skills := LoadAll([]string{root}, ""); len(skills) != 0 {
		t.Fatalf("traversal entry under symlinked skill root must be rejected: %+v", skills)
	}
}

func TestLoadAllSymlinkedSkillRejectsEntrySymlinkEscape(t *testing.T) {
	root := t.TempDir()
	realRoot := t.TempDir()
	realSkill := filepath.Join(realRoot, "shared")
	if err := os.MkdirAll(realSkill, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(realRoot, "outside.py")
	if err := os.WriteFile(outside, []byte("print('outside')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(realSkill, "main.py")); err != nil {
		t.Skipf("entry symlink unavailable: %v", err)
	}
	if err := os.WriteFile(filepath.Join(realSkill, "skill.yaml"), []byte("name: shared\nruntime: python\nentry: main.py\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "shared-link")
	if err := os.Symlink(realSkill, link); err != nil {
		t.Skipf("skill symlink unavailable: %v", err)
	}

	if skills := LoadAll([]string{root}, ""); len(skills) != 0 {
		t.Fatalf("entry symlink escape under symlinked skill root must be rejected: %+v", skills)
	}
}

func TestLoadAgentsSkillsDir(t *testing.T) {
	// Test home expansion without depending on the developer's installed packages.
	home := t.TempDir()
	t.Setenv("HOME", home)
	agents := filepath.Join(home, ".agents", "skills", "fixture")
	if err := os.MkdirAll(agents, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agents, "SKILL.md"), []byte("---\nname: fixture\ndescription: Hermetic discovery fixture.\n---\nRead instructions.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	skills := LoadAll([]string{"~/.agents/skills"}, "")
	if len(skills) == 0 {
		t.Fatal("expected to discover SKILL.md packages under ~/.agents/skills")
	}
	t.Logf("found %d skills, first=%s", len(skills), skills[0].Manifest.Name)
}

func TestConfiguredSkillRootsIntegration(t *testing.T) {
	raw := os.Getenv("MCPX_INTEGRATION_SKILL_ROOTS")
	wantRaw := os.Getenv("MCPX_INTEGRATION_SKILL_COUNT")
	if raw == "" || wantRaw == "" {
		t.Skip("set MCPX_INTEGRATION_SKILL_ROOTS and MCPX_INTEGRATION_SKILL_COUNT")
	}
	want, err := strconv.Atoi(wantRaw)
	if err != nil {
		t.Fatal(err)
	}
	loaded := LoadAll(filepath.SplitList(raw), "")
	if len(loaded) != want {
		names := make([]string, 0, len(loaded))
		for _, item := range loaded {
			names = append(names, item.Manifest.Name)
		}
		t.Fatalf("configured roots discovered %d Skills, want %d: %v", len(loaded), want, names)
	}
	names := map[string]bool{}
	for _, item := range loaded {
		if names[item.Manifest.Name] {
			t.Fatalf("duplicate Skill name %q", item.Manifest.Name)
		}
		names[item.Manifest.Name] = true
	}
}

func TestLoadAllRejectsEntryTraversal(t *testing.T) {
	root := t.TempDir()
	secret := filepath.Join(root, "secret.txt")
	if err := os.WriteFile(secret, []byte("top secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "evil")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "skill.yaml"), []byte("name: evil\nruntime: python\nentry: ../secret.txt\n"), 0o644)
	skills := LoadAll([]string{root}, "")
	if len(skills) != 0 {
		t.Fatalf("traversal entry must reject the skill: %+v", skills)
	}
}

func TestLoadAllRejectsAbsoluteEntry(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "evil")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	absoluteEntry := filepath.ToSlash(filepath.Join(root, "outside.py"))
	manifest := fmt.Sprintf("name: evil\nruntime: python\nentry: '%s'\n", absoluteEntry)
	_ = os.WriteFile(filepath.Join(dir, "skill.yaml"), []byte(manifest), 0o644)
	skills := LoadAll([]string{root}, "")
	if len(skills) != 0 {
		t.Fatalf("absolute entry must reject the skill: %+v", skills)
	}
}

func TestLoadAllAcceptsNestedEntry(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "demo")
	if err := os.MkdirAll(filepath.Join(dir, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "skill.yaml"), []byte("name: demo\nruntime: python\nentry: scripts/main.py\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "scripts", "main.py"), []byte("print(1)\n"), 0o644)
	skills := LoadAll([]string{root}, "")
	if len(skills) != 1 || skills[0].Manifest.Entry != "scripts/main.py" {
		t.Fatalf("nested entry must load: %+v", skills)
	}
}

func TestResolveEntryRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "demo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(root, "secret.md")
	if err := os.WriteFile(secret, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(dir, "SKILL.md")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	sk := Skill{Manifest: Manifest{Name: "demo", Runtime: "markdown", Entry: "SKILL.md", Format: "skill_md"}, Dir: dir}
	if _, err := ResolveEntry(sk); err == nil {
		t.Fatal("symlink escape must be rejected")
	}
	skills := LoadAll([]string{root}, "")
	if len(skills) != 0 {
		t.Fatalf("symlinked skill must be rejected: %+v", skills)
	}
}

func TestExecuteRejectsEntryTraversal(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "demo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(root, "secret.md"), []byte("secret\n"), 0o600)
	doc := Skill{Manifest: Manifest{Name: "demo", Runtime: "markdown", Entry: "../secret.md", Format: "skill_md"}, Dir: dir}
	if _, err := Execute(context.Background(), doc, root, nil); err == nil {
		t.Fatal("Execute must reject traversal doc entry")
	}
	execSK := Skill{Manifest: Manifest{Name: "demo", Runtime: "python", Entry: "../secret.py"}, Dir: dir}
	if _, err := Execute(context.Background(), execSK, root, nil); err == nil {
		t.Fatal("Execute must reject traversal executable entry")
	}
}
