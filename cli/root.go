package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"golang.org/x/xerrors"

	"cdr.dev/slog"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-isatty"

	"github.com/coder/coder/buildinfo"
	"github.com/coder/coder/cli/clibase"
	"github.com/coder/coder/cli/cliui"
	"github.com/coder/coder/cli/config"
	"github.com/coder/coder/coderd"
	"github.com/coder/coder/coderd/gitauth"
	"github.com/coder/coder/codersdk"
	"github.com/coder/coder/codersdk/agentsdk"
)

var (
	Caret = cliui.Styles.Prompt.String()

	// Applied as annotations to workspace commands
	// so they display in a separated "help" section.
	workspaceCommand = map[string]string{
		"workspaces": "",
	}
)

const (
	varURL              = "url"
	varToken            = "token"
	varAgentToken       = "agent-token"
	varAgentURL         = "agent-url"
	varHeader           = "header"
	varNoOpen           = "no-open"
	varNoVersionCheck   = "no-version-warning"
	varNoFeatureWarning = "no-feature-warning"
	varForceTty         = "force-tty"
	varVerbose          = "verbose"
	notLoggedInMessage  = "You are not logged in. Try logging in using 'coder login <url>'."

	envNoVersionCheck   = "CODER_NO_VERSION_WARNING"
	envNoFeatureWarning = "CODER_NO_FEATURE_WARNING"
	envSessionToken     = "CODER_SESSION_TOKEN"
	envURL              = "CODER_URL"
)

var errUnauthenticated = xerrors.New(notLoggedInMessage)

func (r *RootCmd) Core() []*clibase.Cmd {
	// Please re-sort this list alphabetically if you change it!
	return []*clibase.Cmd{
		r.dotfiles(),
		r.login(),
		r.logout(),
		r.portForward(),
		r.publickey(),
		r.resetPassword(),
		r.state(),
		r.templates(),
		r.users(),
		r.version(),

		// Workspace Commands
		r.configSSH(),
		r.create(),
		r.deleteWorkspace(),
		r.list(),
		r.schedules(),
		r.show(),
		r.speedtest(),
		r.ssh(),
		r.start(),
		r.stop(),
		r.update(),

		// Hidden
		r.workspaceAgent(),
		r.scaletest(),
	}
}

func AGPL(r *RootCmd) []*clibase.Cmd {
	all := append(r.Core(), r.Server(func(_ context.Context, o *coderd.Options) (*coderd.API, io.Closer, error) {
		api := coderd.New(o)
		return api, api, nil
	}))
	return all
}

// Main is the entrypoint for the Coder CLI.
func Main(subcommands []*clibase.Cmd) {
	rand.Seed(time.Now().UnixMicro())

	var cmd RootCmd
	err := cmd.Command(subcommands).Invoke().WithMain().Run()
	if err != nil {
		if errors.Is(err, cliui.Canceled) {
			//nolint:revive
			os.Exit(1)
		}
		_, _ = fmt.Fprintln(os.Stderr, err)
		//nolint:revive
		os.Exit(1)
	}
}

