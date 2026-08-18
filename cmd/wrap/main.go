package main

import (
	"errors"
	"fmt"
	"os"
	"runtime/debug"
)

var version = "dev"

func init() {
	if version != "dev" {
		return
	}
	info, ok := debug.ReadBuildInfo()
	if ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		version = info.Main.Version
	}
}

func main() {
	args := os.Args[1:]
	if err := runArgs(args, defaultCommandFuncs(args)); err != nil {
		fmt.Fprintln(os.Stderr, "wrap:", safeHumanText(err.Error()))
		os.Exit(1)
	}
}

type commandFuncs struct {
	start     func(string) error
	list      func(bool) error
	show      func(string, bool) error
	regen     func(string, bool) error
	remove    func(string) error
	doctor    func(bool) error
	version   func() error
	help      func() error
	bootstrap func(string) error
	serve     func([]string) error
}

func runArgs(args []string, funcs commandFuncs) error {
	if len(args) == 0 {
		return callStart(funcs.start, "")
	}
	switch args[0] {
	case "-n", "--name":
		if len(args) != 2 || args[1] == "" {
			if len(args) > 2 {
				return errors.New("wrap does not run commands; it shares the current tmux window")
			}
			return errors.New("-n/--name requires one nonempty name")
		}
		return callStart(funcs.start, args[1])
	case "list":
		jsonOutput, err := parseOptionalJSON(args[1:])
		if err != nil {
			return fmt.Errorf("usage: wrap list [--json]: %w", err)
		}
		return callBool(funcs.list, jsonOutput)
	case "show", "regen":
		if len(args) < 2 || args[1] == "" {
			return fmt.Errorf("usage: wrap %s INSTANCE [--json]", args[0])
		}
		jsonOutput, err := parseOptionalJSON(args[2:])
		if err != nil {
			return fmt.Errorf("usage: wrap %s INSTANCE [--json]: %w", args[0], err)
		}
		if args[0] == "show" {
			if funcs.show == nil {
				return errors.New("show command is unavailable")
			}
			return funcs.show(args[1], jsonOutput)
		}
		if funcs.regen == nil {
			return errors.New("regen command is unavailable")
		}
		return funcs.regen(args[1], jsonOutput)
	case "remove":
		if len(args) != 2 || args[1] == "" {
			return errors.New("usage: wrap remove INSTANCE")
		}
		if funcs.remove == nil {
			return errors.New("remove command is unavailable")
		}
		return funcs.remove(args[1])
	case "doctor":
		jsonOutput, err := parseOptionalJSON(args[1:])
		if err != nil {
			return fmt.Errorf("usage: wrap doctor [--json]: %w", err)
		}
		return callBool(funcs.doctor, jsonOutput)
	case "version":
		if len(args) != 1 {
			return errors.New("usage: wrap version")
		}
		return callNoArgs(funcs.version, "version")
	case "help", "-h", "--help":
		if len(args) != 1 {
			return errors.New("usage: wrap help")
		}
		return callNoArgs(funcs.help, "help")
	case "_bootstrap":
		name, err := parsePrivateName(args[1:])
		if err != nil {
			return err
		}
		if funcs.bootstrap == nil {
			return errors.New("bootstrap command is unavailable")
		}
		return funcs.bootstrap(name)
	case "_serve":
		if funcs.serve == nil {
			return errors.New("worker command is unavailable")
		}
		return funcs.serve(args[1:])
	default:
		return fmt.Errorf("unknown command %q; Wrap does not run commands", args[0])
	}
}

func parseOptionalJSON(args []string) (bool, error) {
	switch len(args) {
	case 0:
		return false, nil
	case 1:
		if args[0] == "--json" {
			return true, nil
		}
	}
	return false, errors.New("unexpected arguments")
}

func parsePrivateName(args []string) (string, error) {
	if len(args) == 0 {
		return "", nil
	}
	if len(args) == 2 && (args[0] == "-n" || args[0] == "--name") && args[1] != "" {
		return args[1], nil
	}
	return "", errors.New("invalid internal bootstrap arguments")
}

func callStart(fn func(string) error, name string) error {
	if fn == nil {
		return errors.New("start command is unavailable")
	}
	return fn(name)
}

func callBool(fn func(bool) error, value bool) error {
	if fn == nil {
		return errors.New("command is unavailable")
	}
	return fn(value)
}

func callNoArgs(fn func() error, name string) error {
	if fn == nil {
		return fmt.Errorf("%s command is unavailable", name)
	}
	return fn()
}
