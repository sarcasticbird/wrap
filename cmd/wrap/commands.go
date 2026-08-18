package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/sarcasticbird/wrap/internal/control"
	"github.com/sarcasticbird/wrap/internal/doctor"
	"github.com/sarcasticbird/wrap/internal/instance"
	"github.com/sarcasticbird/wrap/internal/mirror"
	"github.com/sarcasticbird/wrap/internal/share"
	"github.com/sarcasticbird/wrap/internal/target"
	"github.com/sarcasticbird/wrap/internal/tmux"
	"golang.org/x/sys/unix"
)

const usage = `Wrap shares one tmux window through an encrypted browser terminal.

Usage:
  wrap [-n NAME]
  wrap list [--json]
  wrap show INSTANCE [--json]
  wrap regen INSTANCE [--json]
  wrap remove INSTANCE
  wrap doctor [--json]
  wrap version`

const defaultManagementWait = share.WorkerStopTimeout + 5*time.Second

var externalProbeTimeout = 5 * time.Second

type application struct {
	out io.Writer
	err io.Writer

	store          instance.Store
	getenv         func(string) string
	getwd          func() (string, error)
	evalSymlinks   func(string) (string, error)
	executable     func() (string, error)
	lookPath       func(string) (string, error)
	runCommand     func(context.Context, string, ...string) (string, error)
	now            func() time.Time
	resolveTarget  func(func(string) string, tmux.Runner) (target.Target, error)
	resolveShell   func(func(string) string, tmux.Runner) (string, error)
	resolveCommand func(func(string) string, tmux.Runner) (string, error)
	createSession  func(string, string, tmux.Runner) error
	startWorker    func(context.Context, string, instance.Record) (control.Status, error)
	callControl    func(context.Context, string, control.Request) (control.Status, error)
	cleanupStale   func(instance.Record) error
	replaceProcess func(string, []string, []string) error
	startContext   func() (context.Context, context.CancelFunc)
	random         io.Reader
	managementWait time.Duration
}

func defaultCommandFuncs(args []string) commandFuncs {
	return commandFuncsForArgs(args, os.Stdout, newApplication)
}

func commandFuncsForArgs(args []string, out io.Writer, createApplication func() (*application, error)) commandFuncs {
	if len(args) > 0 {
		switch args[0] {
		case "version":
			return commandFuncs{version: func() error { _, err := fmt.Fprintln(out, "wrap", version); return err }}
		case "help", "-h", "--help":
			return commandFuncs{help: func() error { _, err := fmt.Fprintln(out, usage); return err }}
		}
	}
	app, err := createApplication()
	if err != nil {
		return commandFuncs{
			start: func(string) error { return err }, list: func(bool) error { return err },
			show: func(string, bool) error { return err }, regen: func(string, bool) error { return err },
			remove: func(string) error { return err }, doctor: func(bool) error { return err },
			version: func() error { return err }, help: func() error { return err },
			bootstrap: func(string) error { return err }, serve: func([]string) error { return err },
		}
	}
	return app.funcs()
}

func newApplication() (*application, error) {
	store, err := storeFromEnvironment(os.Getenv, os.UserHomeDir)
	if err != nil {
		return nil, err
	}
	return &application{
		out:          os.Stdout,
		err:          os.Stderr,
		store:        store,
		getenv:       os.Getenv,
		getwd:        os.Getwd,
		evalSymlinks: filepath.EvalSymlinks,
		executable:   os.Executable,
		lookPath:     exec.LookPath,
		runCommand: func(ctx context.Context, path string, args ...string) (string, error) {
			output, err := exec.CommandContext(ctx, path, args...).CombinedOutput()
			return string(output), err
		},
		now:            time.Now,
		resolveTarget:  target.ResolveCurrent,
		resolveShell:   target.ResolveDefaultShell,
		resolveCommand: target.ResolveDefaultCommand,
		createSession:  target.CreateDefaultSession,
		startWorker:    share.Start,
		callControl:    control.Call,
		replaceProcess: syscall.Exec,
		startContext: func() (context.Context, context.CancelFunc) {
			return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		},
		random:         rand.Reader,
		managementWait: defaultManagementWait,
	}, nil
}