func (r *RootCmd) Command(subcommands []*clibase.Cmd) *clibase.Cmd {
	fmtLong := `Coder %s — A tool for provisioning self-hosted development environments with Terraform.
`
	cmd := &clibase.Cmd{
		Use: "coder",
		Long: fmt.Sprintf(fmtLong, buildinfo.Version()) + formatExamples(
			example{
				Description: "Start a Coder server",
				Command:     "coder server",
			},
			example{
				Description: "Get started by creating a template from an example",
				Command:     "coder templates init",
			},
		),
		Handler: func(i *clibase.Invokation) error {
			// fmt.Fprintf(i.Stderr, "env debug: %+v", i.Environ)
			// The GIT_ASKPASS environment variable must point at
			// a binary with no arguments. To prevent writing
			// cross-platform scripts to invoke the Coder binary
			// with a `gitaskpass` subcommand, we override the entrypoint
			// to check if the command was invoked.
			if gitauth.CheckCommand(i.Args, i.Environ.ToOS()) {
				return r.gitAskpass().Handler(i)
			}
			return i.Command.HelpHandler(i)
		},
		Children: subcommands,
	}

	// Set default help handler for all commands.
	cmd.Walk(func(c *clibase.Cmd) {
		if c.HelpHandler == nil {
			c.HelpHandler = func(i *clibase.Invokation) error {
				usageFn(i.Stderr, c)()
				return nil
			}
		}
	})

	if r.agentURL == nil {
		r.agentURL = new(url.URL)
	}
	if r.clientURL == nil {
		r.clientURL = new(url.URL)
	}

	cmd.Options = []clibase.Option{
		{
			Name:        varURL,
			Flag:        varURL,
			Env:         envURL,
			Description: "URL to a deployment.",
			Value:       clibase.URLOf(r.clientURL),
		},
		{
			Name:        varToken,
			Flag:        varToken,
			Env:         envSessionToken,
			Description: fmt.Sprintf("Specify an authentication token. For security reasons setting %s is preferred.", envSessionToken),
			Value:       clibase.StringOf(&r.token),
		},
		{
			Name:        varAgentToken,
			Flag:        varAgentToken,
			Description: "An agent authentication token.",
			Value:       clibase.StringOf(&r.agentToken),
			Hidden:      true,
		},
		{
			Name:        varAgentURL,
			Flag:        varAgentURL,
			Env:         "CODER_AGENT_URL",
			Description: "URL for an agent to access your deployment",
			Value:       clibase.URLOf(r.agentURL),
			Hidden:      true,
		},
		{
			Name:        varNoVersionCheck,
			Flag:        varNoVersionCheck,
			Env:         envNoVersionCheck,
			Description: "Suppress warning when client and server versions do not match.",
			Value:       clibase.BoolOf(&r.noVersionCheck),
		},
		{
			Name:        varNoFeatureWarning,
			Flag:        varNoFeatureWarning,
			Env:         envNoFeatureWarning,
			Description: "Suppress warnings about unlicensed features.",
			Value:       clibase.BoolOf(&r.noFeatureWarning),
		},
		{
			Name:        varHeader,
			Flag:        varHeader,
			Env:         "CODER_HEADER",
			Description: "Additional HTTP headers to send to the server.",
			Value:       clibase.StringsOf(&r.header),
		},
		{
			Name:        varNoOpen,
			Flag:        varNoOpen,
			Env:         "CODER_NO_OPEN",
			Description: "Suppress opening the browser after logging in.",
			Value:       clibase.BoolOf(&r.noOpen),
			Hidden:      true,
		},
		{
			Name:        varForceTty,
			Flag:        varForceTty,
			Env:         "CODER_FORCE_TTY",
			Hidden:      true,
			Description: "Force the use of a TTY.",
			Value:       clibase.BoolOf(&r.forceTTY),
		},
		{
			Name:          varVerbose,
			Flag:          varVerbose,
			FlagShorthand: "v",
			Env:           "CODER_VERBOSE",
			Description:   "Enable verbose logging.",
			Value:         clibase.BoolOf(&r.verbose),
		},
		{
			Name:        config.FlagName,
			Flag:        config.FlagName,
			Env:         "CODER_CONFIG_DIR",
			Description: "Path to the global `coder` config directory.",
			Default:     config.DefaultDir(),
			Value:       clibase.StringOf(&r.globalConfig),
		},
	}

	return cmd
}

type contextKey int

const (
	contextKeyLogger contextKey = iota
)

func ContextWithLogger(ctx context.Context, l slog.Logger) context.Context {
	return context.WithValue(ctx, contextKeyLogger, l)
}

func LoggerFromContext(ctx context.Context) (slog.Logger, bool) {
	l, ok := ctx.Value(contextKeyLogger).(slog.Logger)
	return l, ok
}

// version prints the coder version
func (r *RootCmd) version() *clibase.Cmd {
	return &clibase.Cmd{
		Use:   "version",
		Short: "Show coder version",
		Handler: func(inv *clibase.Invokation) error {
			var str strings.Builder
			_, _ = str.WriteString("Coder ")
			if buildinfo.IsAGPL() {
				_, _ = str.WriteString("(AGPL) ")
			}
			_, _ = str.WriteString(buildinfo.Version())
			buildTime, valid := buildinfo.Time()
			if valid {
				_, _ = str.WriteString(" " + buildTime.Format(time.UnixDate))
			}
			_, _ = str.WriteString("\r\n" + buildinfo.ExternalURL() + "\r\n\r\n")

			if buildinfo.IsSlim() {
				_, _ = str.WriteString(fmt.Sprintf("Slim build of Coder, does not support the %s subcommand.\n", cliui.Styles.Code.Render("server")))
			} else {
				_, _ = str.WriteString(fmt.Sprintf("Full build of Coder, supports the %s subcommand.\n", cliui.Styles.Code.Render("server")))
			}

			_, _ = fmt.Fprint(inv.Stdout, str.String())
			return nil
		},
	}
}

