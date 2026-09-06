package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"
)

// options are the deployment's command line: where the gateway listens, what
// resource it is, and whose tokens it trusts. Everything about the backends
// themselves lives in the configuration file instead, because that is the part
// an operator edits.
type options struct {
	config   string
	addr     string
	path     string
	resource string
	issuers  []string
	keys     []keyFile
	shutdown time.Duration
}

// A keyFile is one authorization server's public key, as named on the command
// line. The kid is optional: a deployment whose issuer signs with one key need
// not name it, and neither need the tokens.
type keyFile struct {
	issuer string
	kid    string
	path   string
}

func parseFlags(args []string) (options, error) {
	var opts options
	var issuers, keys repeated

	fs := flag.NewFlagSet("mcp-reverse-proxy", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&opts.config, "config", "", "path to the configuration file (required)")
	fs.StringVar(&opts.addr, "addr", "127.0.0.1:8080", "address to listen on")
	fs.StringVar(&opts.path, "path", "/mcp", "path the MCP endpoint is served at")
	fs.StringVar(&opts.resource, "resource", "",
		"canonical URI clients reach this endpoint by, and the audience tokens must name (required)")
	fs.Var(&issuers, "issuer", "authorization server issuer to trust; repeat for several (required)")
	fs.Var(&keys, "key",
		"public key of an issuer, as issuer=path or issuer#kid=path; repeat for several (required)")
	fs.DurationVar(&opts.shutdown, "shutdown-timeout", 30*time.Second,
		"how long a shutdown waits for requests in flight before it stops waiting")
	if err := fs.Parse(args); err != nil {
		return options{}, fmt.Errorf("%w\n\n%s", err, usage(fs))
	}
	if fs.NArg() > 0 {
		return options{}, fmt.Errorf("unexpected argument %q\n\n%s", fs.Arg(0), usage(fs))
	}

	opts.issuers = issuers
	for _, raw := range keys {
		parsed, err := parseKey(raw)
		if err != nil {
			return options{}, err
		}
		opts.keys = append(opts.keys, parsed)
	}
	// Reported together rather than one per run: an operator starting the
	// gateway for the first time should learn everything that is missing at
	// once, not one restart at a time.
	var missing []string
	for _, required := range []struct {
		flag  string
		empty bool
	}{
		{"-config", opts.config == ""},
		{"-resource", opts.resource == ""},
		{"-issuer", len(opts.issuers) == 0},
		{"-key", len(opts.keys) == 0},
	} {
		if required.empty {
			missing = append(missing, required.flag)
		}
	}
	if len(missing) > 0 {
		return options{}, fmt.Errorf("%s is required\n\n%s", strings.Join(missing, ", "), usage(fs))
	}
	return opts, nil
}

// parseKey splits "issuer=path" or "issuer#kid=path".
//
// The separator is the last "=", because an issuer is a URL and a path may
// hold one too; the kid is cut from the issuer side, where "#" cannot occur in
// an issuer identifier.
func parseKey(raw string) (keyFile, error) {
	at := strings.LastIndex(raw, "=")
	if at <= 0 || at == len(raw)-1 {
		return keyFile{}, fmt.Errorf("-key %q: want issuer=path or issuer#kid=path", raw)
	}
	issuer, path := raw[:at], raw[at+1:]
	kid := ""
	if hash := strings.Index(issuer, "#"); hash >= 0 {
		issuer, kid = issuer[:hash], issuer[hash+1:]
	}
	if issuer == "" {
		return keyFile{}, fmt.Errorf("-key %q: names no issuer", raw)
	}
	return keyFile{issuer: issuer, kid: kid, path: path}, nil
}

// repeated collects a flag given more than once.
type repeated []string

func (r *repeated) String() string { return strings.Join(*r, ", ") }

func (r *repeated) Set(v string) error {
	if v == "" {
		return errors.New("empty value")
	}
	*r = append(*r, v)
	return nil
}

func usage(fs *flag.FlagSet) string {
	var b strings.Builder
	b.WriteString("usage: mcp-reverse-proxy -config FILE -resource URI -issuer URI -key ISSUER=FILE [options]\n\n")
	fs.SetOutput(&b)
	fs.PrintDefaults()
	fs.SetOutput(io.Discard)
	return b.String()
}
