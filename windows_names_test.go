package lmget

import "testing"

func TestWindowsUnsafeNames(t *testing.T) {
	for _, bad := range []string{"NUL", "nul.json", "Con", "com1.txt", "LPT9", "COM¹", "conin$",
		"a:b", "model.safetensors:hidden", "x?.json", "a|b", "trailing.", "trailing ", "GIT~1", "git~12", "PROGRA~1.TXT"} {
		if !windowsUnsafeName(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
	for _, ok := range []string{"config.json", "nullable.json", "console.py", "com10", "model-00001-of-00002.safetensors",
		"tokenizer.json", "a~b", "file~.txt", ".gitattributes", "LPT"} {
		if windowsUnsafeName(ok) {
			t.Errorf("%q refused", ok)
		}
	}
}
