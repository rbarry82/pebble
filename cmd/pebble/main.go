// Copyright (c) 2014-2020 Canonical Ltd
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License version 3 as
// published by the Free Software Foundation.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package main

import (
	"bytes"
	"errors"
	"fmt"
	"go/doc"
	"io"
	"os"
	"os/user"
	"strconv"
	"strings"
	"text/tabwriter"
	"unicode"
	"unicode/utf8"

	"github.com/jessevdk/go-flags"

	"golang.org/x/crypto/ssh/terminal"

	"github.com/canonical/pebble/internal/client"
	"github.com/canonical/pebble/internal/logger"
)

var (
	// Standard streams, redirected for testing.
	Stdin  io.Reader = os.Stdin
	Stdout io.Writer = os.Stdout
	Stderr io.Writer = os.Stderr
	// overridden for testing
	ReadPassword = terminal.ReadPassword
	// set to logger.Panicf in testing
	noticef = logger.Noticef
)

type options struct {
	Version func() `long:"version"`
}

type argDesc struct {
	name string
	desc string
}

var optionsData options

// ErrExtraArgs is returned  if extra arguments to a command are found
var ErrExtraArgs = fmt.Errorf("too many arguments for command")

// cmdInfo holds information needed to call parser.AddCommand(...).
type cmdInfo struct {
	name, shortHelp, longHelp string
	builder                   func() flags.Commander
	hidden                    bool
	optDescs                  map[string]string
	argDescs                  []argDesc
	alias                     string
	extra                     func(*flags.Command)
}

// commands holds information about all non-debug commands.
var commands []*cmdInfo

// debugCommands holds information about all debug commands.
var debugCommands []*cmdInfo

// addCommand replaces parser.addCommand() in a way that is compatible with
// re-constructing a pristine parser.
func addCommand(name, shortHelp, longHelp string, builder func() flags.Commander, optDescs map[string]string, argDescs []argDesc) *cmdInfo {
	info := &cmdInfo{
		name:      name,
		shortHelp: shortHelp,
		longHelp:  longHelp,
		builder:   builder,
		optDescs:  optDescs,
		argDescs:  argDescs,
	}
	commands = append(commands, info)
	return info
}

// addDebugCommand replaces parser.addCommand() in a way that is
// compatible with re-constructing a pristine parser. It is meant for
// adding debug commands.
func addDebugCommand(name, shortHelp, longHelp string, builder func() flags.Commander, optDescs map[string]string, argDescs []argDesc) *cmdInfo {
	info := &cmdInfo{
		name:      name,
		shortHelp: shortHelp,
		longHelp:  longHelp,
		builder:   builder,
		optDescs:  optDescs,
		argDescs:  argDescs,
	}
	debugCommands = append(debugCommands, info)
	return info
}

type parserSetter interface {
	setParser(*flags.Parser)
}

func lintDesc(cmdName, optName, desc, origDesc string) {
	if len(optName) == 0 {
		logger.Panicf("option on %q has no name", cmdName)
	}
	if len(origDesc) != 0 {
		logger.Panicf("description of %s's %q of %q set from tag", cmdName, optName, origDesc)
	}
	if len(desc) > 0 {
		// decode the first rune instead of converting all of desc into []rune
		r, _ := utf8.DecodeRuneInString(desc)
		// note IsLower != !IsUpper for runes with no upper/lower.
		if unicode.IsLower(r) && !strings.HasPrefix(desc, "login.ubuntu.com") && !strings.HasPrefix(desc, cmdName) {
			noticef("description of %s's %q is lowercase: %q", cmdName, optName, desc)
		}
	}
}

func lintArg(cmdName, optName, desc, origDesc string) {
	lintDesc(cmdName, optName, desc, origDesc)
	if len(optName) > 0 && optName[0] == '<' && optName[len(optName)-1] == '>' {
		return
	}
	if len(optName) > 0 && optName[0] == '<' && strings.HasSuffix(optName, ">s") {
		// see comment in fixupArg about the >s case
		return
	}
	noticef("argument %q's %q should begin with < and end with >", cmdName, optName)
}

func fixupArg(optName string) string {
	// Due to misunderstanding some localized versions of option name are
	// literally "<option>s" instead of "<option>". While translators can
	// improve this over time we can be smarter and avoid silly messages
	// logged whenever the command is used.
	//
	// See: https://bugs.launchpad.net/snapd/+bug/1806761
	if strings.HasSuffix(optName, ">s") {
		return optName[:len(optName)-1]
	}
	return optName
}

