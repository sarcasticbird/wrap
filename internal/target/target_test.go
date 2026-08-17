package target

import (
	"reflect"
	"strings"
	"testing"
)

type queuedRunner struct {
	outputs []string
	calls   [][]string
}

func (r *queuedRunner) Run(args ...string) (string, error) {
	r.calls = append(r.calls, append([]string(nil), args...))
	if len(r.outputs) == 0 {
		return "", nil
	}
	out := r.outputs[0]
	r.outputs = r.outputs[1:]
	return out, nil
}

func TestResolveCurrentTargetsExactWindowOnCurrentSocket(t *testing.T) {
	t.Parallel()

	const generation = "0123456789abcdef0123456789abcdef"
	runner := &queuedRunner{outputs: []string{
		"",
		generation,
		"2:$73:@123:api6:editor9:/work/api32:" + generation,
	}}
	got, err := ResolveCurrent(func(key string) string {
		if key == "TMUX" {
			return "/tmp/tmux-501/default,1234,0"
		}
		return ""
	}, runner)
	if err != nil {
		t.Fatalf("ResolveCurrent() error = %v", err)
	}
	want := Target{
		SocketPath:  "/tmp/tmux-501/default",
		Generation:  generation,
		SessionID:   "$7",
		WindowID:    "@12",
		SessionName: "api",
		WindowName:  "editor",
		Directory:   "/work/api",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ResolveCurrent() = %+v, want %+v", got, want)
	}
	for index, call := range runner.calls {
		if len(call) < 2 || call[0] != "-S" || call[1] != "/tmp/tmux-501/default" {
			t.Fatalf("call %d did not use exact socket path: %v", index, call)
		}
	}
	lastCall := runner.calls[len(runner.calls)-1]
	if format := lastCall[len(lastCall)-1]; !strings.Contains(format, "#{n:session_name}:#{session_name}") {
		t.Fatalf("target format is not length-prefixed: %q", format)
	}
}

func TestParseTargetAcceptsTabsInNamesAndDirectory(t *testing.T) {
	t.Parallel()
	const generation = "0123456789abcdef0123456789abcdef"
	output := "2:$73:@128:api\tteam10:editor\tlog13:/work/api\ttab32:" + generation
	got, err := parseTarget("/tmp/tmux.sock", generation, output)
	if err != nil {
		t.Fatalf("parseTarget() = %v", err)
	}
	if got.SessionName != "api\tteam" || got.WindowName != "editor\tlog" || got.Directory != "/work/api\ttab" {
		t.Fatalf("tabbed target = %+v", got)
	}
}

func TestParseLengthPrefixedFieldsCountsUTF8BytesLikeTmux(t *testing.T) {
	t.Parallel()
	fields, err := parseLengthPrefixedFields("5:café", 1)
	if err != nil {
		t.Fatalf("parseLengthPrefixedFields() = %v", err)
	}
	if !reflect.DeepEqual(fields, []string{"café"}) {
		t.Fatalf("UTF-8 fields = %q", fields)
	}
}

func TestResolveCurrentRejectsUnavailableOrMalformedTmux(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		tmux    string
		outputs []string
		wantErr string
	}{
		{name: "outside tmux", wantErr: "not inside tmux"},
		{name: "relative socket", tmux: "relative,123,0", wantErr: "absolute"},
		{name: "missing client fields", tmux: "/tmp/tmux,123", wantErr: "malformed TMUX"},
		{name: "bad target", tmux: "/tmp/tmux,123,0", outputs: []string{"", "0123456789abcdef0123456789abcdef", "$7\tapi"}, wantErr: "target fields"},
		{name: "bad session id", tmux: "/tmp/tmux,123,0", outputs: []string{"", "0123456789abcdef0123456789abcdef", "1:72:@13:api4:main4:/tmp32:0123456789abcdef0123456789abcdef"}, wantErr: "session id"},
		{name: "bad window id", tmux: "/tmp/tmux,123,0", outputs: []string{"", "0123456789abcdef0123456789abcdef", "2:$71:13:api4:main4:/tmp32:0123456789abcdef0123456789abcdef"}, wantErr: "window id"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := ResolveCurrent(func(key string) string {
				if key == "TMUX" {
					return tt.tmux
				}
				return ""
			}, &queuedRunner{outputs: append([]string(nil), tt.outputs...)})
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("ResolveCurrent() error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestResolveDefaultShellUsesExactTmuxSocket(t *testing.T) {
	t.Parallel()
	runner := &queuedRunner{outputs: []string{"/bin/sh"}}
	got, err := ResolveDefaultShell(func(key string) string {
		if key == "TMUX" {
			return "/tmp/tmux-501/default,1234,0"
		}
		return ""
	}, runner)
	if err != nil {
		t.Fatalf("ResolveDefaultShell() = %v", err)
	}
	if got != "/bin/sh" {
		t.Fatalf("default shell = %q, want /bin/sh", got)
	}
	if len(runner.calls) != 1 || strings.Join(runner.calls[0], " ") != "-S /tmp/tmux-501/default show-options -gv default-shell" {
		t.Fatalf("default shell call = %v", runner.calls)
	}
}

func TestResolveDefaultCommandUsesExactTmuxSocket(t *testing.T) {
	t.Parallel()
	runner := &queuedRunner{outputs: []string{"exec env -i /bin/zsh"}}
	got, err := ResolveDefaultCommand(func(key string) string {
		if key == "TMUX" {
			return "/tmp/tmux-501/default,1234,0"
		}
		return ""
	}, runner)
	if err != nil {
		t.Fatalf("ResolveDefaultCommand() = %v", err)
	}
	if got != "exec env -i /bin/zsh" {
		t.Fatalf("default command = %q", got)
	}
	if len(runner.calls) != 1 || strings.Join(runner.calls[0], " ") != "-S /tmp/tmux-501/default show-options -gv default-command" {
		t.Fatalf("default command call = %v", runner.calls)
	}
}

func TestTargetKeySeparatesWindowsAndServers(t *testing.T) {
	t.Parallel()

	base := Target{SocketPath: "/tmp/tmux", Generation: "0123456789abcdef0123456789abcdef", WindowID: "@1"}
	key := base.Key()
	if key == "" || key != base.Key() {
		t.Fatal("Key() is empty or unstable")
	}
	otherWindow := base
	otherWindow.WindowID = "@2"
	otherServer := base
	otherServer.Generation = "fedcba9876543210fedcba9876543210"
	if base.Key() == otherWindow.Key() || base.Key() == otherServer.Key() {
		t.Fatal("Key() did not separate window/server identity")
	}
}