func isTest() bool {
	return flag.Lookup("test.v") != nil
}

// RootCmd contains parameters and helpers useful to all commands.
type RootCmd struct {
	clientURL    *url.URL
	token        string
	globalConfig string
	header       []string
	agentToken   string
	agentURL     *url.URL
	forceTTY     bool
	noOpen       bool
	verbose      bool

	noVersionCheck   bool
	noFeatureWarning bool
}

// UseClient returns a new client from the command context.
// It reads from global configuration files if flags are not set.
func (r *RootCmd) UseClient(client *codersdk.Client) clibase.MiddlewareFunc {
	return func(next clibase.HandlerFunc) clibase.HandlerFunc {
		return func(i *clibase.Invokation) error {
			root := r.createConfig()
			var err error
			if r.clientURL.String() == "" {
				rawURL, err := root.URL().Read()
				// If the configuration files are absent, the user is logged out
				if os.IsNotExist(err) {
					return (errUnauthenticated)
				}
				if err != nil {
					return err
				}

				r.clientURL, err = url.Parse(strings.TrimSpace(rawURL))
				if err != nil {
					return err
				}
			}

			if r.token == "" {
				r.token, err = root.Session().Read()
				// If the configuration files are absent, the user is logged out
				if os.IsNotExist(err) {
					return (errUnauthenticated)
				}
				if err != nil {
					return err
				}
			}

			err = r.initClient(client)
			if err != nil {
				return err
			}

			client.SetSessionToken(r.token)

			// We send these requests in parallel to minimize latency.
			var (
				versionErr = make(chan error)
				warningErr = make(chan error)
			)
			go func() {
				versionErr <- r.checkVersions(i, client)
				close(versionErr)
			}()

			go func() {
				warningErr <- r.checkWarnings(i, client)
				close(warningErr)
			}()

			if err = <-versionErr; err != nil {
				// Just log the error here. We never want to fail a command
				// due to a pre-run.
				_, _ = fmt.Fprintf(i.Stderr,
					cliui.Styles.Warn.Render("check versions error: %s"), err)
				_, _ = fmt.Fprintln(i.Stderr)
			}

			if err = <-warningErr; err != nil {
				// Same as above
				_, _ = fmt.Fprintf(i.Stderr,
					cliui.Styles.Warn.Render("check entitlement warnings error: %s"), err)
				_, _ = fmt.Fprintln(i.Stderr)
			}

			return nil
		}
	}
}

func (r *RootCmd) initClient(client *codersdk.Client) error {
	transport := &headerTransport{
		transport: http.DefaultTransport,
		headers:   map[string]string{},
	}
	for _, header := range r.header {
		parts := strings.SplitN(header, "=", 2)
		if len(parts) < 2 {
			return xerrors.Errorf("split header %q had less than two parts", header)
		}
		transport.headers[parts[0]] = parts[1]
	}
	client.HTTPClient.Transport = transport
	return nil
}

func (r *RootCmd) createUnauthenticatedClient(serverURL *url.URL) (*codersdk.Client, error) {
	var client codersdk.Client
	r.initClient(&client)
	return &client, nil
}

// createAgentClient returns a new client from the command context.
// It works just like CreateClient, but uses the agent token and URL instead.
func (r *RootCmd) createAgentClient(inv *clibase.Invokation) (*agentsdk.Client, error) {
	client := agentsdk.New(r.agentURL)
	client.SetSessionToken(r.agentToken)
	return client, nil
}

// CurrentOrganization returns the currently active organization for the authenticated user.
func CurrentOrganization(inv *clibase.Invokation, client *codersdk.Client) (codersdk.Organization, error) {
	orgs, err := client.OrganizationsByUser(inv.Context(), codersdk.Me)
	if err != nil {
		return codersdk.Organization{}, nil
	}
	// For now, we won't use the config to set this.
	// Eventually, we will support changing using "coder switch <org>"
	return orgs[0], nil
}

