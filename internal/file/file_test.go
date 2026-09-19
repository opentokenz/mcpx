package file

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestResolveEscape(t *testing.T) {
	root := t.TempDir()
	if _, err := Resolve(root, "../x"); err == nil {
		t.Fatal("expected escape error")
	}
	if _, err := Resolve(root, "a/b.txt"); err != nil {
		t.Fatal(err)
	}
}

func TestResolveRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "outside")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := Resolve(root, "outside/secret.txt"); err == nil {
		t.Fatal("expected symlink escape error")
	}
}

func TestLexicalPathPreservesFinalSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.txt")
	link := filepath.Join(root, "link.txt")
	if err := os.WriteFile(target, []byte("target"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target.txt", link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	lexical, err := LexicalPath(root, "link.txt")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(lexical)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("lexical path followed symlink: %s", lexical)
	}
	resolved, err := Resolve(root, "link.txt")
	if err != nil {
		t.Fatal(err)
	}
	if resolved == lexical {
		t.Fatalf("Resolve should follow existing symlink: %s", resolved)
	}
}

func TestReadReturnsFileContent(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "f.txt")
	if err := os.WriteFile(path, []byte("hello world\nline2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := Read(ReadOptions{WorkspaceRoot: root, Path: "f.txt", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Content, "hello") {
		t.Fatal(r.Content)
	}
}

func TestReadContinuesWithinLongUTF8Line(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "long.txt"), []byte("abcdef\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	first, err := Read(ReadOptions{WorkspaceRoot: root, Path: "long.txt", Offset: 0, Limit: 1, MaxBytes: 3})
	if err != nil {
		t.Fatal(err)
	}
	if first.Content != "abc" || !first.Truncated {
		t.Fatalf("first fragment=%+v", first)
	}
	second, err := Read(ReadOptions{WorkspaceRoot: root, Path: "long.txt", Offset: 0, LineByteOffset: 3, Limit: 1, MaxBytes: 3})
	if err != nil {
		t.Fatal(err)
	}
	if second.Content != "def" || !second.Truncated {
		t.Fatalf("second fragment=%+v", second)
	}
	third, err := Read(ReadOptions{WorkspaceRoot: root, Path: "long.txt", Offset: 0, LineByteOffset: 6, Limit: 1, MaxBytes: 3})
	if err != nil {
		t.Fatal(err)
	}
	if third.Content != "\n" || third.Truncated {
		t.Fatalf("final fragment=%+v", third)
	}
	if first.Content+second.Content+third.Content != "abcdef\n" {
		t.Fatalf("fragments did not reconstruct source")
	}
}

func TestReadRejectsLineByteOffsetInsideUTF8Rune(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "unicode.txt"), []byte("你a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(ReadOptions{WorkspaceRoot: root, Path: "unicode.txt", Offset: 0, LineByteOffset: 1, Limit: 1, MaxBytes: 8}); err == nil {
		t.Fatal("line_byte_offset inside a UTF-8 rune must be rejected")
	}
}