type clientSetter interface {
	setClient(*client.Client)
}

type clientMixin struct {
	client *client.Client
}

func (ch *clientMixin) setClient(cli *client.Client) {
	ch.client = cli
}

// Parser creates and populates a fresh parser.
// Since commands have local state a fresh parser is required to isolate tests
// from each other.
func Parser(cli *client.Client) *flags.Parser {
	optionsData.Version = func() {
		printVersions(cli)
		panic(&exitStatus{0})
	}
	flagopts := flags.Options(flags.PassDoubleDash)
	parser := flags.NewParser(&optionsData, flagopts)
	parser.ShortDescription = "Tool to interact with pebble"
	parser.LongDescription = longPebbleDescription
	// hide the unhelpful "[OPTIONS]" from help output
	parser.Usage = ""
	if version := parser.FindOptionByLongName("version"); version != nil {
		version.Description = "Print the version and exit"
		version.Hidden = true
	}
	// add --help like what go-flags would do for us, but hidden
	addHelp(parser)

	// Add all regular commands
	for _, c := range commands {
		obj := c.builder()
		if x, ok := obj.(clientSetter); ok {
			x.setClient(cli)
		}
		if x, ok := obj.(parserSetter); ok {
			x.setParser(parser)
		}

		cmd, err := parser.AddCommand(c.name, c.shortHelp, strings.TrimSpace(c.longHelp), obj)
		if err != nil {
			logger.Panicf("cannot add command %q: %v", c.name, err)
		}
		cmd.Hidden = c.hidden
		if c.alias != "" {
			cmd.Aliases = append(cmd.Aliases, c.alias)
		}

		opts := cmd.Options()
		if c.optDescs != nil && len(opts) != len(c.optDescs) {
			logger.Panicf("wrong number of option descriptions for %s: expected %d, got %d", c.name, len(opts), len(c.optDescs))
		}
		for _, opt := range opts {
			name := opt.LongName
			if name == "" {
				name = string(opt.ShortName)
			}
			desc, ok := c.optDescs[name]
			if !(c.optDescs == nil || ok) {
				logger.Panicf("%s missing description for %s", c.name, name)
			}
			lintDesc(c.name, name, desc, opt.Description)
			if desc != "" {
				opt.Description = desc
			}
		}

		args := cmd.Args()
		if c.argDescs != nil && len(args) != len(c.argDescs) {
			logger.Panicf("wrong number of argument descriptions for %s: expected %d, got %d", c.name, len(args), len(c.argDescs))
		}
		for i, arg := range args {
			name, desc := arg.Name, ""
			if c.argDescs != nil {
				name = c.argDescs[i].name
				desc = c.argDescs[i].desc
			}
			lintArg(c.name, name, desc, arg.Description)
			name = fixupArg(name)
			arg.Name = name
			arg.Description = desc
		}
		if c.extra != nil {
			c.extra(cmd)
		}
	}
	// Add the debug command
	debugCommand, err := parser.AddCommand("debug", shortDebugHelp, longDebugHelp, &cmdDebug{})
	debugCommand.Hidden = true
	if err != nil {
		logger.Panicf("cannot add command %q: %v", "debug", err)
	}
	// Add all the sub-commands of the debug command
	for _, c := range debugCommands {
		obj := c.builder()
		if x, ok := obj.(clientSetter); ok {
			x.setClient(cli)
		}
		cmd, err := debugCommand.AddCommand(c.name, c.shortHelp, strings.TrimSpace(c.longHelp), obj)
		if err != nil {
			logger.Panicf("cannot add debug command %q: %v", c.name, err)
		}
		cmd.Hidden = c.hidden
		opts := cmd.Options()
		if c.optDescs != nil && len(opts) != len(c.optDescs) {
			logger.Panicf("wrong number of option descriptions for %s: expected %d, got %d", c.name, len(opts), len(c.optDescs))
		}
		for _, opt := range opts {
			name := opt.LongName
			if name == "" {
				name = string(opt.ShortName)
			}
			desc, ok := c.optDescs[name]
			if !(c.optDescs == nil || ok) {
				logger.Panicf("%s missing description for %s", c.name, name)
			}
			lintDesc(c.name, name, desc, opt.Description)
			if desc != "" {
				opt.Description = desc
			}
		}

		args := cmd.Args()
		if c.argDescs != nil && len(args) != len(c.argDescs) {
			logger.Panicf("wrong number of argument descriptions for %s: expected %d, got %d", c.name, len(args), len(c.argDescs))
		}
		for i, arg := range args {
			name, desc := arg.Name, ""
			if c.argDescs != nil {
				name = c.argDescs[i].name
				desc = c.argDescs[i].desc
			}
			lintArg(c.name, name, desc, arg.Description)
			name = fixupArg(name)
			arg.Name = name
			arg.Description = desc
		}
	}
	return parser
}