// namedWorkspace fetches and returns a workspace by an identifier, which may be either
// a bare name (for a workspace owned by the current user) or a "user/workspace" combination,
// where user is either a username or UUID.
func namedWorkspace(ctx context.Context, client *codersdk.Client, identifier string) (codersdk.Workspace, error) {
	parts := strings.Split(identifier, "/")

	var owner, name string
	switch len(parts) {
	case 1:
		owner = codersdk.Me
		name = parts[0]
	case 2:
		owner = parts[0]
		name = parts[1]
	default:
		return codersdk.Workspace{}, xerrors.Errorf("invalid workspace name: %q", identifier)
	}

	return client.WorkspaceByOwnerAndName(ctx, owner, name, codersdk.WorkspaceOptions{})
}

// createConfig consumes the global configuration flag to produce a config root.
func (r *RootCmd) createConfig() config.Root {
	return config.Root(r.globalConfig)
}

// isTTY returns whether the passed reader is a TTY or not.
// This accepts a reader to work with Cobra's "InOrStdin"
// function for simple testing.
func isTTY(inv *clibase.Invokation) bool {
	// If the `--force-tty` command is available, and set,
	// assume we're in a tty. This is primarily for cases on Windows
	// where we may not be able to reliably detect this automatically (ie, tests)
	forceTty, err := inv.ParsedFlags().GetBool(varForceTty)
	if forceTty && err == nil {
		return true
	}
	file, ok := inv.Stdin.(*os.File)
	if !ok {
		return false
	}
	return isatty.IsTerminal(file.Fd())
}

// isTTYOut returns whether the passed reader is a TTY or not.
// This accepts a reader to work with Cobra's "OutOrStdout"
// function for simple testing.
func isTTYOut(inv *clibase.Invokation) bool {
	return isTTYWriter(inv, inv.Stdout)
}

// isTTYErr returns whether the passed reader is a TTY or not.
// This accepts a reader to work with Cobra's "ErrOrStderr"
// function for simple testing.
func isTTYErr(inv *clibase.Invokation) bool {
	return isTTYWriter(inv, inv.Stderr)
}

func isTTYWriter(inv *clibase.Invokation, writer io.Writer) bool {
	// If the `--force-tty` command is available, and set,
	// assume we're in a tty. This is primarily for cases on Windows
	// where we may not be able to reliably detect this automatically (ie, tests)
	forceTty, err := inv.ParsedFlags().GetBool(varForceTty)
	if forceTty && err == nil {
		return true
	}
	file, ok := writer.(*os.File)
	if !ok {
		return false
	}
	return isatty.IsTerminal(file.Fd())
}

// example represents a standard example for command usage, to be used
// with formatExamples.
type example struct {
	Description string
	Command     string
}

// formatExamples formats the examples as width wrapped bulletpoint
// descriptions with the command underneath.
func formatExamples(examples ...example) string {
	wrap := cliui.Styles.Wrap.Copy()
	wrap.PaddingLeft(4)
	var sb strings.Builder
	for i, e := range examples {
		if len(e.Description) > 0 {
			_, _ = sb.WriteString("  - " + wrap.Render(e.Description + ":")[4:] + "\n\n    ")
		}
		// We add 1 space here because `cliui.Styles.Code` adds an extra
		// space. This makes the code block align at an even 2 or 6
		// spaces for symmetry.
		_, _ = sb.WriteString(" " + cliui.Styles.Code.Render(fmt.Sprintf("$ %s", e.Command)))
		if i < len(examples)-1 {
			_, _ = sb.WriteString("\n\n")
		}
	}
	return sb.String()
}

func (r *RootCmd) checkVersions(i *clibase.Invokation, client *codersdk.Client) error {
	if r.noVersionCheck {
		return nil
	}

	ctx, cancel := context.WithTimeout(i.Context(), 10*time.Second)
	defer cancel()

	clientVersion := buildinfo.Version()
	info, err := client.BuildInfo(ctx)
	// Avoid printing errors that are connection-related.
	if isConnectionError(err) {
		return nil
	}

	if err != nil {
		return xerrors.Errorf("build info: %w", err)
	}

	fmtWarningText := `version mismatch: client %s, server %s
`
	// Our installation script doesn't work on Windows, so instead we direct the user
	// to the GitHub release page to download the latest installer.
	if runtime.GOOS == "windows" {
		fmtWarningText += `download the server version from: https://github.com/coder/coder/releases/v%s`
	} else {
		fmtWarningText += `download the server version with: 'curl -L https://coder.com/install.sh | sh -s -- --version %s'`
	}

	if !buildinfo.VersionsMatch(clientVersion, info.Version) {
		warn := cliui.Styles.Warn.Copy().Align(lipgloss.Left)
		_, _ = fmt.Fprintf(i.Stderr, warn.Render(fmtWarningText), clientVersion, info.Version, strings.TrimPrefix(info.CanonicalVersion(), "v"))
		_, _ = fmt.Fprintln(i.Stderr)
	}

	return nil
}