func TestReadReportsLineEnding(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "crlf.java"), []byte("class Demo {\r\n}\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	read, err := Read(ReadOptions{WorkspaceRoot: root, Path: "crlf.java", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if read.LineEnding != "CRLF" {
		t.Fatalf("line ending = %q, want CRLF", read.LineEnding)
	}
	if read.Format.Charset != "utf-8" || read.Format.BOM != "none" || read.Format.LineEnding != "CRLF" {
		t.Fatalf("window format = %+v", read.Format)
	}
	if read.Format.LineEndingCounts != (LineEndingCounts{CRLF: 2}) || read.Format.FinalNewline == nil || !*read.Format.FinalNewline {
		t.Fatalf("window format details = %+v", read.Format)
	}

	full, err := ReadFull(FullReadOptions{WorkspaceRoot: root, Path: "crlf.java", MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if full.LineEnding != "CRLF" {
		t.Fatalf("full line ending = %q, want CRLF", full.LineEnding)
	}
	if !reflect.DeepEqual(read.Format, full.Format) {
		t.Fatalf("window/full format mismatch: window=%+v full=%+v", read.Format, full.Format)
	}
}

func TestReadFullReportsCharsetBOMAndFinalNewline(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		name       string
		content    []byte
		charset    string
		bom        string
		lineEnding string
		final      *bool
	}{
		{name: "utf8 bom", content: append([]byte{0xef, 0xbb, 0xbf}, []byte("one\n")...), charset: "utf-8", bom: "utf-8", lineEnding: "LF", final: boolPointerForTest(true)},
		{name: "utf8 no final newline", content: []byte("one\r\ntwo"), charset: "utf-8", bom: "none", lineEnding: "CRLF", final: boolPointerForTest(false)},
		{name: "cr final newline", content: []byte("one\rtwo\r"), charset: "utf-8", bom: "none", lineEnding: "CR", final: boolPointerForTest(true)},
		{name: "mixed", content: []byte("one\r\ntwo\nthree\rfour"), charset: "utf-8", bom: "none", lineEnding: "mixed", final: boolPointerForTest(false)},
		{name: "utf16le", content: []byte{0xff, 0xfe, 'o', 0, 'k', 0}, charset: "utf-16le", bom: "utf-16le", lineEnding: "none", final: boolPointerForTest(false)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(root, tc.name+".txt")
			if err := os.WriteFile(path, tc.content, 0o644); err != nil {
				t.Fatal(err)
			}
			result, err := ReadFull(FullReadOptions{WorkspaceRoot: root, Path: tc.name + ".txt", MaxBytes: 1 << 20})
			if err != nil {
				t.Fatal(err)
			}
			if result.Format.Charset != tc.charset || result.Format.BOM != tc.bom || result.Format.LineEnding != tc.lineEnding {
				t.Fatalf("format = %+v", result.Format)
			}
			if tc.final == nil {
				if result.Format.FinalNewline != nil {
					t.Fatalf("final_newline = %v, want null", result.Format.FinalNewline)
				}
			} else if result.Format.FinalNewline == nil || *result.Format.FinalNewline != *tc.final {
				t.Fatalf("final_newline = %v, want %v", result.Format.FinalNewline, *tc.final)
			}
		})
	}
}

func boolPointerForTest(value bool) *bool { return &value }