func (a *application) funcs() commandFuncs {
	return commandFuncs{
		start: a.start, list: a.list, show: a.show, regen: a.regen,
		remove: a.remove, doctor: a.doctor,
		version:   func() error { _, err := fmt.Fprintln(a.out, "wrap", version); return err },
		help:      func() error { _, err := fmt.Fprintln(a.out, usage); return err },
		bootstrap: a.bootstrap,
		serve:     a.serve,
	}
}

func (a *application) start(name string) error {
	if name != "" {
		if err := instance.ValidateName(name); err != nil {
			return err
		}
	}
	if a.getenv("TMUX") != "" {
		ctx, stop := a.startContext()
		defer stop()
		_, err := a.startInside(ctx, name)
		return err
	}
	if err := a.checkStartDependencies(context.Background()); err != nil {
		return err
	}
	cwd, err := a.getwd()
	if err != nil {
		return fmt.Errorf("get current directory: %w", err)
	}
	physical, err := a.evalSymlinks(cwd)
	if err != nil {
		return fmt.Errorf("resolve physical current directory: %w", err)
	}
	executable, err := a.executable()
	if err != nil {
		return fmt.Errorf("resolve Wrap executable: %w", err)
	}
	command := shellQuote(executable) + " _bootstrap"
	if name != "" {
		command += " --name " + shellQuote(name)
	}
	return a.createSession(physical, command, nil)
}

func (a *application) startInside(ctx context.Context, requestedName string) (control.Status, error) {
	tmuxTarget, err := a.resolveTarget(a.getenv, nil)
	if err != nil {
		return control.Status{}, fmt.Errorf("resolve current tmux window: %w", err)
	}
	inspected, problems, err := a.inspectRecords(ctx, true)
	if err != nil {
		return control.Status{}, err
	}
	for _, problem := range problems {
		if _, writeErr := fmt.Fprintln(a.err, "wrap: malformed instance record:", safeHumanText(problem.Error())); writeErr != nil {
			return control.Status{}, fmt.Errorf("write malformed record warning: %w", writeErr)
		}
	}
	records := make([]instance.Record, 0, len(inspected))
	for _, item := range inspected {
		records = append(records, item.record)
	}
	for _, item := range inspected {
		record := item.record
		if record.Target.Key() != tmuxTarget.Key() {
			continue
		}
		if item.err != nil {
			return control.Status{}, fmt.Errorf("existing Wrap %q is unreachable: %w", record.Name, item.err)
		}
		if requestedName != "" && requestedName != record.Name {
			for _, other := range records {
				if other.ID != record.ID && other.Name == requestedName {
					return control.Status{}, fmt.Errorf("instance name %q already exists", requestedName)
				}
			}
			status, renameErr := a.callControl(ctx, record.ControlSocket, control.Request{
				InstanceID: record.ID, Action: control.ActionRename, Name: requestedName,
			})
			if renameErr != nil {
				return control.Status{}, fmt.Errorf("rename Wrap %q: %w", record.Name, renameErr)
			}
			if err := writeStatus(a.out, status, false); err != nil {
				return control.Status{}, err
			}
			return status, nil
		}
		if err := writeStatus(a.out, item.status, false); err != nil {
			return control.Status{}, err
		}
		return item.status, nil
	}
	if err := a.checkStartDependencies(ctx); err != nil {
		return control.Status{}, err
	}
	name, err := instance.ChooseName(requestedName, tmuxTarget, records)
	if err != nil {
		return control.Status{}, err
	}
	id, err := newInstanceID(a.random)
	if err != nil {
		return control.Status{}, err
	}
	executable, err := a.executable()
	if err != nil {
		return control.Status{}, fmt.Errorf("resolve Wrap executable: %w", err)
	}
	record := instance.Record{
		Version:       instance.RecordVersion,
		ID:            id,
		Name:          name,
		PID:           1,
		ControlSocket: filepath.Join(a.store.RuntimeRoot, id+".sock"),
		StartedAt:     a.now().UTC(),
		Directory:     tmuxTarget.Directory,
		Target:        tmuxTarget,
	}
	status, err := a.startWorker(ctx, executable, record)
	if err != nil {
		return control.Status{}, err
	}
	if err := writeStatus(a.out, status, false); err != nil {
		return control.Status{}, err
	}
	return status, nil
}

