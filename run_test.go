package main

import (
	"reflect"
	"testing"
)

func TestRunArgv(t *testing.T) {
	pre := []string{"/usr/bin/systemd-run", "--user", "--scope", "--slice=claude.slice", "--unit=ccorral-run-test", "--collect", "-q", "--"}
	out := func(rest ...string) []string { return append(append([]string{}, pre...), rest...) }
	tests := []struct {
		name   string
		inside bool
		cmd    string
		args   []string
		want   []string
	}{
		{"outside, no args", false, "make", nil, out("make")},
		{"outside, args", false, "npm", []string{"run", "dev"}, out("npm", "run", "dev")},
		{"outside, dashes and spaces", false, "sh", []string{"-x", "-c", "echo a b", "--", "--flag"},
			out("sh", "-x", "-c", "echo a b", "--", "--flag")},
		{"outside, dollars doubled for systemd-run", false, "sh", []string{"-c", "echo $$ ${HOME} $1"},
			out("sh", "-c", "echo $$$$ $${HOME} $$1")},
		{"inside, dollars untouched", true, "sh", []string{"-c", "echo $$ ${HOME}"}, []string{"sh", "-c", "echo $$ ${HOME}"}},
		{"inside, no args", true, "make", nil, []string{"make"}},
		{"inside, dashes and spaces", true, "sh", []string{"-x", "-c", "echo a b", "--", "--flag"},
			[]string{"sh", "-x", "-c", "echo a b", "--", "--flag"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			orig := append([]string(nil), tc.args...)
			got := runArgv(tc.inside, "/usr/bin/systemd-run", "ccorral-run-test", tc.cmd, tc.args)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("runArgv = %q, want %q", got, tc.want)
			}
			if len(tc.args) != len(orig) || (len(orig) > 0 && !reflect.DeepEqual(orig, tc.args)) {
				t.Errorf("args mutated: %q", tc.args)
			}
		})
	}
}

func TestRunMainNoCommand(t *testing.T) {
	if got := runMain(nil); got != 2 {
		t.Errorf("runMain(nil) = %d, want 2", got)
	}
}

func TestRunMainNotFound(t *testing.T) {
	if got := runMain([]string{"ccorral-no-such-command-xyz"}); got != 127 {
		t.Errorf("runMain(missing) = %d, want 127", got)
	}
}