func TestReadDecodesUTF16WindowAndPreservesRawSHA(t *testing.T) {
	root := t.TempDir()
	text := "one\r\ntwo\n😀\n"
	units := utf16.Encode([]rune(text))
	content := []byte{0xff, 0xfe}
	for _, unit := range units {
		var encoded [2]byte
		binary.LittleEndian.PutUint16(encoded[:], unit)
		content = append(content, encoded[:]...)
	}
	if err := os.WriteFile(filepath.Join(root, "utf16.txt"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	window, err := Read(ReadOptions{WorkspaceRoot: root, Path: "utf16.txt", Offset: 1, Limit: 1, MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if window.Content != "two\n" || window.TotalLines != 3 || window.Format.Charset != "utf-16le" || window.Format.LineEnding != "mixed" {
		t.Fatalf("decoded window=%+v", window)
	}
	fragment, err := Read(ReadOptions{WorkspaceRoot: root, Path: "utf16.txt", Offset: 1, LineByteOffset: 2, Limit: 1, MaxBytes: 2})
	if err != nil {
		t.Fatal(err)
	}
	if fragment.Content != "o\n" || !fragment.Truncated {
		t.Fatalf("utf16 line-byte continuation=%+v", fragment)
	}
	full, err := ReadFull(FullReadOptions{WorkspaceRoot: root, Path: "utf16.txt", MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if got, format, decodeErr := DecodeText(full.Content); decodeErr != nil || got != text || format.Charset != "utf-16le" {
		t.Fatalf("decoded full=%q format=%+v err=%v", got, format, decodeErr)
	}
	if window.SHA256 == "" || window.SHA256 != full.SHA256 {
		t.Fatalf("raw SHA mismatch: window=%q full=%q", window.SHA256, full.SHA256)
	}
}

func TestReadUTF16WindowLongLineKeepsRetainedHeapBounded(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "utf16-long.txt")
	handle, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handle.Write([]byte{0xff, 0xfe}); err != nil {
		handle.Close()
		t.Fatal(err)
	}
	block := make([]byte, 64<<10)
	for index := 0; index < len(block); index += 2 {
		block[index] = 'a'
	}
	remaining := 8 << 20
	for remaining > 0 {
		write := len(block)
		if write > remaining {
			write = remaining
		}
		if _, err := handle.Write(block[:write]); err != nil {
			handle.Close()
			t.Fatal(err)
		}
		remaining -= write
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	block = nil
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	result, err := Read(ReadOptions{
		WorkspaceRoot:  root,
		Path:           "utf16-long.txt",
		Offset:         0,
		LineByteOffset: 2 << 20,
		Limit:          1,
		MaxBytes:       64,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Content != strings.Repeat("a", 64) || !result.Truncated || result.TotalLines != 1 {
		t.Fatalf("bounded UTF-16 window=%+v", result)
	}

	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(result)
	if after.HeapAlloc > before.HeapAlloc+(1<<20) {
		t.Fatalf("UTF-16 64-byte window retained too much heap: before=%d after=%d delta=%d", before.HeapAlloc, after.HeapAlloc, after.HeapAlloc-before.HeapAlloc)
	}
}

func TestDecodeTextRejectsMalformedUTF16(t *testing.T) {
	if _, _, err := DecodeText([]byte{0xff, 0xfe, 0x00}); err == nil {
		t.Fatal("odd UTF-16 byte length should be rejected")
	}
	if _, _, err := DecodeText([]byte{0xff, 0xfe, 0x00, 0xd8}); err == nil {
		t.Fatal("unpaired UTF-16 surrogate should be rejected")
	}
}

func TestReadFullReturnsWholeTextAndMIME(t *testing.T) {
	root := t.TempDir()
	content := []byte("<!doctype html>\n<html><body>preview</body></html>\n")
	if err := os.WriteFile(filepath.Join(root, "preview.html"), content, 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := ReadFull(FullReadOptions{WorkspaceRoot: root, Path: "preview.html", MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Content) != string(content) || result.MIMEType != "text/html" || result.Size != int64(len(content)) {
		t.Fatalf("full read=%+v", result)
	}
	if !strings.HasPrefix(result.SHA256, "sha256:") {
		t.Fatalf("missing full-file hash: %q", result.SHA256)
	}
}

func TestReadFullTreatsTypeScriptAsSourceText(t *testing.T) {
	root := t.TempDir()
	content := []byte("const answer: number = 42;\n")
	if err := os.WriteFile(filepath.Join(root, "engine.ts"), content, 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := ReadFull(FullReadOptions{WorkspaceRoot: root, Path: "engine.ts", MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if result.MIMEType != "text/typescript" || result.Format.Charset != "utf-8" || string(result.Content) != string(content) {
		t.Fatalf("TypeScript full read=%+v", result)
	}
}

func TestReadFullPreservesBinaryAndRejectsOversize(t *testing.T) {
	root := t.TempDir()
	content := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0xff}
	if err := os.WriteFile(filepath.Join(root, "preview.png"), content, 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := ReadFull(FullReadOptions{WorkspaceRoot: root, Path: "preview.png", MaxBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Content) != string(content) || result.MIMEType != "image/png" {
		t.Fatalf("binary full read=%+v", result)
	}
	if _, err := ReadFull(FullReadOptions{WorkspaceRoot: root, Path: "preview.png", MaxBytes: 4}); err == nil {
		t.Fatal("oversize full read should fail")
	}
}

func TestSnapshotDiff(t *testing.T) {
	root := t.TempDir()
	_ = os.WriteFile(filepath.Join(root, "a.txt"), []byte("1"), 0o644)
	s1, err := TakeSnapshot(root)
	if err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(root, "a.txt"), []byte("2"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "b.txt"), []byte("n"), 0o644)
	s2, err := TakeSnapshot(root)
	if err != nil {
		t.Fatal(err)
	}
	ch := DiffSnapshots(s1, s2)
	if len(ch) < 1 {
		t.Fatal(ch)
	}
}