func (a *application) list(jsonOutput bool) error {
	inspected, problems, err := a.inspectRecords(context.Background(), true)
	if err != nil {
		return err
	}
	for _, problem := range problems {
		if _, writeErr := fmt.Fprintln(a.err, "wrap: malformed instance record:", safeHumanText(problem.Error())); writeErr != nil {
			return fmt.Errorf("write malformed record warning: %w", writeErr)
		}
	}
	type listItem struct {
		ID        string        `json:"id"`
		Name      string        `json:"name"`
		State     string        `json:"state"`
		Directory string        `json:"directory"`
		Clients   int           `json:"clients"`
		StartedAt time.Time     `json:"started_at"`
		Target    target.Target `json:"target"`
	}
	items := make([]listItem, 0, len(inspected))
	for _, item := range inspected {
		if item.err != nil {
			if _, writeErr := fmt.Fprintf(a.err, "wrap: %s is unreachable: %s\n",
				safeHumanLabel(item.record.Name), safeHumanText(item.err.Error())); writeErr != nil {
				return fmt.Errorf("write unreachable instance warning: %w", writeErr)
			}
			continue
		}
		status := item.status
		items = append(items, listItem{
			ID: status.ID, Name: status.Name, State: status.State,
			Directory: status.Directory, Clients: status.Clients,
			StartedAt: status.StartedAt, Target: status.Target,
		})
	}
	if jsonOutput {
		return json.NewEncoder(a.out).Encode(items)
	}
	if len(items) == 0 {
		_, err := fmt.Fprintln(a.out, "No running Wraps.")
		return err
	}
	table := tabwriter.NewWriter(a.out, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(table, "NAME\tID\tSTATE\tCLIENTS\tDIRECTORY\tTARGET\tSTARTED"); err != nil {
		return err
	}
	for _, item := range items {
		shortID := item.ID
		if len(shortID) > 10 {
			shortID = shortID[:10]
		}
		if _, err := fmt.Fprintf(table, "%s\t%s\t%s\t%d\t%s\t%s %s\t%s\n",
			safeHumanText(item.Name), shortID, safeHumanText(item.State), item.Clients, safeHumanText(item.Directory),
			safeHumanText(item.Target.SocketPath), safeHumanText(item.Target.WindowID), item.StartedAt.Format(time.RFC3339)); err != nil {
			return err
		}
	}
	return table.Flush()
}

type inspectedRecord struct {
	record instance.Record
	status control.Status
	err    error
}

func (a *application) inspectRecords(ctx context.Context, removeDead bool) ([]inspectedRecord, []instance.RecordError, error) {
	records, problems, err := a.store.ReadAll()
	if err != nil {
		return nil, nil, err
	}
	inspected := make([]inspectedRecord, 0, len(records))
	for _, record := range records {
		status, callErr := a.callControl(ctx, record.ControlSocket, control.Request{
			InstanceID: record.ID, Action: control.ActionStatus,
		})
		if callErr == nil && status.ID == record.ID {
			inspected = append(inspected, inspectedRecord{record: record, status: status})
			continue
		}
		if callErr == nil {
			callErr = errors.New("control socket returned a different instance ID")
		}
		if removeDead {
			recordStore, storeErr := a.store.ForRecord(record)
			if storeErr != nil {
				return nil, problems, fmt.Errorf("resolve worker runtime for Wrap %q: %w", record.Name, storeErr)
			}
			lease, leaseErr := recordStore.AcquireLease(record.ID)
			if errors.Is(leaseErr, instance.ErrLeaseHeld) {
				inspected = append(inspected, inspectedRecord{record: record, err: callErr})
				continue
			}
			if leaseErr != nil {
				return nil, problems, fmt.Errorf("inspect worker lease for Wrap %q: %w", record.Name, leaseErr)
			}
			if cleanupErr := a.cleanupStaleRecord(record); cleanupErr != nil {
				closeErr := lease.Close()
				return nil, problems, errors.Join(
					fmt.Errorf("clean stale Wrap %q: %w", record.Name, cleanupErr),
					closeErr,
				)
			}
			removed, removeErr := a.store.RemoveIfPID(record.ID, record.PID)
			if removeErr != nil {
				_ = lease.Close()
				return nil, problems, fmt.Errorf("remove stale Wrap %q: %w", record.Name, removeErr)
			}
			if removed {
				if _, writeErr := fmt.Fprintf(a.err, "wrap: removed stale instance record %s (%s)\n", safeHumanLabel(record.Name), record.ID); writeErr != nil {
					return nil, problems, errors.Join(
						fmt.Errorf("write stale instance warning: %w", writeErr),
						lease.Close(),
					)
				}
				if err := lease.Close(); err != nil {
					return nil, problems, fmt.Errorf("release stale Wrap %q lease: %w", record.Name, err)
				}
				continue
			}
			if err := lease.Close(); err != nil {
				return nil, problems, fmt.Errorf("release Wrap %q lease: %w", record.Name, err)
			}
		}
		inspected = append(inspected, inspectedRecord{record: record, err: callErr})
	}
	return inspected, problems, nil
}

func (a *application) cleanupStaleRecord(record instance.Record) error {
	if a.cleanupStale != nil {
		return a.cleanupStale(record)
	}
	return a.cleanupStaleArtifacts(record)
}

func (a *application) cleanupStaleArtifacts(record instance.Record) error {
	recordStore, err := a.store.ForRecord(record)
	if err != nil {
		return err
	}
	helperErr := target.CleanupHelper(record.Target, record.ID, nil)
	controlErr := cleanupStaleControlSocket(recordStore.RuntimeRoot, record)
	return errors.Join(helperErr, controlErr)
}

func cleanupStaleControlSocket(runtimeRoot string, record instance.Record) error {
	path := filepath.Clean(record.ControlSocket)
	if filepath.Dir(path) != filepath.Clean(runtimeRoot) || filepath.Base(path) != record.ID+".sock" {
		return errors.New("control socket is outside the owned runtime path")
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSocket == 0 || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("control socket path is not a socket")
	}
	return os.Remove(path)
}

func safeHumanLabel(value string) string {
	return strconv.QuoteToGraphic(value)
}

func safeHumanText(value string) string {
	quoted := strconv.QuoteToGraphic(value)
	if len(quoted) >= 2 {
		return quoted[1 : len(quoted)-1]
	}
	return quoted
}

func (a *application) show(selector string, jsonOutput bool) error {
	record, err := a.store.Resolve(selector)
	if err != nil {
		return err
	}
	status, err := a.callControl(context.Background(), record.ControlSocket, control.Request{
		InstanceID: record.ID, Action: control.ActionStatus,
	})
	if err != nil {
		return err
	}
	return writeStatus(a.out, status, jsonOutput)
}

func (a *application) regen(selector string, jsonOutput bool) error {
	record, err := a.store.Resolve(selector)
	if err != nil {
		return err
	}
	status, err := a.callControl(context.Background(), record.ControlSocket, control.Request{
		InstanceID: record.ID, Action: control.ActionRotate,
	})
	if err != nil {
		return err
	}
	return writeStatus(a.out, status, jsonOutput)
}

func (a *application) remove(selector string) error {
	record, err := a.store.Resolve(selector)
	if err != nil {
		return err
	}
	if _, err := a.callControl(context.Background(), record.ControlSocket, control.Request{
		InstanceID: record.ID, Action: control.ActionShutdown,
	}); err != nil {
		recordStore, storeErr := a.store.ForRecord(record)
		if storeErr != nil {
			return errors.Join(err, storeErr)
		}
		lease, leaseErr := recordStore.AcquireLease(record.ID)
		if errors.Is(leaseErr, instance.ErrLeaseHeld) {
			return err
		}
		if leaseErr != nil {
			return errors.Join(err, leaseErr)
		}
		cleanupErr := a.cleanupStaleRecord(record)
		if cleanupErr != nil {
			return errors.Join(err, fmt.Errorf("clean stale Wrap %q: %w", record.Name, cleanupErr), lease.Close())
		}
		removed, removeErr := a.store.RemoveIfPID(record.ID, record.PID)
		closeErr := lease.Close()
		if removeErr != nil || closeErr != nil {
			return errors.Join(err, removeErr, closeErr)
		}
		if !removed {
			return err
		}
		_, writeErr := fmt.Fprintf(a.out, "Removed stale Wrap %s. The tmux window is still running.\n", safeHumanLabel(record.Name))
		return writeErr
	}
	if err := a.waitForStopped(record); err != nil {
		return err
	}
	_, writeErr := fmt.Fprintf(a.out, "Removed Wrap %s. The tmux window is still running.\n", safeHumanLabel(record.Name))
	return writeErr
}

func (a *application) waitForStopped(record instance.Record) error {
	recordStore, err := a.store.ForRecord(record)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(a.managementWait)
	for {
		_, err := a.store.Resolve(record.ID)
		if errors.Is(err, instance.ErrNotFound) {
			held, leaseErr := recordStore.LeaseHeld(record.ID)
			if leaseErr != nil {
				return leaseErr
			}
			if !held {
				return nil
			}
			err = nil
		}
		if err != nil {
			return err
		}
		if time.Now().After(deadline) {
			return errors.New("worker did not finish shutdown")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (a *application) doctor(jsonOutput bool) error {
	return a.doctorContext(context.Background(), jsonOutput)
}

func (a *application) doctorContext(ctx context.Context, jsonOutput bool) error {
	report := doctor.Check(ctx, doctor.Options{
		Platform:     runtime.GOOS,
		Store:        a.store,
		LookPath:     a.lookPath,
		Run:          a.runCommand,
		Call:         a.callControl,
		ProbeTimeout: externalProbeTimeout,
	})
	if jsonOutput {
		if err := json.NewEncoder(a.out).Encode(struct {
			Version string        `json:"version"`
			Report  doctor.Report `json:"report"`
		}{Version: version, Report: report}); err != nil {
			return err
		}
		if report.State.Error != "" {
			return errors.New("state inspection incomplete")
		}
		return nil
	}
	if _, err := fmt.Fprintf(a.out, "Wrap %s (%s)\n", version, report.Platform); err != nil {
		return err
	}
	for _, item := range []doctor.Dependency{report.Tmux, report.Cloudflared} {
		if item.OK {
			if _, err := fmt.Fprintf(a.out, "%s: %s (%s)\n", item.Name, safeHumanText(item.Path), safeHumanText(item.Version)); err != nil {
				return err
			}
		} else {
			if _, err := fmt.Fprintf(a.out, "%s: %s\n", item.Name, safeHumanText(item.Error)); err != nil {
				return err
			}
		}
	}
	if _, err := fmt.Fprintf(a.out, "instances: %d live, %d stale, %d unreachable, %d malformed\n",
		report.State.Live, report.State.Stale, report.State.Unreachable, len(report.State.Malformed)); err != nil {
		return err
	}
	if report.State.Error != "" {
		if _, err := fmt.Fprintln(a.out, "state:", safeHumanText(report.State.Error)); err != nil {
			return err
		}
	}
	if !report.State.SafePermissions {
		if _, err := fmt.Fprintln(a.out, "state permissions: unsafe"); err != nil {
			return err
		}
	}
	for _, legacy := range report.Legacy {
		if _, err := fmt.Fprintf(a.out, "legacy %s: %s\n  recover: %s\n",
			safeHumanText(legacy.Name), safeHumanText(legacy.ListCommand), safeHumanText(legacy.AttachCommand)); err != nil {
			return err
		}
	}
	if report.State.Error != "" {
		return errors.New("state inspection incomplete")
	}
	return nil
}

func (a *application) bootstrap(name string) error {
	if a.getenv("TMUX") == "" {
		return errors.New("internal bootstrap must run inside tmux")
	}
	shell, err := a.resolveShell(a.getenv, nil)
	if err != nil {
		return fmt.Errorf("resolve tmux default shell: %w", err)
	}
	defaultCommand, err := a.resolveCommand(a.getenv, nil)
	if err != nil {
		return fmt.Errorf("resolve tmux default command: %w", err)
	}
	ctx, stop := a.startContext()
	status, startErr := a.startInside(ctx, name)
	stop()
	if startErr != nil {
		_, _ = fmt.Fprintln(a.err, "wrap:", safeHumanText(startErr.Error()))
	}
	execArgs := []string{"-" + filepath.Base(shell)}
	if defaultCommand != "" {
		execArgs = []string{shell, "-c", defaultCommand}
	}
	execErr := a.replaceProcess(shell, execArgs, os.Environ())
	if execErr == nil {
		return nil
	}
	var rollbackErr error
	if status.ID != "" {
		rollbackErr = a.rollbackStarted(status.ID)
	}
	return errors.Join(startErr, fmt.Errorf("start tmux default shell: %w", execErr), rollbackErr)
}

func (a *application) rollbackStarted(id string) error {
	record, err := a.store.Resolve(id)
	if errors.Is(err, instance.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("resolve started Wrap for rollback: %w", err)
	}
	if _, err := a.callControl(context.Background(), record.ControlSocket, control.Request{
		InstanceID: record.ID, Action: control.ActionShutdown,
	}); err != nil {
		return fmt.Errorf("stop started Wrap after shell failure: %w", err)
	}
	if err := a.waitForStopped(record); err != nil {
		return fmt.Errorf("wait for started Wrap rollback: %w", err)
	}
	return nil
}

func (a *application) serve(args []string) error {
	parsed, err := parseServeArgs(args)
	if err != nil {
		return err
	}
	recordFile := os.NewFile(uintptr(parsed.recordFD), "wrap-launch-record")
	readyFile := os.NewFile(uintptr(parsed.readyFD), "wrap-readiness")
	if recordFile == nil || readyFile == nil {
		return errors.New("worker inherited file descriptors are unavailable")
	}
	defer func() { _ = recordFile.Close() }()
	defer func() { _ = readyFile.Close() }()
	record, err := share.ReadLaunch(recordFile, parsed.instanceID, parsed.controlSocket)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return share.Run(ctx, share.Options{
		Record:      record,
		Store:       a.store,
		Diagnostics: mirror.NewJSONLDiagnosticSink(filepath.Join(a.store.DiagnosticsDir(), record.ID+".jsonl")),
		Ready: func(status control.Status) error {
			return reportWorkerReady(readyFile, status, redirectWorkerStderr)
		},
	})
}

func reportWorkerReady(writer io.WriteCloser, status control.Status, redirectStderr func() error) error {
	var payload bytes.Buffer
	if err := share.WriteReady(&payload, status); err != nil {
		return err
	}
	if redirectStderr == nil {
		return errors.New("worker stderr redirect is required")
	}
	if err := redirectStderr(); err != nil {
		return fmt.Errorf("redirect worker stderr: %w", err)
	}
	if _, err := io.Copy(writer, &payload); err != nil {
		return fmt.Errorf("write worker readiness: %w", err)
	}
	return writer.Close()
}

func redirectWorkerStderr() error {
	sink, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("open stderr sink: %w", err)
	}
	defer func() { _ = sink.Close() }()
	if err := unix.Dup2(int(sink.Fd()), int(os.Stderr.Fd())); err != nil {
		return fmt.Errorf("replace stderr: %w", err)
	}
	return nil
}

type serveArgs struct {
	instanceID    string
	controlSocket string
	recordFD      int
	readyFD       int
}

func parseServeArgs(args []string) (serveArgs, error) {
	flags := flag.NewFlagSet("_serve", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var parsed serveArgs
	flags.StringVar(&parsed.instanceID, "instance", "", "")
	flags.StringVar(&parsed.controlSocket, "control", "", "")
	flags.IntVar(&parsed.recordFD, "record-fd", -1, "")
	flags.IntVar(&parsed.readyFD, "ready-fd", -1, "")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return serveArgs{}, errors.New("invalid internal worker arguments")
	}
	if parsed.instanceID == "" || !filepath.IsAbs(parsed.controlSocket) || parsed.recordFD != 3 || parsed.readyFD != 4 {
		return serveArgs{}, errors.New("invalid internal worker arguments")
	}
	return parsed, nil
}

func (a *application) checkStartDependencies(ctx context.Context) error {
	probeCtx, cancel := context.WithTimeout(ctx, externalProbeTimeout)
	defer cancel()
	tmuxDependency, cloudflaredDependency := doctor.CheckDependencies(
		probeCtx, a.lookPath, a.runCommand,
	)
	for _, dependency := range []doctor.Dependency{tmuxDependency, cloudflaredDependency} {
		if !dependency.OK {
			return fmt.Errorf("starting Wrap requires %s: %s", dependency.Name, dependency.Error)
		}
	}
	return nil
}

func storeFromEnvironment(getenv func(string) string, userHome func() (string, error)) (instance.Store, error) {
	stateBase := getenv("XDG_STATE_HOME")
	if stateBase == "" {
		home, err := userHome()
		if err != nil {
			return instance.Store{}, fmt.Errorf("resolve home directory: %w", err)
		}
		stateBase = filepath.Join(home, ".local", "state")
	}
	if !filepath.IsAbs(stateBase) {
		return instance.Store{}, errors.New("XDG_STATE_HOME must be absolute")
	}
	stateRoot := filepath.Join(stateBase, "wrap")
	runtimeBase := getenv("XDG_RUNTIME_DIR")
	runtimeRoot := filepath.Join(stateRoot, "runtime")
	if runtimeBase != "" {
		if !filepath.IsAbs(runtimeBase) {
			return instance.Store{}, errors.New("XDG_RUNTIME_DIR must be absolute")
		}
		runtimeRoot = filepath.Join(runtimeBase, "wrap")
	}
	return instance.Store{StateRoot: stateRoot, RuntimeRoot: runtimeRoot}, nil
}

func newInstanceID(random io.Reader) (string, error) {
	var value [16]byte
	if _, err := io.ReadFull(random, value[:]); err != nil {
		return "", fmt.Errorf("generate Wrap instance ID: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

func writeStatus(writer io.Writer, status control.Status, jsonOutput bool) error {
	if jsonOutput {
		return json.NewEncoder(writer).Encode(status)
	}
	if _, err := fmt.Fprintf(writer, "Wrap %s (%s)\n%s\n", status.Name, status.ID, status.PairingURL); err != nil {
		return err
	}
	if status.Warning != "" {
		if _, err := fmt.Fprintln(writer, "Warning:", safeHumanText(status.Warning)); err != nil {
			return err
		}
	}
	if status.QR != "" {
		_, err := fmt.Fprint(writer, status.QR)
		return err
	}
	return nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