func (r *RootCmd) checkWarnings(i *clibase.Invokation, client *codersdk.Client) error {
	if r.noFeatureWarning {
		return nil
	}

	ctx, cancel := context.WithTimeout(i.Context(), 10*time.Second)
	defer cancel()

	entitlements, err := client.Entitlements(ctx)
	if err == nil {
		for _, w := range entitlements.Warnings {
			_, _ = fmt.Fprintln(i.Stderr, cliui.Styles.Warn.Render(w))
		}
	}
	return nil
}

type headerTransport struct {
	transport http.RoundTripper
	headers   map[string]string
}

func (h *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	for k, v := range h.headers {
		req.Header.Add(k, v)
	}
	return h.transport.RoundTrip(req)
}

// dumpHandler provides a custom SIGQUIT and SIGTRAP handler that dumps the
// stacktrace of all goroutines to stderr and a well-known file in the home
// directory. This is useful for debugging deadlock issues that may occur in
// production in workspaces, since the default Go runtime will only dump to
// stderr (which is often difficult/impossible to read in a workspace).
//
// SIGQUITs will still cause the program to exit (similarly to the default Go
// runtime behavior).
//
// A SIGQUIT handler will not be registered if GOTRACEBACK=crash.
//
// On Windows this immediately returns.
func dumpHandler(ctx context.Context) {
	if runtime.GOOS == "windows" {
		// free up the goroutine since it'll be permanently blocked anyways
		return
	}

	listenSignals := []os.Signal{syscall.SIGTRAP}
	if os.Getenv("GOTRACEBACK") != "crash" {
		listenSignals = append(listenSignals, syscall.SIGQUIT)
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, listenSignals...)
	defer signal.Stop(sigs)

	for {
		sigStr := ""
		select {
		case <-ctx.Done():
			return
		case sig := <-sigs:
			switch sig {
			case syscall.SIGQUIT:
				sigStr = "SIGQUIT"
			case syscall.SIGTRAP:
				sigStr = "SIGTRAP"
			}
		}

		// Start with a 1MB buffer and keep doubling it until we can fit the
		// entire stacktrace, stopping early once we reach 64MB.
		buf := make([]byte, 1_000_000)
		stacklen := 0
		for {
			stacklen = runtime.Stack(buf, true)
			if stacklen < len(buf) {
				break
			}
			if 2*len(buf) > 64_000_000 {
				// Write a message to the end of the buffer saying that it was
				// truncated.
				const truncatedMsg = "\n\n\nstack trace truncated due to size\n"
				copy(buf[len(buf)-len(truncatedMsg):], truncatedMsg)
				break
			}
			buf = make([]byte, 2*len(buf))
		}

		_, _ = fmt.Fprintf(os.Stderr, "%s:\n%s\n", sigStr, buf[:stacklen])

		// Write to a well-known file.
		dir, err := os.UserHomeDir()
		if err != nil {
			dir = os.TempDir()
		}
		fpath := filepath.Join(dir, fmt.Sprintf("coder-agent-%s.dump", time.Now().Format("2006-01-02T15:04:05.000Z")))
		_, _ = fmt.Fprintf(os.Stderr, "writing dump to %q\n", fpath)

		f, err := os.Create(fpath)
		if err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "failed to open dump file: %v\n", err.Error())
			goto done
		}
		_, err = f.Write(buf[:stacklen])
		_ = f.Close()
		if err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "failed to write dump file: %v\n", err.Error())
			goto done
		}

	done:
		if sigStr == "SIGQUIT" {
			//nolint:revive
			os.Exit(1)
		}
	}
}

// IiConnectionErr is a convenience function for checking if the source of an
// error is due to a 'connection refused', 'no such host', etc.
func isConnectionError(err error) bool {
	var (
		// E.g. no such host
		dnsErr *net.DNSError
		// Eg. connection refused
		opErr *net.OpError
	)

	return xerrors.As(err, &dnsErr) || xerrors.As(err, &opErr)
}
