package main

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestCommandFuncsForArgsSkipsStateInitializationForHelpAndVersion(t *testing.T) {
	for _, args := range [][]string{{"help"}, {"--help"}, {"version"}, {"version", "extra"}} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			var output bytes.Buffer
			funcs := commandFuncsForArgs(args, &output, func() (*application, error) {
				t.Fatal("stateless command initialized the state store")
				return nil, nil
			})
			if err := runArgs(args, funcs); len(args) == 1 && err != nil {
				t.Fatalf("runArgs(%v) = %v", args, err)
			}
		})
	}
}

func TestRunArgsStartGrammar(t *testing.T) {
	for _, test := range []struct {
		name     string
		args     []string
		wantName string
	}{
		{name: "bare"},
		{name: "short name", args: []string{"-n", "api"}, wantName: "api"},
		{name: "long name", args: []string{"--name", "list"}, wantName: "list"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var names []string
			err := runArgs(test.args, commandFuncs{
				start: func(name string) error { names = append(names, name); return nil },
			})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(names, []string{test.wantName}) {
				t.Fatalf("start names = %v", names)
			}
		})
	}
}

func TestRunArgsManagementGrammar(t *testing.T) {
	var calls []string
	funcs := commandFuncs{
		list: func(json bool) error { calls = append(calls, "list:"+boolString(json)); return nil },
		show: func(selector string, json bool) error {
			calls = append(calls, "show:"+selector+":"+boolString(json))
			return nil
		},
		regen: func(selector string, json bool) error {
			calls = append(calls, "regen:"+selector+":"+boolString(json))
			return nil
		},
		remove:  func(selector string) error { calls = append(calls, "remove:"+selector); return nil },
		doctor:  func(json bool) error { calls = append(calls, "doctor:"+boolString(json)); return nil },
		version: func() error { calls = append(calls, "version"); return nil },
	}
	for _, args := range [][]string{
		{"list"}, {"list", "--json"},
		{"show", "api"}, {"show", "api", "--json"},
		{"regen", "01KWRAP", "--json"},
		{"remove", "api"},
		{"doctor"}, {"doctor", "--json"},
		{"version"},
	} {
		if err := runArgs(args, funcs); err != nil {
			t.Fatalf("runArgs(%v) = %v", args, err)
		}
	}
	want := []string{
		"list:false", "list:true", "show:api:false", "show:api:true",
		"regen:01KWRAP:true", "remove:api", "doctor:false", "doctor:true", "version",
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
}

func TestRunArgsRejectsCommandPayloadAndAmbiguousSyntax(t *testing.T) {
	for _, args := range [][]string{
		{"-n", "api", "--", "codex"},
		{"api"},
		{"show"},
		{"remove"},
		{"list", "extra"},
		{"show", "api", "extra"},
		{"--name"},
	} {
		err := runArgs(args, commandFuncs{})
		if err == nil {
			t.Fatalf("runArgs(%v) succeeded", args)
		}
		if args[0] == "-n" && !strings.Contains(err.Error(), "does not run commands") {
			t.Fatalf("payload error = %v", err)
		}
	}
}

func TestRunArgsPrivateCommands(t *testing.T) {
	var bootstrapName string
	var served []string
	funcs := commandFuncs{
		bootstrap: func(name string) error { bootstrapName = name; return nil },
		serve:     func(args []string) error { served = append([]string(nil), args...); return nil },
	}
	if err := runArgs([]string{"_bootstrap", "--name", "api"}, funcs); err != nil {
		t.Fatal(err)
	}
	serveArgs := []string{"--instance", "01KWRAP", "--control", "/tmp/wrap.sock", "--record-fd", "3", "--ready-fd", "4"}
	if err := runArgs(append([]string{"_serve"}, serveArgs...), funcs); err != nil {
		t.Fatal(err)
	}
	if bootstrapName != "api" || !reflect.DeepEqual(served, serveArgs) {
		t.Fatalf("private dispatch = %q, %v", bootstrapName, served)
	}
}

func TestRunArgsPropagatesCommandError(t *testing.T) {
	want := errors.New("boom")
	err := runArgs(nil, commandFuncs{start: func(string) error { return want }})
	if !errors.Is(err, want) {
		t.Fatalf("runArgs() = %v", err)
	}
}

func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}
