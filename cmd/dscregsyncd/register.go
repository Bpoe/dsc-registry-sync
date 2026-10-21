package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Bpoe/dsc-registry-sync/internal/config"
	"github.com/Bpoe/dsc-registry-sync/internal/provision"
	"github.com/Bpoe/dsc-registry-sync/internal/registry"
	"golang.org/x/term"
)

const registrationUsage = "Usage: dscregsyncd register <machine-name> --server <registry-url> [--config path] [--token-stdin]\nThe token is read without echo from the terminal, or as one line with --token-stdin.\n"

var errRestoreTerminal = errors.New("cannot restore terminal settings after registration token input")

type registerOptions struct {
	name, server, path string
	stdin              bool
}

func parseRegister(args []string) (registerOptions, error) {
	options := registerOptions{}
	flags := flag.NewFlagSet("register", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&options.server, "server", "", "Registry HTTP or HTTPS origin")
	flags.StringVar(&options.path, "config", config.DefaultPath(), "destination service JSON configuration")
	flags.BoolVar(&options.stdin, "token-stdin", false, "read one token line from standard input")
	// flag stops at a positional argument. Consume the documented leading name
	// first, while also accepting flags before the name.
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		options.name, args = args[0], args[1:]
	}
	if err := flags.Parse(args); err != nil {
		return options, err
	}
	if options.name == "" && flags.NArg() == 1 {
		options.name = flags.Arg(0)
	} else if flags.NArg() != 0 {
		return options, errors.New("unexpected registration arguments")
	}
	if !registry.ValidMachineName(options.name) || options.server == "" {
		return options, errors.New("registration requires a valid machine name and --server")
	}
	return options, nil
}

func runRegister(ctx context.Context, args []string, input io.Reader, output, diagnostics io.Writer,
	readPassword func(context.Context, io.Reader) ([]byte, error),
) (code int) {
	fail := func(err error) int {
		fmt.Fprintln(diagnostics, err)
		return 1
	}
	options, err := parseRegister(args)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Fprint(output, registrationUsage)
		return 0
	}
	if err != nil {
		fmt.Fprintln(diagnostics, "Invalid registration command; use dscregsyncd register --help.")
		return 2
	}
	cfg := config.Defaults()
	cfg.RegistryURL = options.server
	cfg.APIKeyFile = filepath.Join(filepath.Dir(options.path), "dscregsyncd.key")
	if err := cfg.Validate(); err != nil {
		return fail(err)
	}
	files, err := provision.Prepare(options.path, cfg)
	if err != nil {
		return fail(err)
	}
	defer func() {
		if err := files.Close(); err != nil {
			fmt.Fprintln(diagnostics, err)
			code = 1
		}
	}()
	if strings.HasPrefix(cfg.RegistryURL, "http://") {
		if _, err := fmt.Fprint(diagnostics, "WARNING: HTTP connections are not encrypted. Registration tokens\nand machine API keys may be exposed in transit.\n\n"); err != nil {
			return fail(errors.New("cannot write registration transport warning"))
		}
	}
	token, err := readToken(ctx, input, diagnostics, options.stdin, readPassword)
	if err != nil {
		return fail(err)
	}
	result, err := registry.Register(ctx, cfg, options.name, token)
	if err != nil {
		return fail(err)
	}
	if err := files.Publish(ctx, result.APIKey); err != nil {
		return fail(errors.New("machine registered remotely but local provisioning failed; the name is already registered and administrator-assisted recovery may be required"))
	}
	if err := files.Close(); err != nil {
		return fail(errors.New("machine registered remotely and configuration published, but cleanup was incomplete; administrator-assisted recovery may be required"))
	}
	if _, err := fmt.Fprintf(output, "Machine registered successfully.\nName: %s\nID: %s\nServer: %s\nConfiguration: %s\n",
		result.Name, result.ID, cfg.RegistryURL, filepath.Clean(options.path)); err != nil {
		return 1
	}
	return 0
}

func readToken(ctx context.Context, input io.Reader, prompt io.Writer, stdin bool,
	readPassword func(context.Context, io.Reader) ([]byte, error),
) (string, error) {
	var data []byte
	var err error
	if stdin {
		data, err = readTokenLine(ctx, input)
	} else {
		if _, err := fmt.Fprint(prompt, "Registration token: "); err != nil {
			return "", errors.New("cannot write registration prompt")
		}
		data, err = readPassword(ctx, input)
		if _, newlineErr := fmt.Fprintln(prompt); err == nil {
			err = newlineErr
		}
	}
	defer clear(data)
	if errors.Is(err, errRestoreTerminal) {
		return "", errRestoreTerminal
	}
	if ctx.Err() != nil {
		return "", errors.New("registration canceled while reading token")
	}
	if err != nil {
		return "", errors.New("cannot read registration token; use a terminal or --token-stdin")
	}
	token := string(data)
	if !registry.ValidRegistrationToken(token) {
		return "", errors.New("missing or invalid registration token")
	}
	return token, nil
}

func readTokenLine(ctx context.Context, input io.Reader) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	type result struct {
		data []byte
		err  error
	}
	done := make(chan result)
	// A pipe or console read may outlive cancellation. Do not wait for it before
	// cleaning provisioning files and exiting this one-shot command.
	go func() {
		reader := bufio.NewReader(io.LimitReader(input, 8194))
		data, err := reader.ReadBytes('\n')
		if err == io.EOF {
			err = nil
		}
		select {
		case done <- result{data, err}:
		case <-ctx.Done():
			clear(data)
		}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case result := <-done:
		data := result.data
		if len(data) > 0 && data[len(data)-1] == '\n' {
			data = data[:len(data)-1]
		}
		if len(data) > 0 && data[len(data)-1] == '\r' {
			data = data[:len(data)-1]
		}
		return data, result.err
	}
}

func terminalToken(ctx context.Context, input io.Reader) (data []byte, err error) {
	file, ok := input.(*os.File)
	if !ok || !term.IsTerminal(int(file.Fd())) {
		return nil, errors.New("standard input is not a terminal")
	}
	restore, err := disableTokenEcho(file)
	if err != nil {
		return nil, err
	}
	defer func() {
		if restore() != nil {
			clear(data)
			data, err = nil, errRestoreTerminal
		}
	}()
	return readTokenLine(ctx, file)
}