var (
	isStdinTTY  = terminal.IsTerminal(0)
	isStdoutTTY = terminal.IsTerminal(1)
)

// TODO Support a flag to set the overall configuration directory.
var PebbleSocket = "./pebble.socket"

// ClientConfig is the configuration of the Client used by all commands.
var ClientConfig = client.Config{
	// we need the general socket
	Socket: PebbleSocket,
}

// Client returns a new client using ClientConfig as configuration.
// commands should (in general) not use this, and instead use clientMixin.
func mkClient() *client.Client {
	cfg := &ClientConfig
	cli := client.New(cfg)
	return cli
}

func main() {
	defer func() {
		if v := recover(); v != nil {
			if e, ok := v.(*exitStatus); ok {
				os.Exit(e.code)
			}
			panic(v)
		}
	}()

	if err := run(); err != nil {
		fmt.Fprintf(Stderr, errorPrefix+" %v\n", err)
		os.Exit(1)
	}
}

type exitStatus struct {
	code int
}

func (e *exitStatus) Error() string {
	return fmt.Sprintf("internal error: exitStatus{%d} being handled as normal error", e.code)
}

func run() error {
	err := logger.SimpleSetup()
	if err != nil {
		fmt.Fprintf(Stderr, "WARNING: Cannot activate logging: %v\n", err)
	}

	cli := mkClient()
	parser := Parser(cli)
	xtra, err := parser.Parse()
	if err != nil {
		if e, ok := err.(*flags.Error); ok {
			switch e.Type {
			case flags.ErrCommandRequired:
				printShortHelp()
				return nil
			case flags.ErrHelp:
				parser.WriteHelp(Stdout)
				return nil
			case flags.ErrUnknownCommand:
				sub := os.Args[1]
				sug := "pebble help"
				if len(xtra) > 0 {
					sub = xtra[0]
					if x := parser.Command.Active; x != nil && x.Name != "help" {
						sug = "pebble help " + x.Name
					}
				}
				return fmt.Errorf("unknown command %q, see '%s'.", sub, sug)
			}
		}

		msg, err := errorToMessage(err)
		if err != nil {
			return err
		}

		fmt.Fprintln(Stderr, msg)
		return nil
	}
	return nil
}

func tabWriter() *tabwriter.Writer {
	return tabwriter.NewWriter(Stdout, 5, 3, 2, ' ', 0)
}

var termSize = termSizeImpl

func termSizeImpl() (width, height int) {
	if f, ok := Stdout.(*os.File); ok {
		width, height, _ = terminal.GetSize(int(f.Fd()))
	}

	if width <= 0 {
		width, _ = strconv.Atoi(os.Getenv("COLUMNS"))
	}

	if height <= 0 {
		height, _ = strconv.Atoi(os.Getenv("LINES"))
	}

	if width < 40 {
		width = 80
	}

	if height < 15 {
		height = 25
	}

	return width, height
}

func fill(para string, indent int) string {
	width, _ := termSize()

	if width > 100 {
		width = 100
	}

	// some terminals aren't happy about writing in the last
	// column (they'll add line for you). We could check terminfo
	// for "sam" (semi_auto_right_margin), but that's a lot of
	// work just for this.
	width--

	var buf bytes.Buffer
	indentStr := strings.Repeat(" ", indent)
	doc.ToText(&buf, para, indentStr, indentStr, width-indent)

	return strings.TrimSpace(buf.String())
}

var errorPrefix = "error: "

func errorToMessage(e error) (normalMessage string, err error) {
	cerr, ok := e.(*client.Error)
	if !ok {
		return "", e
	}

	logger.Debugf("error: %s", cerr)

	isError := true

	var msg string
	switch cerr.Kind {
	case client.ErrorKindLoginRequired:
		u, _ := user.Current()
		if u != nil && u.Username == "root" {
			msg = cerr.Message
		} else {
			msg = fmt.Sprintf(`%s (try with sudo)`, cerr.Message)
		}
	default:
		msg = cerr.Message
	}

	msg = fill(msg, len(errorPrefix))
	if isError {
		return "", errors.New(msg)
	}

	return msg, nil
}