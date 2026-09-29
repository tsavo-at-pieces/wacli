package transcribe

import (
	"reflect"
	"strings"
	"testing"
)

func TestSplitCommand(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want []string
	}{
		{name: "empty", in: "", want: nil},
		{name: "blank", in: " \t\n ", want: nil},
		{name: "plain", in: "stt --input {wav}", want: []string{"stt", "--input", "{wav}"}},
		{name: "runs of whitespace", in: "  stt \t --fast\n{wav}  ", want: []string{"stt", "--fast", "{wav}"}},
		{name: "single quotes keep spaces", in: "'/opt/My Tools/stt' {wav}", want: []string{"/opt/My Tools/stt", "{wav}"}},
		{name: "double quotes keep spaces", in: `stt --prompt "hello there" {wav}`, want: []string{"stt", "--prompt", "hello there", "{wav}"}},
		{name: "adjacent segments join", in: `stt --opt='a b'"c d"e {wav}`, want: []string{"stt", "--opt=a bc de", "{wav}"}},
		{name: "empty quotes are an argument", in: `stt '' "" {wav}`, want: []string{"stt", "", "", "{wav}"}},
		{name: "other quote inside quotes", in: `stt '"x"' "it's" {wav}`, want: []string{"stt", `"x"`, "it's", "{wav}"}},
		{name: "no variable expansion", in: `stt $HOME "$HOME" '${X:-y}' ~/model {wav}`, want: []string{"stt", "$HOME", "$HOME", "${X:-y}", "~/model", "{wav}"}},
		{name: "no shell operators", in: `stt {wav} | tee out; rm -rf x`, want: []string{"stt", "{wav}", "|", "tee", "out;", "rm", "-rf", "x"}},
		{name: "backslashes are literal", in: `C:\Tools\stt.exe "C:\Models\m.bin" {wav}`, want: []string{`C:\Tools\stt.exe`, `C:\Models\m.bin`, "{wav}"}},
		{name: "unicode", in: "stt --lang 日本語 {wav}", want: []string{"stt", "--lang", "日本語", "{wav}"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SplitCommand(tc.in)
			if err != nil {
				t.Fatalf("SplitCommand(%q): %v", tc.in, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("SplitCommand(%q) = %#v, want %#v", tc.in, got, tc.want)
			}
		})
	}
}

func TestSplitCommandRejectsUnterminatedQuotes(t *testing.T) {
	for _, in := range []string{`stt 'open {wav}`, `stt "open {wav}`, `stt '"' "`} {
		if _, err := SplitCommand(in); err == nil || !strings.Contains(err.Error(), "unterminated") {
			t.Fatalf("SplitCommand(%q) error = %v, want unterminated quote", in, err)
		}
	}
}
