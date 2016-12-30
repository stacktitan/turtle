package turtle

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
)

// AuthMode constants.
const (
	AUTHMODEREQUIRED = "required"
	AUTMODETRY       = "try"
	AUTHMODENONE     = "none"
)

var validMode = map[string]bool{AUTHMODEREQUIRED: true, AUTHMODENONE: true, AUTMODETRY: true}

func isValidAuthMode(mode string) bool {
	_, ok := validMode[mode]
	return ok
}

// CtxCredentials is the key for the request context value
// holding credentials returned from Scheme.Authenticate.
type CtxCredentials struct{}

// ErrorWriter implements error handling for bundled routes.
// Each function should write to the ResponseWriter.
// No further writing to the ResponseWriter should occur.
type ErrorWriter interface {
	Unauthorized(w http.ResponseWriter, r *http.Request, err error)
	ServerError(w http.ResponseWriter, r *http.Request, err error)
	Forbidden(w http.ResponseWriter, r *http.Request, err error)
	BadRequest(w http.ResponseWriter, r *http.Request, err error)
}

// Roler is in interface used during authorization to
// validate that the implementer has the required role.
type Roler interface {
	HasRole(role string) bool
}

// Bundler bundles authentication, authorization, validation and per HandlerFunc logic into a nice package.
type Bundler struct {
	schemes       map[string]Scheme
	defaultScheme string
	ew            ErrorWriter
}

// NewBundler returns a new Bundler.
func NewBundler(ew ErrorWriter) *Bundler {
	return &Bundler{
		schemes: make(map[string]Scheme),
		ew:      ew,
	}
}

// RegisterScheme registers the scheme by name with bundler.
// It can then be used in O.Schemes.
func (b *Bundler) RegisterScheme(name string, scheme Scheme) {
	b.schemes[name] = scheme
}

// SetDefaultScheme sets the scheme name that will be used for every bundled HandlerFunc.
// Error will be returned if the scheme has not been registered.
func (b *Bundler) SetDefaultScheme(name string) error {
	if _, ok := b.schemes[name]; !ok {
		return errors.New("scheme not registered")
	}
	b.defaultScheme = name
	return nil
}

// O are options to pass to Bundle.
type O struct {
	Allow       []string     // Content-Types to allow.
	Roles       []string     // Roles to allow, object in request context with key CtxCredentials must implement Roler.
	Schemes     []string     // A series of authentication schemes to try in order. Must be a key in Bundler.SchemeMap.
	AuthMode    string       // 'try', 'required', 'none'.
	Before      []HandleWrap // A series of HandlerFuncs to execute before Handle.
	After       []HandleWrap // A serios of HandlerFuncs to execute after Handle.
	HandlerFunc func(http.ResponseWriter, *http.Request)
}

// HandleWrap is a function that takes a HandlerFunc and returns a HandlerFunc.
type HandleWrap func(func(http.ResponseWriter, *http.Request)) func(http.ResponseWriter, *http.Request)

// WrapSlice takes a variable amount of HandleWraps and returns a slice.
// This is a convienience function for setting O.Before.
func WrapSlice(funcs ...HandleWrap) []HandleWrap {
	chain := make([]HandleWrap, len(funcs))
	for i, f := range funcs {
		chain[i] = f
	}
	return chain
}

// New returns a bundled HandlerFunc. New may panic if options are incorrect that could result in a
// invalid or insecure configuration.
func (b *Bundler) New(options O) func(http.ResponseWriter, *http.Request) {
	// Panic here because we don't want our app to run with an invalid authmode.
	if !isValidAuthMode(options.AuthMode) {
		panic(fmt.Sprintf("invalid auth mode: %s", options.AuthMode))
	}
	if options.AuthMode != AUTHMODEREQUIRED && len(options.Roles) != 0 {
		panic(fmt.Sprintf("invalid authentication mode %s for amount of roles %d", options.AuthMode, len(options.Roles)))
	}
	for _, k := range options.Schemes {
		if _, ok := b.schemes[k]; !ok {
			panic(fmt.Sprintf("invalid scheme in RO.Schemes: %s", k))
		}
	}
	// Load the default scheme.
	if len(options.Schemes) < 1 && b.defaultScheme != "" {
		options.Schemes = append(options.Schemes, b.defaultScheme)
	}

	bindle := bundle{bundler: b, opts: options}

	// Prepend auth HandlerFunc chain.
	bindle.chain = append(bindle.chain, bindle.authenticate)
	bindle.chain = append(bindle.chain, bindle.authorize)
	bindle.chain = append(bindle.chain, bindle.allow)
	bindle.chain = append(bindle.chain, bindle.opts.Before...)

	// Turtles all the way down...
	for i := (len(bindle.chain) - 1); i >= 0; i-- {
		bindle.opts.HandlerFunc = bindle.chain[i](bindle.opts.HandlerFunc)
	}
	var after func(http.ResponseWriter, *http.Request)
	for i := (len(bindle.opts.After) - 1); i >= 0; i-- {
		after = bindle.opts.After[i](after)
	}

	return func(w http.ResponseWriter, r *http.Request) {
		bindle.opts.HandlerFunc(w, r)
		after(w, r)
	}
}

type bundle struct {
	bundler *Bundler
	opts    O
	chain   []HandleWrap
}

// authenticate attempts to authenticate a request for the configured schemes.
func (b *bundle) authenticate(next func(http.ResponseWriter, *http.Request)) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		log.Printf("Authentiate\n")
		if b.opts.AuthMode == AUTHMODENONE {
			next(w, r)
			return
		}

		for i, k := range b.opts.Schemes {
			scheme, ok := b.bundler.schemes[k]
			if !ok {
				b.bundler.ew.ServerError(w, r, errors.New("authentication scheme not registered"))
				return
			}
			user, err := scheme.Authenticate(w, r)
			if err != nil {
				if b.opts.AuthMode == AUTHMODEREQUIRED {
					// Last in the chain.
					if i == len(b.opts.Schemes)-1 {
						b.bundler.ew.Unauthorized(w, r, err)
						return
					}
				}
			} else {
				r = r.WithContext(context.WithValue(r.Context(), CtxCredentials{}, user))
				break
			}
		}
		next(w, r)
	}
}

// authorize ensures the user from CtxCredentials has a valid role for the bundle.
func (b *bundle) authorize(next func(http.ResponseWriter, *http.Request)) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		if len(b.opts.Roles) < 1 {
			next(w, r)
			return
		}
		roler, ok := r.Context().Value(CtxCredentials{}).(Roler)
		if !ok {
			b.bundler.ew.ServerError(w, r, errors.New("CtxCredentials does not implement Roler"))
			return
		}
		var isAllowed bool
		for _, r := range b.opts.Roles {
			if roler.HasRole(r) {
				isAllowed = true
				break
			}
		}
		if !isAllowed {
			b.bundler.ew.Forbidden(w, r, fmt.Errorf("missing required roles: %s", strings.Join(b.opts.Roles, " ")))
			return
		}
		next(w, r)
	}
}

// allow checks the content-type header of a request and ensures that it is allowed.
func (b *bundle) allow(next func(http.ResponseWriter, *http.Request)) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" && r.Method != "HEAD" && r.Method != "DELETE" {
			contentType := r.Header.Get("Conntent-Type")
			var found bool
			for _, allowed := range b.opts.Allow {
				if strings.Contains(contentType, allowed) {
					found = true
					break
				}
			}
			if !found {
				b.bundler.ew.BadRequest(w, r, fmt.Errorf("invalid request content-type: %s", contentType))
				return
			}
		}
		next(w, r)
	}
}